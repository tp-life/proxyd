// Package pool 提供节点健康检测与本地端口分配能力。
package pool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"proxyd/internal/proxy/node"
)

// defaultConcurrency 是未指定并发数时的默认值。
const defaultConcurrency = 32

// tunnelTimeoutFactor 是隧道类节点健康检测超时相对全局 health-timeout 的放大倍数：
// tsnet/openvpn 首次拨号涉及 DERP 协商或 TLS 握手，普通节点的秒级超时对它们必然过紧。
const tunnelTimeoutFactor = 3

// Check 并发地检测普通节点，并为 Tailscale 与 dialer-proxy 节点建立可加载候选状态。
//
// 参数：
//   - ctx: context.Context，控制整轮检测的取消；取消后尚未执行的节点标记为不可用。
//   - nodes: []*node.Node，待检测节点；结果原地写回 Alive、Delay 与 FailReason。
//   - url: string，普通节点 URLTest 使用的探测地址。
//   - timeout: time.Duration，单节点网络探测的最大时长；隧道类节点自动放大
//     tunnelTimeoutFactor 倍（见 ProbeTimeout）。
//   - concurrency: int，最大并发探测数；小于等于 0 时使用默认值 32。
//   - stateDir: string，proxyd 状态目录，用于 tailscale 节点的 tsnet 状态目录改写
//     （见 node.WithTunnelStateDir）；为空时不改写。
//   - dialerTargets: ...string，可作为链式目标的已配置 proxy-group 名称。
//
// 返回值：无；单节点失败通过节点状态表达，不中断其它节点检测。
//
// 错误情况：普通节点的超时、协议解析和网络错误写入 FailReason。Tailscale 出站
// 不在预检查阶段拨号，避免在正式 mihomo 核心之外启动第二个 tsnet；这里只验证
// mihomo 能否解析配置。链式节点同样先校验结构、依赖和循环引用，需公网探测的
// Tailscale Exit Node 与普通链式节点由应用层在完整代理表加载后执行端到端测速。
func Check(ctx context.Context, nodes []*node.Node, url string, timeout time.Duration, concurrency int, stateDir string, dialerTargets ...string) {
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}

	ordinaryCount := 0
	for _, n := range nodes {
		if n != nil && n.DialerProxy() == "" {
			ordinaryCount++
		}
	}

	// 固定 worker 池把 goroutine 数量限制在配置并发度内。旧实现虽然用信号量限制了
	// 网络请求数，却仍为每个节点创建一个 goroutine；节点很多或探测超时时，这些排队
	// goroutine 会长期保留栈和节点引用，造成与节点数线性相关的额外内存占用。
	workerCount := min(concurrency, ordinaryCount)
	jobs := make(chan *node.Node)
	var wg sync.WaitGroup
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range jobs {
				// 即使整轮已取消，也让 checkOne 处理每个排队节点。它会从已取消的父上下文
				// 立即返回并统一清空旧 Alive/Delay 状态，避免刷新取消后残留上轮健康结果。
				checkOne(ctx, n, url, timeout, stateDir)
			}
		}()
	}
	for _, n := range nodes {
		if n != nil && n.DialerProxy() == "" {
			jobs <- n
		}
	}
	close(jobs)
	wg.Wait()
	resolveDialerCandidates(nodes, dialerTargets)
}

// checkOne 检测一个不含 dialer-proxy 的节点，或建立 Tailscale 配置候选。
//
// 参数：
//   - ctx: context.Context，继承整轮检测取消信号。
//   - n: *node.Node，待检测节点，结果原地写回。
//   - url: string，URLTest 目标地址。
//   - timeout: time.Duration，普通节点允许占用的最大检测时间；隧道类节点按倍数放宽。
//   - stateDir: string，proxyd 状态目录，用于 tailscale 节点的 tsnet 状态目录改写。
//
// 返回值：无；普通节点成功时写入延迟并标记 Alive；Tailscale 配置可解析时标记为
// mihomo 候选且延迟保持 0；失败写入首行错误。
//
// 错误情况：配置无法解析、网络失败或超时都将节点标记为不可用，不向调用方抛错。
func checkOne(ctx context.Context, n *node.Node, url string, timeout time.Duration, stateDir string) {
	n.Alive = false
	n.Delay = 0
	n.FailReason = ""

	// Tailscale 映射仍使用正式运行时相同的隔离 state-dir 做语义校验，但预检查绝不
	// 调用 URLTest：URLTest 会启动 tsnet，造成健康检查实例与正式 mihomo 实例同时
	// 管理同一身份。解析得到的临时 Adapter 必须关闭，因为 NewTailscale 会注册 DNS
	// transport，即使尚未拨号也持有进程级资源。
	mapping, _ := node.WithTunnelStateDir(stateDir, n)
	proxy, err := adapter.ParseProxy(mapping)
	if err != nil {
		// 配置无法解析的节点视为不可用，跳过探测
		n.FailReason = "配置解析失败: " + err.Error()
		return
	}
	defer func() {
		// 健康探测 Adapter 不属于正式 mihomo 代理表，必须在本轮结束时释放。关闭失败
		// 不改变已经完成的网络探测结论；正式运行态会由 Runner/Executor 独立管理。
		_ = proxy.Close()
	}()
	if n.IsTailscale() {
		if err := ctx.Err(); err != nil {
			n.FailReason = firstLine(err.Error())
			return
		}
		n.Alive = true
		return
	}

	cctx, cancel := context.WithTimeout(ctx, ProbeTimeout(n, timeout))
	defer cancel()

	delay, err := proxy.URLTest(cctx, url, nil)
	if err != nil {
		n.FailReason = firstLine(err.Error())
		return
	}
	n.Alive = true
	n.Delay = delay
}

// resolveDialerCandidates 按依赖顺序校验链式节点，使其能够进入首次 mihomo 配置。
//
// 参数：
//   - nodes: []*node.Node，普通节点已完成真实测速、链式节点尚未检测的完整节点集。
//   - dialerTargets: []string，配置层已经校验过、生成时会存在的 proxy-group 名称。
//
// 返回值：无；链式节点的候选状态原地写回。
//
// 错误情况：依赖节点不可用、引用不存在、循环引用或节点配置无法解析时标记为不可用。
// `DIRECT` 是 mihomo 内置出站，已配置策略组由 dialerTargets 明确传入；其它未知名称
// 不作为候选，避免被 include/exclude 移除的依赖使整份运行配置自检失败。
func resolveDialerCandidates(nodes []*node.Node, dialerTargets []string) {
	byName := make(map[string]*node.Node, len(nodes))
	for _, n := range nodes {
		if n != nil {
			byName[n.Name] = n
		}
	}
	knownTargets := map[string]bool{"DIRECT": true}
	for _, target := range dialerTargets {
		if target = strings.TrimSpace(target); target != "" {
			knownTargets[target] = true
		}
	}
	states := make(map[*node.Node]uint8, len(nodes))
	var resolve func(*node.Node) bool
	resolve = func(n *node.Node) bool {
		if n == nil {
			return false
		}
		if states[n] == 2 {
			return n.Alive
		}
		if states[n] == 1 {
			n.Alive = false
			n.Delay = 0
			n.FailReason = "dialer-proxy 存在循环引用"
			return false
		}
		ref := n.DialerProxy()
		if ref == "" {
			states[n] = 2
			return n.Alive
		}

		states[n] = 1
		dependency := byName[ref]
		if dependency == nil && !knownTargets[ref] {
			n.Alive = false
			n.Delay = 0
			n.FailReason = fmt.Sprintf("链路依赖 %q 不存在", ref)
			states[n] = 2
			return false
		}
		if dependency != nil && !resolve(dependency) {
			n.Alive = false
			n.Delay = 0
			n.FailReason = fmt.Sprintf("链路依赖 %q 当前不可用", ref)
			states[n] = 2
			return false
		}
		proxy, err := adapter.ParseProxy(n.Mapping)
		if err != nil {
			n.Alive = false
			n.Delay = 0
			n.FailReason = "配置解析失败: " + err.Error()
			states[n] = 2
			return false
		}
		// 链式候选这里只验证 mihomo 配置结构，不属于正式代理表；及时关闭可释放
		// Tailscale DNS transport、SSH 会话池等适配器级资源，且不会发起任何网络连接。
		_ = proxy.Close()

		// 已知节点依赖继承其延迟，未知名称可能是稍后生成的 proxy-group；后者使用
		// 最大值避免在端口容量不足时挤掉已经完成真实测速的普通节点。
		n.Alive = true
		n.Delay = ^uint16(0)
		n.FailReason = ""
		if dependency != nil {
			n.Delay = dependency.Delay
		}
		states[n] = 2
		return true
	}
	for _, n := range nodes {
		if n != nil && n.DialerProxy() != "" {
			resolve(n)
		}
	}
}

// ProbeTimeout 返回单个节点允许占用的最大探测时长。
//
// 该策略同时提供给预检查和应用层的 mihomo 运行态探测，确保 Tailscale 等隧道
// 节点即使改由正式 mihomo 代理表管理，也仍有足够时间完成首次控制面与中继协商。
//
// 参数：
//   - n: *node.Node，待检测节点。
//   - base: time.Duration，全局 health-timeout。
//
// 返回值：time.Duration，隧道类节点放大 tunnelTimeoutFactor 倍，其余节点原样返回。
//
// 错误情况：无；nil 节点按普通节点处理。
func ProbeTimeout(n *node.Node, base time.Duration) time.Duration {
	if n.IsTunnel() {
		return base * tunnelTimeoutFactor
	}
	return base
}

// firstLine 取错误信息首行，避免长堆栈进入 UI。
//
// 参数：
//   - s: string，可能包含多行堆栈或底层错误的文本。
//
// 返回值：string，首个换行符之前的内容；无换行时返回原字符串。
//
// 错误情况：无；空字符串原样返回。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
