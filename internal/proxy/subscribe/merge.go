package subscribe

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"

	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
)

// maxConcurrentSubscriptionFetches 是单轮刷新允许同时读取的订阅响应数。
// 单个响应上限为 32 MiB；固定为 4 可以约束最坏情况下的并行响应缓冲内存，同时仍
// 保留多源并发能力。该限制只影响大量订阅的排队时长，不改变单源解析或降级语义。
const maxConcurrentSubscriptionFetches = 4

// Merge 合并多个订阅的节点：
//   - 按 Node.DedupKey() 去重，先出现的保留（订阅按名字排序后依次处理，保证结果稳定）；
//   - excludeRe 非 nil 时按节点名过滤掉匹配项；
//   - 保证 Name 全局唯一：冲突时追加 " (订阅名)"，仍冲突再追加序号；
//   - 同步设置 Mapping["name"] 和 Node.Subscription。
func Merge(subs map[string][]*node.Node, excludeRe *regexp.Regexp) []*node.Node {
	return MergeFiltered(subs, nil, excludeRe)
}

// MergeFiltered 合并多个订阅节点，并按 include/exclude 正则执行对称过滤。
//
// 参数：
//   - subs: map[string][]*node.Node，来源名到节点列表的映射。
//   - includeRe: *regexp.Regexp，非 nil 时只保留名称匹配的节点。
//   - excludeRe: *regexp.Regexp，非 nil 时剔除名称匹配的节点；exclude 优先级更高。
//
// 返回值：
//   - []*node.Node：按来源名稳定排序、按节点领域去重键去重并保证名称全局唯一的结果。
//     内容无需改动的节点原样返回（保持指针身份），需要改名或改写 Mapping 的节点以
//     私有副本返回。
//
// 错误情况：无；nil/无 Mapping 的节点会被忽略。相同 DedupKey 只评估稳定排序后
// 首次出现的节点名称，保持普通节点的历史去重语义，不因后续来源的别名改变过滤结果。
//
// 并发契约：本函数**绝不修改输入节点**。输入常常是运行态里被概览、配置生成等
// 并发读取的已发布对象（见 App.Nodes），原地改写 Name/Subscription/Mapping 会与那些
// 读取构成数据竞争，其中 Mapping 的并发写入还会直接触发运行时 fatal error 终止进程。
// 因此所有写入都先经 own() 取一份私有副本（含 Mapping 顶层），只有发生改动时才复制，
// 未改动的节点保持原指针，不产生额外开销。
func MergeFiltered(subs map[string][]*node.Node, includeRe, excludeRe *regexp.Regexp) []*node.Node {
	names := make([]string, 0, len(subs))
	for name := range subs {
		names = append(names, name)
	}
	sort.Strings(names)

	// owned 把输入节点与它已经派生的副本都映射到同一份私有副本：两遍改写共用，
	// 第一遍改过名的节点在第二遍改写引用时不会再复制一层。
	owned := map[*node.Node]*node.Node{}
	own := func(n *node.Node) *node.Node {
		if cloned := owned[n]; cloned != nil {
			return cloned
		}
		cloned := n.CloneWithMapping()
		owned[n] = cloned
		owned[cloned] = cloned
		return cloned
	}

	seenKey := map[string]*node.Node{}
	usedName := map[string]bool{}
	type acceptedNode struct {
		node *node.Node
	}
	accepted := make([]acceptedNode, 0)
	nameAliases := make(map[string]map[string]string, len(names))
	for _, subName := range names {
		for _, n := range subs[subName] {
			if n == nil || n.Mapping == nil {
				continue
			}
			// Tailscale 出站名称同时决定独立的 tsnet 状态目录和设备身份，因此必须使用
			// 领域去重键；若继续复用通用 Key，同一控制面下多个空 auth-key 的审批节点
			// 会被错误合并，后续策略组与注册触发器都无法在 mihomo 中找到被丢节点。
			key := n.DedupKey()
			if retained := seenKey[key]; retained != nil {
				// 去重节点仍可能被同一份订阅中的 dialer-proxy 以别名引用。记录别名
				// 到实际保留节点的最终名称，避免依赖节点虽然等价却被误判为不存在。
				if nameAliases[subName] == nil {
					nameAliases[subName] = map[string]string{}
				}
				nameAliases[subName][n.Name] = retained.Name
				continue
			}
			seenKey[key] = n
			if includeRe != nil && !includeRe.MatchString(n.Name) {
				continue
			}
			if excludeRe != nil && excludeRe.MatchString(n.Name) {
				continue
			}
			originalName := n.Name
			// uniqueName 必须无条件调用：即使不需要改名，它也要把最终名称登记为已占用。
			finalName := uniqueName(originalName, subName, usedName)
			if n.Subscription != subName || n.Name != finalName || mappingName(n.Mapping) != finalName {
				n = own(n)
				n.Subscription = subName
				n.Name = finalName
				n.Mapping["name"] = finalName
			}
			// 别名表必须指向改名后的对象：去重分支会用 retained.Name 作最终名称，
			// 而改动过的节点是副本，旧的指针会残留改名前的名字。
			seenKey[key] = n
			if nameAliases[subName] == nil {
				nameAliases[subName] = map[string]string{}
			}
			// 同一订阅内若出现重名节点，mihomo 原始引用本身就存在歧义；保留首个映射
			// 与订阅原始顺序一致，避免后出现的节点悄悄改变已有链路目标。
			if _, exists := nameAliases[subName][originalName]; !exists {
				nameAliases[subName][originalName] = n.Name
			}
			accepted = append(accepted, acceptedNode{node: n})
		}
	}

	// 名称全局去重会改变节点的 mihomo name，而 dialer-proxy 保存的是名称引用。
	// 必须等所有最终名称确定后再做第二遍重写，否则被引用节点排在后面时无法解析。
	// 引用优先在同一订阅内解析，防止不同订阅的同名节点被错误串成跨订阅链路。
	out := make([]*node.Node, 0, len(accepted))
	for _, item := range accepted {
		n := item.node
		if raw, ok := n.Mapping["dialer-proxy"].(string); ok {
			if resolved, exists := nameAliases[n.Subscription][raw]; exists && resolved != raw {
				n = own(n)
				n.Mapping["dialer-proxy"] = resolved
			}
		}
		out = append(out, n)
	}
	return out
}

// mappingName 读取 Mapping 顶层的协议名称，缺失或类型异常时返回空字符串。
//
// 参数：
//   - mapping: map[string]any，mihomo 出站映射。
//
// 返回值：string，`name` 键的字符串值；不存在或非字符串时为空。
//
// 错误情况：无。用于判断合并结果是否真的需要改写 Mapping，从而避免无谓的复制。
func mappingName(mapping map[string]any) string {
	name, _ := mapping["name"].(string)
	return name
}

// uniqueName 保证节点名全局唯一：冲突时追加 " (订阅名)"，仍冲突再追加序号。
func uniqueName(name, subName string, used map[string]bool) string {
	if !used[name] {
		used[name] = true
		return name
	}
	cand := fmt.Sprintf("%s (%s)", name, subName)
	if !used[cand] {
		used[cand] = true
		return cand
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s %d", cand, i)
		if !used[c] {
			used[c] = true
			return c
		}
	}
}

// FetchAll 并发拉取所有订阅并合并节点。
// 单个订阅失败不影响其他订阅；返回与 subs 一一对应的错误切片（nil 表示成功，
// *FetchWarning 表示拉取失败但降级用了缓存）。
// static 是可选的静态节点集（如手动节点，键为来源名），与订阅节点一起参与去重合并。
func FetchAll(ctx context.Context, subs []config.Subscription, stateDir string, excludeRe *regexp.Regexp, static ...map[string][]*node.Node) ([]*node.Node, []error) {
	nodes, errs := FetchAllFiltered(ctx, subs, stateDir, nil, excludeRe, static...)
	return nodes, errs
}

// FetchAllFiltered 并发拉取所有订阅，并应用 include/exclude 双向过滤。
//
// 参数：
//   - ctx: context.Context，用于取消所有订阅请求。
//   - subs: []config.Subscription，待拉取的订阅。
//   - stateDir: string，订阅缓存目录根路径。
//   - includeRe: *regexp.Regexp，非 nil 时只保留匹配节点。
//   - excludeRe: *regexp.Regexp，非 nil 时剔除匹配节点。
//   - static: ...map[string][]*node.Node，手动节点等静态来源。
//
// 返回值：
//   - []*node.Node：过滤、去重、统一命名后的节点。
//   - []error：与 subs 一一对应的拉取错误或缓存降级警告。
//
// 错误情况：单个订阅失败不会阻断其它来源，详细错误写入返回切片。
func FetchAllFiltered(ctx context.Context, subs []config.Subscription, stateDir string, includeRe, excludeRe *regexp.Regexp, static ...map[string][]*node.Node) ([]*node.Node, []error) {
	nodes, _, errs := FetchAllWithInfoAndFilters(ctx, subs, stateDir, includeRe, excludeRe, static...)
	return nodes, errs
}

// FetchAllWithInfo 并发拉取所有订阅并合并节点，同时返回各订阅的用量信息。
//
// 参数：
//   - ctx: context.Context，用于控制所有订阅请求。
//   - subs: []config.Subscription，待拉取的订阅列表。
//   - stateDir: string，缓存目录根路径。
//   - excludeRe: *regexp.Regexp，节点名过滤规则；nil 表示不过滤。
//   - static: ...map[string][]*node.Node，手动节点等非 HTTP 来源。
//
// 返回值：
//   - []*node.Node: 合并去重后的节点列表。
//   - map[string]UserInfo: 订阅名到用量信息的映射，仅包含有有效信息的订阅。
//   - []error: 与 subs 一一对应的错误列表。
//
// 错误情况：
//   - 单个订阅失败不会中断其它订阅；错误记录在返回的 errs 对应槽位。
//   - 静态节点不产生 UserInfo，因为它没有订阅响应头来源。
func FetchAllWithInfo(ctx context.Context, subs []config.Subscription, stateDir string, excludeRe *regexp.Regexp, static ...map[string][]*node.Node) ([]*node.Node, map[string]UserInfo, []error) {
	return FetchAllWithInfoAndFilters(ctx, subs, stateDir, nil, excludeRe, static...)
}

// FetchAllWithInfoAndFilters 并发拉取订阅，同时返回用量信息并应用双向名称过滤。
//
// 参数：
//   - ctx: context.Context，用于控制所有订阅请求。
//   - subs: []config.Subscription，待拉取的订阅列表。
//   - stateDir: string，缓存目录根路径。
//   - includeRe: *regexp.Regexp，只保留匹配节点；nil 表示不限制。
//   - excludeRe: *regexp.Regexp，剔除匹配节点；nil 表示不排除，且其优先级高于 include。
//   - static: ...map[string][]*node.Node，手动节点等非 HTTP 来源。
//
// 返回值：
//   - []*node.Node：合并、过滤、去重后的节点列表。
//   - map[string]UserInfo：订阅名到有效用量信息的映射。
//   - []error：与 subs 一一对应的错误或缓存降级警告。
//
// 错误情况：单源失败仅写入对应错误槽位；静态来源不会产生 UserInfo。
func FetchAllWithInfoAndFilters(ctx context.Context, subs []config.Subscription, stateDir string, includeRe, excludeRe *regexp.Regexp, static ...map[string][]*node.Node) ([]*node.Node, map[string]UserInfo, []error) {
	return FetchAllWithInfoAndFiltersOptions(ctx, subs, stateDir, includeRe, excludeRe, FetchOptions{}, static...)
}

// FetchAllWithInfoAndFiltersOptions 并发拉取订阅，并为每个网络来源应用相同的降级策略。
//
// 参数说明：
//   - ctx: context.Context，用于取消全部 worker、HTTP 请求与重试退避。
//   - subs: []config.Subscription，保持配置顺序的订阅列表。
//   - stateDir: string，订阅正文和用量缓存目录。
//   - includeRe/excludeRe: *regexp.Regexp，节点名称包含/排除过滤器。
//   - options: FetchOptions，主链路失败时使用的本机主端口代理配置。
//   - static: 手动节点等非 HTTP 来源，按来源名参与最终合并。
//
// 返回值说明：合并节点、有效用量映射和与 subs 一一对应的错误槽位。
//
// 错误情况：单个来源的所有网络路径失败时只写对应槽位；有缓存时返回
// *FetchWarning 并继续合并缓存节点，不会取消其它并发来源。
func FetchAllWithInfoAndFiltersOptions(ctx context.Context, subs []config.Subscription, stateDir string, includeRe, excludeRe *regexp.Regexp, options FetchOptions, static ...map[string][]*node.Node) ([]*node.Node, map[string]UserInfo, []error) {
	nodesBySub := make(map[string][]*node.Node, len(subs)+len(static))
	for _, m := range static {
		for src, nodes := range m {
			nodesBySub[src] = append(nodesBySub[src], nodes...)
		}
	}
	infos := make(map[string]UserInfo, len(subs))
	errs := make([]error, len(subs))
	nodesByIndex := make([][]*node.Node, len(subs))
	infosByIndex := make([]UserInfo, len(subs))
	enabledCount := 0
	for _, sub := range subs {
		if sub.IsEnabled() {
			enabledCount++
		}
	}

	// worker 只写各自任务索引对应的切片槽位，不共享写 map，因此无需在网络请求结束
	// 时竞争互斥锁。所有 worker 完成后再按原订阅顺序汇总，既保留错误槽位语义，也让
	// goroutine 与潜在的大响应体数量不再随订阅总数增长。
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(maxConcurrentSubscriptionFetches, enabledCount) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				sub := subs[i]
				nodes, info, err := FetchWithInfoOptions(ctx, sub, stateDir, options)
				if err != nil {
					var warning *FetchWarning
					errs[i] = err
					if !errors.As(err, &warning) {
						// 无缓存的硬失败不应写入空来源；其它任务继续执行，保持单源隔离。
						continue
					}
				}
				nodesByIndex[i] = nodes
				infosByIndex[i] = info
			}
		}()
	}
	for i, sub := range subs {
		// 禁用订阅仍保留在配置与缓存中，但不应产生网络请求或进入节点合并。
		// errs 保持与原 subs 等长，对应槽位留 nil，让调用方能区分“主动禁用”和“拉取失败”。
		if sub.IsEnabled() {
			jobs <- i
		}
	}
	close(jobs)
	wg.Wait()
	for i, sub := range subs {
		if nodesByIndex[i] != nil {
			nodesBySub[sub.Name] = nodesByIndex[i]
		}
		if !infosByIndex[i].IsZero() {
			infos[sub.Name] = infosByIndex[i]
		}
	}
	return MergeFiltered(nodesBySub, includeRe, excludeRe), infos, errs
}
