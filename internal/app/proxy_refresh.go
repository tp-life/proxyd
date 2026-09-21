package app

// 代理域：订阅刷新流水线（拉取/健康检测/端口分配/热更新）与启动快照恢复。

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/proxy/core"
	"proxyd/internal/proxy/groupstate"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/pool"
	"proxyd/internal/proxy/ruleurl"
	"proxyd/internal/proxy/subscribe"
)

// configApplied 报告生成的配置字节是否与当前已热更新生效的一致。
//
// 参数：
//   - candidate: []byte，本次生成（或自检降级后）的配置字节。
//
// 返回值：bool，逐字节等于最近一次成功生效的配置时返回 true。
//
// 错误情况：无；从未成功应用过配置时返回 false，保证启动与恢复流程不会被跳过。
func (a *App) configApplied(candidate []byte) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.appliedConfig) > 0 && bytes.Equal(a.appliedConfig, candidate)
}

// markConfigApplied 记录一份已经成功热更新到 mihomo 的配置字节。
//
// 参数：
//   - applied: []byte，自检通过并成功 Reload 的最终配置字节。
//
// 返回值：无。
//
// 错误情况：无；调用方必须确保 mihomo 已经生效，否则后续刷新会错误地跳过重试。
func (a *App) markConfigApplied(applied []byte) {
	a.mu.Lock()
	a.appliedConfig = applied
	a.mu.Unlock()
}

// clearAppliedConfig 丢弃已生效配置记录，使下一次生成必定重新自检并热更新。
//
// 参数：无。
//
// 返回值：无。
//
// 错误情况：无；mihomo 被停用、被替换或热更新可能只应用了一半时都必须调用。
func (a *App) clearAppliedConfig() {
	a.mu.Lock()
	a.appliedConfig = nil
	a.mu.Unlock()
}

// Regenerate 仅按当前 cfg + 当前 assignments 重新生成并热应用 mihomo 配置
// （不拉订阅、不测速）。用于 auto-port/rules/groups 等变更后的热更新。
func (a *App) Regenerate() error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	return a.regenerateCurrentLocked()
}

// regenerateCurrentLocked 用当前 assigns 重新生成；调用方须已持有 refreshing 锁。
func (a *App) regenerateCurrentLocked() error {
	a.mu.RLock()
	assigns := make([]pool.Assignment, len(a.assigns))
	copy(assigns, a.assigns)
	a.mu.RUnlock()
	return a.regenerateLocked(assigns)
}

// regenerateLocked 是重新生成热更新的核心；调用方须已持有 refreshing 锁。
func (a *App) regenerateLocked(assigns []pool.Assignment) error {
	a.mu.RLock()
	cfg := a.cfg
	imported := a.mergedImportedLocked()
	a.mu.RUnlock()
	return a.regenerateWithLocked(cfg, assigns, imported)
}

// regenerateWithLocked 用指定配置生成并热应用；调用方须已持有 refreshing 锁。
// cfg 为运行时副本（不与 a.cfg 同步修改），供启动快照恢复等场景使用。
func (a *App) regenerateWithLocked(cfg *config.Config, assigns []pool.Assignment, imported []string) error {
	return a.applyConfigLocked(cfg, assigns, imported)
}

// applyConfigLocked 生成并热应用一版 mihomo 配置。
//
// 参数：
//   - cfg: *config.Config，本次需要应用的运行配置副本。
//   - assigns: []pool.Assignment，需要创建本地 listener 的节点端口映射。
//   - imported: []string，已经合并清洗的远程规则。
//
// 返回值：error，生成、自检、mihomo 热加载或 TUN 实际状态不一致时返回。
//
// 错误情况：调用方必须持有 refreshing 锁，确保读取节点快照和 Reload 期间没有另一轮
// 刷新并发修改。生成时同时传入完整健康节点集，使 dialer-proxy 依赖和策略组成员即使
// 没有独立本地端口，也会作为 proxy-only 出站注册到 mihomo。
func (a *App) applyConfigLocked(cfg *config.Config, assigns []pool.Assignment, imported []string) (resultErr error) {
	generation := a.proxyLifecycle.Begin()
	// 热更新和启动共用状态收尾；外围刷新另行记录拉取失败，不依赖仅在 HTTP 操作时更新。
	defer func() {
		running := a.runner.Running()
		phase, message := "idle", ""
		if running {
			phase = "running"
		}
		if cfg.ProxyDisabled && !running {
			phase = "disabled"
		}
		if resultErr != nil {
			phase, message = "failed", "代理配置应用失败，请检查监听端口、TUN 权限和运行日志"
			if running {
				phase = "degraded"
			}
		}
		a.proxyLifecycle.Complete(generation, phase, running, message, 0)
	}()
	// 所有配置热更新共用此门，停用期间修改规则或端口也不能意外恢复监听。
	if cfg.ProxyDisabled {
		// Suspend 会关闭 TUN listener，helper 注入的 fd 随 mihomo 一并关闭，
		// 这里只清状态，恢复代理时重新申请。
		a.releaseTUNFDLocked(false)
		// 核心被停用后运行态不再对应任何配置；不清空记录会让恢复代理时误判为
		// “配置没变”而跳过热更新，留下已停用的空核心。
		a.clearAppliedConfig()
		return a.runner.Suspend()
	}
	// darwin 普通用户模式下经 tun-helper 申请 utun fd 并注入生成配置；
	// fd 不落盘（injectTUNFD 作用于运行时副本）。
	fd, acquired, err := a.ensureTUNFDLocked(cfg)
	if err != nil {
		return err
	}
	cfg = injectTUNFD(cfg, fd)
	releaseOnError := func() {
		// fd 未随配置成功应用时所有权仍在本层，回滚必须关闭，避免泄漏 utun。
		if acquired {
			a.releaseTUNFDLocked(true)
		}
	}
	// select 分组的持久化选中项随每次生成注入 mihomo default-selected；
	// 状态文件损坏仅打日志丢弃，mihomo 回退到组成员首位。
	selected, err := groupstate.Load(a.groupSelectedPath())
	if err != nil {
		log.Printf("[groupstate] %v (ignored)", err)
	}
	// 稳态快路径：后台健康检查每轮都会走到这里，但节点可用性没有变化时生成结果与
	// 当前运行态逐字节一致。此时跳过 mihomo 自检与整棵 tunnel 的热更新：自检会加载
	// geo 数据与规则集，热更新会重建全部出站与 listener，二者是这一轮唯一的可观开销，
	// 也是进程 RSS 高水位的主要来源。
	// 字节比较是可靠的：配置由固定结构的 map 序列化，yaml.v3 对 map 键排序，相同输入
	// 必然得到相同输出（见 core.BuildWithState 的文档契约）。
	built, err := core.BuildWithState(cfg, assigns, a.Nodes(), imported, selected)
	if err != nil {
		releaseOnError()
		return fmt.Errorf("generate mihomo config: %w", err)
	}
	if a.configApplied(built.YAML()) {
		releaseOnError()
		return nil
	}
	cfgYAML, err := built.Validate()
	if err != nil {
		releaseOnError()
		return fmt.Errorf("generate mihomo config: %w", err)
	}
	// GEO 数据不可用时会降级成剔除 GEO 规则的配置，其字节与 built 不同；再比一次，
	// 避免每个健康检查周期都把同一份降级配置重复热更新。降级期间仍会走一遍自检，
	// 这样 geo 数据恢复可用后下一轮就能自动回到含 GEO 规则的配置。
	if a.configApplied(cfgYAML) {
		releaseOnError()
		return nil
	}
	if err := a.runner.Reload(cfgYAML); err != nil {
		// 热更新失败时 mihomo 可能只应用了一半，运行态不再对应该配置；清空记录让下一轮
		// 重新自检并重试，而不是把失败状态误当成“已生效、无需再试”。
		a.clearAppliedConfig()
		releaseOnError()
		return fmt.Errorf("apply mihomo config: %w", err)
	}
	// mihomo 的 ReCreateTun 在创建虚拟网卡失败时只写日志，不把错误返回给 hub.Parse。
	// 因此必须在 Reload 返回后读取 listener 实际状态，否则可能把 enable:true 持久化，
	// 但运行时 TUN 已关闭。状态不一致作为应用失败返回，上层会恢复旧配置。
	if active := a.runner.TUNEnabled(); active != cfg.TUN.Enable {
		a.clearAppliedConfig()
		releaseOnError()
		return fmt.Errorf("mihomo TUN 实际状态与请求不一致（期望 enable=%t，实际 active=%t）；请检查 TUN 日志、stack 与系统权限", cfg.TUN.Enable, active)
	}
	a.markConfigApplied(cfgYAML)
	if !cfg.TUN.Enable {
		// TUN 已随本次应用关闭，fd 由 mihomo 关闭（所有权已移交），只清持有状态。
		a.releaseTUNFDLocked(false)
	}
	return nil
}

// Refresh 执行一轮代理节点流水线：fetch=true 时显式拉取订阅与规则源；fetch=false
// 时只使用内存节点、已确认的本地订阅缓存和当前配置里的手动节点，绝不访问订阅 URL。
// 步骤：按请求拉取或重建节点集合 → 健康检测 → 稳定端口分配 → 生成配置 → 热更新核心。
//
// 参数：
//   - ctx: context.Context，控制订阅下载、规则源下载、健康检测和链路验证的取消。
//   - fetch: bool，只有用户明确执行同步时才传 true；后台健康检查必须传 false。
//
// 返回值：error，节点来源为空、全部节点失效、下载失败且无缓存，或 mihomo 热更新失败时返回。
//
// 错误情况：fetch=false 即使没有内存节点也不会隐式下载订阅，这是“订阅只允许手动同步”
// 的关键边界；新安装尚未同步时会返回无节点错误，由调用方保留当前 DIRECT 运行配置。
func (a *App) Refresh(ctx context.Context, fetch bool) (resultErr error) {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	return a.refreshLocked(ctx, fetch)
}

// refreshLocked 执行完整节点刷新流水线，但不自行获取 refreshing 锁。
//
// 参数说明：
//   - ctx: context.Context，控制订阅下载、规则源下载、健康检测与运行态应用。
//   - fetch: bool，true 时显式下载订阅；false 时只使用现有内存与本地缓存。
//
// 返回值说明：error，节点来源为空、检测全部失败或 mihomo 热更新失败时返回。
//
// 错误情况：调用方必须已经持有 refreshing 锁。该拆分让 Tailscale 一体化接入能把
// “修改配置 → 刷新节点池 → 持久化”纳入同一事务，避免在锁间隙被其它刷新覆盖。
func (a *App) refreshLocked(ctx context.Context, fetch bool) (resultErr error) {
	a.mu.RLock()
	disabled := a.cfg.ProxyDisabled
	a.mu.RUnlock()
	// 模块关闭时暂停手动同步与周期健康探测，避免已停用代理继续访问外网。
	if disabled {
		return nil
	}

	a.proxyLifecycle.Begin()
	defer func() {
		// 收尾覆盖最终状态：期间 applyConfigLocked 可能已 Begin/Complete 过，
		// 这里以当前世代直接提交最终结果，不再新增一次尝试计数。
		phase, message := "running", ""
		running := a.runner.Running()
		if !running {
			phase = "idle"
		}
		if resultErr != nil {
			phase = "failed"
			message = "代理刷新失败，请检查节点健康状态与运行日志"
			if running {
				phase = "degraded"
			}
		}
		a.proxyLifecycle.CompleteCurrent(phase, running, message, 0)
	}()
	var nodes []*node.Node
	if fetch {
		fetchOptions := a.subscriptionFetchOptions()
		a.mu.RLock()
		subs := make([]config.Subscription, len(a.cfg.Subscriptions))
		copy(subs, a.cfg.Subscriptions)
		ruleURLs := make([]config.RuleURL, len(a.cfg.RuleURLs))
		copy(ruleURLs, a.cfg.RuleURLs)
		manualEntries := make([]any, len(a.cfg.ManualNodes))
		copy(manualEntries, a.cfg.ManualNodes)
		stateDir := a.cfg.StateDir
		a.mu.RUnlock()

		// 订阅与规则源并发拉取
		ruleCh := make(chan []ruleurl.Result, 1)
		go func() { ruleCh <- ruleurl.FetchAll(ctx, ruleURLs, stateDir) }()

		manual, manualErrs := subscribe.ParseManualNodes(manualEntries)
		for _, err := range manualErrs {
			if err != nil {
				log.Printf("[manual] %v", err)
			}
		}

		var errs []error
		var infos map[string]subscribe.UserInfo
		nodes, infos, errs = subscribe.FetchAllWithInfoAndFiltersOptions(ctx, subs, stateDir, a.includeRe, a.excludeRe, fetchOptions,
			map[string][]*node.Node{subscribe.ManualSubscription: manual})
		for _, err := range errs {
			if err != nil {
				log.Printf("[subscribe] %v", err)
			}
		}
		a.applyRuleResults(<-ruleCh)
		if len(nodes) == 0 {
			return fmt.Errorf("no nodes available from any subscription")
		}
		// 重新解析出来的节点先继承同一身份上一轮的展示值，再发布：控制台在检测期间
		// 保持稳定状态，不会整列闪成「失效/—」再逐个回填。
		a.inheritDisplay(nodes)
		a.mu.Lock()
		a.nodes = nodes
		a.subInfos = infos
		a.mu.Unlock()
	} else {
		// 非下载路径仍需重新解析手动节点，因为新增/删除手动节点后 API 会复用这条
		// 流水线更新运行态。订阅节点只能来自已提交的内存快照，绝不能因为内存为空
		// 而回退到网络；这样启动、定时健康检查和模块恢复均不会产生订阅请求。
		a.mu.RLock()
		manualEntries := append([]any(nil), a.cfg.ManualNodes...)
		subscriptions := append([]config.Subscription(nil), a.cfg.Subscriptions...)
		stateDir := a.cfg.StateDir
		currentInfos := cloneSubscriptionInfos(a.subInfos)
		a.mu.RUnlock()
		manual, manualErrs := subscribe.ParseManualNodes(manualEntries)
		for _, err := range manualErrs {
			if err != nil {
				log.Printf("[manual] %v", err)
			}
		}
		bySource := map[string][]*node.Node{subscribe.ManualSubscription: manual}
		for _, existing := range filterEnabledSubscriptionNodes(a.Nodes(), subscriptions) {
			if existing.Subscription == subscribe.ManualSubscription {
				continue
			}
			bySource[existing.Subscription] = append(bySource[existing.Subscription], existing)
		}
		infos := make(map[string]subscribe.UserInfo, len(subscriptions))
		for _, subscription := range subscriptions {
			if !subscription.IsEnabled() {
				continue
			}
			if info, ok := currentInfos[subscription.Name]; ok {
				infos[subscription.Name] = info
			} else if info, err := subscribe.ReadCachedUserInfo(stateDir, subscription.Name); err == nil && !info.IsZero() {
				// 内存中没有该订阅的用量（典型场景：进程重启后节点来自快照恢复，
				// 尚未经过任何订阅拉取），从用量 sidecar 补回，否则控制台的流量
				// 与到期展示会一直缺失，直到用户下一次手动同步。
				infos[subscription.Name] = info
			}
			if len(bySource[subscription.Name]) > 0 {
				continue
			}
			// 合并去重可能让某个订阅暂时没有内存节点；删除其它订阅或重新启用时，
			// 只从该订阅最后一次已确认缓存恢复，绝不为了补齐来源访问网络。
			cached, info, err := subscribe.LoadCachedWithInfo(subscription, stateDir)
			if err != nil {
				continue
			}
			bySource[subscription.Name] = cached
			if !info.IsZero() {
				infos[subscription.Name] = info
			}
		}
		nodes = subscribe.MergeFiltered(bySource, a.includeRe, a.excludeRe)
		if len(nodes) == 0 {
			return fmt.Errorf("no cached or manual nodes available; sync a subscription manually first")
		}
		// 手动节点每轮都重新解析，先继承同一身份的上一轮展示值；再提前发布（与下载路径
		// 一致），这样检测期间概览看到的就是本轮节点对象，能逐节点标记「测速中」并换上新延迟。
		a.inheritDisplay(nodes)
		a.mu.Lock()
		a.nodes = nodes
		a.subInfos = infos
		a.mu.Unlock()
	}

	a.checkNodes(ctx, nodes, pool.CheckOptions{}, a.dialerTargets()...)
	return a.applyNodes(ctx, nodes)
}

// Testing 报告当前是否正在进行节点健康检测（测速）。
//
// 参数：无。
//
// 返回值：bool，任一轮 pool.Check 执行期间为 true。
//
// 错误情况：无；只读原子计数，供概览暴露全局「测速中」状态（控制台据此决定是否加密
// 轮询）。逐节点的延迟列以节点自己的展示行为准。
func (a *App) Testing() bool {
	return a.testRounds.Load() > 0
}

// checkNodes 包裹 pool.Check，检测期间维护全局「测速中」轮数。
//
// 参数：
//   - ctx: context.Context，控制整轮检测的取消与超时。
//   - nodes: []*node.Node，待检测节点，结果原地写回。
//   - opts: pool.CheckOptions，逐节点结果回调等可选行为。
//   - dialerTargets: ...string，可作为链式目标的策略组名称。
//
// 返回值：无；单节点失败通过节点状态表达。
//
// 错误情况：检测串行化由调用方的 refreshing 锁保证（按订阅的测速之间可以并行，
// 因此用计数而不是布尔标记）；即使 panic 也经 defer 复位，不会让「测速中」状态残留。
func (a *App) checkNodes(ctx context.Context, nodes []*node.Node, opts pool.CheckOptions, dialerTargets ...string) {
	a.testRounds.Add(1)
	defer a.testRounds.Add(-1)
	a.mu.RLock()
	healthURL := a.cfg.HealthURL
	healthTimeout := a.cfg.HealthTimeout.D()
	stateDir := a.cfg.StateDir
	a.mu.RUnlock()
	pool.CheckWithOptions(ctx, nodes, healthURL, healthTimeout, 32, stateDir, opts, dialerTargets...)
}

// inheritDisplay 让新解析出来的节点继承同一身份（DedupKey）上一轮节点的展示行。
//
// 参数：nodes 为即将发布的本轮节点集合（尚未进入运行态）。
//
// 返回值：无。
//
// 错误情况：无。只按领域身份匹配：服务器或凭据变化意味着这是另一个出口，必须从零开始
// 测速，不能继承旧结果；没有 auth-key 的多个 Tailscale 审批节点共用同一个 Key，也只有
// DedupKey 能区分它们。用于避免「重新解析 → 检测结束」这段窗口里控制台整列闪成
// 「失效 / —」——逐节点展示后这段中间态是可见的，必须保持上一轮的稳定值。
func (a *App) inheritDisplay(nodes []*node.Node) {
	previous := a.nodesByDedupKey()
	for _, n := range nodes {
		if n == nil {
			continue
		}
		existing := previous[n.DedupKey()]
		if existing == nil || existing == n {
			continue
		}
		n.PublishDisplay(existing.Display())
	}
}

// markTesting 把节点标记为「测速中」，并固化它们当前的展示值。
//
// 参数：nodes 为待标记节点，nil 元素被忽略。
//
// 返回值：无。
//
// 错误情况：无。用于在测速开始前向概览预告进度：克隆节点上测速时，已发布节点自己
// 不会收到 pool 的标记，需要调用方显式标记，并在收尾时用 settleDisplay 复位。
func markTesting(nodes []*node.Node) {
	for _, n := range nodes {
		n.MarkTesting()
	}
}

// settleDisplay 按节点当前的权威字段发布最终展示行。
//
// 参数：nodes 为待收尾节点，nil 元素被忽略。
//
// 返回值：无。
//
// 错误情况：无；可重复调用。负责兜住所有提前返回路径（整轮取消、全部节点失效、
// 提交前发现订阅已变更等），避免节点永久停留在「测速中」。
func settleDisplay(nodes []*node.Node) {
	for _, n := range nodes {
		n.PublishResult()
	}
}

// applyNodes 执行健康检测后的流水线尾部，并完成 mihomo 托管节点的二阶段验证。
//
// 参数：
//   - ctx: context.Context，控制完整链路 URLTest 的取消与超时传播。
//   - nodes: []*node.Node，本轮订阅合并后的节点集合；健康状态会原地更新并保存快照。
//
// 返回值：error，没有任何可用节点、端口分配后的配置无法加载，或完整链路全部失败时返回。
//
// 错误情况：调用方必须持有 a.refreshing 锁。普通节点已经由 pool.Check 直接测速；
// dialer-proxy 节点先以依赖候选身份加载，Tailscale 则避免在核心外启动临时 tsnet；
// 配置了 Exit Node 的 Tailscale 与普通链式节点随后通过 Runner.URLTest 验证真实链路。
// 若候选失败，会重新分配端口并热加载一次，确保失败链路不会残留在最终监听入口。
func (a *App) applyNodes(ctx context.Context, nodes []*node.Node) error {
	// 无论本轮如何收场，都把展示行复位成最终结果：链路探测阶段的节点已经逐节点发布，
	// 而「全部节点失效」「端口重新分配失败」等提前返回也必须让控制台不再显示「测速中…」。
	defer settleDisplay(nodes)

	// 先更新应用节点快照，让 GenerateWithNodes 能看到未分配端口的链路依赖。
	// 刷新失败时仍保留本轮状态和失败原因，便于 Web/CLI 解释问题，而不是展示旧假象。
	a.mu.Lock()
	a.nodes = nodes
	a.mu.Unlock()

	alive := make([]*node.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Alive {
			alive = append(alive, n)
		}
	}
	if len(alive) == 0 {
		return fmt.Errorf("all %d nodes failed health check", len(nodes))
	}
	// 隧道类节点不参与端口映射，容量提示只统计实际需要一对一端口的节点。
	mappable := 0
	for _, n := range alive {
		if !n.IsTunnel() {
			mappable++
		}
	}
	a.mu.RLock()
	capacity := a.cfg.Capacity()
	portLo, portHi := a.cfg.PortRange[0], a.cfg.PortRange[1]
	a.mu.RUnlock()
	if mappable > capacity {
		log.Printf("[alloc] %d alive nodes exceed port capacity %d, keeping the fastest", mappable, capacity)
	}

	prev, err := pool.LoadSnapshot(a.snapshotPath())
	if err != nil {
		log.Printf("[alloc] load snapshot: %v (ignored)", err)
	}
	assigns := pool.Allocate(alive, portLo, portHi, prev)
	if err := a.regenerateLocked(assigns); err != nil {
		return err
	}

	// pool.Check 无法在首次配置加载前解析 dialer-proxy 的运行时依赖，也不得为
	// Tailscale 单独启动第二个 tsnet。此处使用刚生效的 mihomo 代理表执行真实
	// URLTest；只在可用性变化时重生成，延迟变化不触发无意义的 listener 重建。
	if a.verifyMihomoManagedNodes(ctx, nodes) {
		alive = alive[:0]
		for _, n := range nodes {
			if n.Alive {
				alive = append(alive, n)
			}
		}
		if len(alive) == 0 {
			// 候选配置已经短暂加载，不能直接返回并把失败链路 listener 留在运行态。
			// 生成空 assignment 配置会释放节点端口，同时保留主端口的 DIRECT 回退；
			// 清理失败时把两层错误一并返回，避免调用方误以为运行态已经安全收敛。
			if cleanupErr := a.regenerateLocked(nil); cleanupErr != nil {
				return fmt.Errorf("all %d dialer-proxy candidates failed end-to-end health check; cleanup failed: %w", len(nodes), cleanupErr)
			}
			a.mu.Lock()
			a.assigns = nil
			a.mu.Unlock()
			return fmt.Errorf("all %d dialer-proxy candidates failed end-to-end health check", len(nodes))
		}
		assigns = pool.Allocate(alive, portLo, portHi, prev)
		if err := a.regenerateLocked(assigns); err != nil {
			return err
		}
	}

	snap := &pool.Snapshot{Mapping: make(map[string]int, len(assigns))}
	for _, as := range assigns {
		snap.Mapping[as.Node.Key()] = as.Port
	}
	if err := pool.SaveSnapshot(a.snapshotPath(), snap); err != nil {
		log.Printf("[alloc] save snapshot: %v", err)
	}

	a.mu.Lock()
	a.assigns = assigns
	a.mu.Unlock()
	if err := node.SaveSnapshot(a.nodesSnapshotPath(), nodes); err != nil {
		log.Printf("[snapshot] 保存节点快照失败: %v", err)
	}
	// 顺带调和网关执行层：mihomo 入口刚热更新完毕，失败的 gateway 应用到点重试，
	// 被外部清除的规则（如 helper 看门狗）也在此收敛。
	a.reconcileGatewayLocked()
	a.ensureInteractiveTailscaleEnrollments(nodes)
	// reloads 是进程累计的成功热更新次数：稳态下它应当每轮保持不变，只有节点可用性
	// 或配置真的变化时才增长。排查周期性重建与 RSS 高水位时先看这个数字。
	log.Printf("[refresh] done: %d nodes, %d alive, %d ports mapped, %d reloads", len(nodes), len(alive), len(assigns), a.runner.Reloads())
	return nil
}

// ensureInteractiveTailscaleEnrollments 为没有 auth-key 的 mihomo Tailscale 出站恢复
// 管理员审批状态观察，并在进程重启后主动触发一次 tsnet 登录。
//
// 参数说明：nodes 是刚成功加载到 mihomo 代理表的完整节点集合。
//
// 返回值说明：无；触发在有界后台协程执行，不阻塞刷新事务。
//
// 错误情况：已经存在注册状态的节点不会重复触发；网络或审批等待错误由 Runner
// 状态记录。这样待审批配置即使重启，也能重新在管理面获得同一身份的注册链接。
func (a *App) ensureInteractiveTailscaleEnrollments(nodes []*node.Node) {
	for _, candidate := range nodes {
		if candidate == nil || !candidate.Alive || !candidate.IsTailscale() {
			continue
		}
		authKey, _ := candidate.Mapping["auth-key"].(string)
		if strings.TrimSpace(authKey) != "" {
			continue
		}
		if _, exists := a.runner.TailscaleEnrollment(candidate.Name); exists {
			continue
		}
		a.runner.BeginTailscaleEnrollment(candidate.Name, "approval")
		go func(name string) {
			triggerCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			a.runner.TriggerTailscaleEnrollment(triggerCtx, name)
		}(candidate.Name)
	}
}

// verifyMihomoManagedNodes 使用已加载的 mihomo 代理表验证需要运行态探测的节点。
//
// 参数：
//   - ctx: context.Context，整轮刷新取消时终止后续测试。
//   - nodes: []*node.Node，包含普通、dialer-proxy 与 Tailscale 节点的本轮节点集合。
//
// 返回值：bool，只要任一候选从可用变为不可用就返回 true，提示调用方重生成配置。
//
// 错误情况：代理不存在、上游组不可用、网络失败与超时均写入 FailReason。检测串行执行，
// 因为 Runner 为保护 mihomo 全局代理表会持锁；这样避免并发 URLTest 与热更新产生竞态。
// 未配置 Exit Node 的 Tailscale 只承载 Tailnet/子网路由，公网 health-url 对其没有意义，
// 因此保持“mihomo 已加载”候选状态，实际连接仍由 mihomo 按首次流量懒启动。
func (a *App) verifyMihomoManagedNodes(ctx context.Context, nodes []*node.Node) bool {
	availabilityChanged := false
	a.mu.RLock()
	healthURL := a.cfg.HealthURL
	healthTimeout := a.cfg.HealthTimeout.D()
	a.mu.RUnlock()
	for _, n := range nodes {
		if !needsMihomoRuntimeProbe(n) {
			continue
		}
		// Tailscale 首次启动可能需要完成控制面登录与 DERP 协商；继续复用代理域的
		// 隧道超时策略，避免迁移到正式 mihomo 运行态后退化为普通节点的短超时。
		timeout := pool.ProbeTimeout(n, healthTimeout)
		delay, err := a.runner.URLTest(ctx, n.Name, healthURL, timeout)
		if err != nil {
			n.Alive = false
			n.Delay = 0
			n.FailReason = firstErrorLine(err.Error())
			// 每个节点探测完立即发布展示行：这些节点的真实延迟只能在这里得到，
			// 逐节点发布让控制台在大批链路探测期间也能逐个换掉「测速中…」。
			n.PublishResult()
			availabilityChanged = true
			continue
		}
		n.Delay = delay
		n.FailReason = ""
		n.PublishResult()
	}
	return availabilityChanged
}

// needsMihomoRuntimeProbe 判断候选节点是否需要在正式 mihomo 代理表中执行公网测速。
//
// 参数：
//   - n: *node.Node，已经完成预检查的节点。
//
// 返回值：bool，普通 dialer-proxy 节点或配置了 Exit Node 的 Tailscale 返回 true。
//
// 错误情况：无；nil、预检查失败及仅访问 Tailnet/子网路由的 Tailscale 返回 false。
// Tailscale 的判断优先于 dialer-proxy，避免“使用上游拨号但未配置 Exit Node”的节点
// 被拿公网 health-url 误判为失效。
func needsMihomoRuntimeProbe(n *node.Node) bool {
	if n == nil || !n.Alive {
		return false
	}
	if n.IsTailscale() {
		return n.TailscaleExitNode() != ""
	}
	return n.DialerProxy() != ""
}

// firstErrorLine 把底层多行错误压缩为适合节点状态展示的一行文本。
//
// 参数：
//   - message: string，可能包含换行、堆栈或协议详情的错误内容。
//
// 返回值：string，第一个换行符之前的内容；没有换行时返回原内容。
//
// 错误情况：无；空字符串原样返回。
func firstErrorLine(message string) string {
	if index := strings.IndexByte(message, '\n'); index >= 0 {
		return message[:index]
	}
	return message
}

// dialerTargets 返回当前配置中可被节点 dialer-proxy 引用的策略组名称快照。
//
// 参数：无；方法在读锁内读取 Config.Groups。
//
// 返回值：[]string，保持配置顺序的组名列表；调用方可自由修改返回切片。
//
// 错误情况：无；分组结构已经由 config.Validate 校验，空名称仍会在 pool 层忽略。
func (a *App) dialerTargets() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	targets := make([]string, 0, len(a.cfg.Groups))
	for _, group := range a.cfg.Groups {
		targets = append(targets, group.Name)
	}
	return targets
}

// restoreSnapshot 启动时只加载 nodes.json 节点快照并生成 mihomo 配置，不访问任何
// 订阅地址。快照缺失、损坏或没有健康节点时仍应用一份 DIRECT 配置，使控制台和主端口
// 可以正常启动，用户随后可通过手动同步取得节点。
//
// 参数：无；快照路径来自当前配置的 state-dir。
//
// 返回值：error，仅在 mihomo 配置生成、热加载或 TUN 实际状态校验失败时返回。
//
// 错误情况：快照读取失败会记录日志并降级为空节点启动，不会触发订阅下载；应用失败
// 必须返回给 Run，使要求 TUN 的配置仍能执行流量绕过保护。
func (a *App) restoreSnapshot() error {
	snap, err := node.LoadSnapshot(a.nodesSnapshotPath())
	if err != nil {
		log.Printf("[snapshot] %v", err)
	}
	var nodes []*node.Node
	if snap != nil {
		nodes = filterEnabledSubscriptionNodes(snap.Nodes, a.Subscriptions())
	}
	var alive []*node.Node
	for _, n := range nodes {
		if n.Alive {
			alive = append(alive, n)
		}
	}

	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	prev, err := pool.LoadSnapshot(a.snapshotPath())
	if err != nil {
		log.Printf("[alloc] load snapshot: %v (ignored)", err)
	}
	assigns := pool.Allocate(alive, a.cfg.PortRange[0], a.cfg.PortRange[1], prev)
	if err := a.regenerateLocked(assigns); err != nil {
		return fmt.Errorf("从节点快照生成启动配置失败: %w", err)
	}
	a.mu.Lock()
	a.nodes = nodes
	a.assigns = assigns
	// 节点来自快照时内存用量为空，直接从用量 sidecar 恢复，让控制台在首次
	// 健康检查之前就能展示订阅流量与到期信息；读取失败按无用量处理。
	for _, sub := range a.cfg.Subscriptions {
		if !sub.IsEnabled() {
			continue
		}
		if info, err := subscribe.ReadCachedUserInfo(a.cfg.StateDir, sub.Name); err == nil && !info.IsZero() {
			a.subInfos[sub.Name] = info
		}
	}
	a.mu.Unlock()
	if snap == nil || len(nodes) == 0 {
		log.Printf("[snapshot] 没有可用节点快照，已按空节点启动；请在控制台手动同步订阅")
		return nil
	}
	log.Printf("[snapshot] 已从快照恢复 %d 个节点（%d 个可用，%d 个端口，保存于 %s）",
		len(nodes), len(alive), len(assigns), snap.SavedAt.Format("2006-01-02 15:04:05"))
	return nil
}
