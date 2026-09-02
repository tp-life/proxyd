// Package pool 提供节点健康检测与本地端口分配能力。
package pool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"proxyd/internal/node"
)

// defaultConcurrency 是未指定并发数时的默认值。
const defaultConcurrency = 32

// Check 并发地检测普通节点，并为 dialer-proxy 链式节点建立可加载的候选状态。
//
// 参数：
//   - ctx: context.Context，控制整轮检测的取消；取消后尚未执行的节点标记为不可用。
//   - nodes: []*node.Node，待检测节点；结果原地写回 Alive、Delay 与 FailReason。
//   - url: string，普通节点 URLTest 使用的探测地址。
//   - timeout: time.Duration，单节点网络探测的最大时长。
//   - concurrency: int，最大并发探测数；小于等于 0 时使用默认值 32。
//   - dialerTargets: ...string，可作为链式目标的已配置 proxy-group 名称。
//
// 返回值：无；单节点失败通过节点状态表达，不中断其它节点检测。
//
// 错误情况：普通节点的超时、协议解析和网络错误写入 FailReason。链式节点在
// mihomo 配置加载前无法完成真实 URLTest，因此这里只校验配置结构、依赖存在性和
// 循环引用，并继承上游延迟形成候选；应用层加载完整代理表后会再次做端到端测速。
func Check(ctx context.Context, nodes []*node.Node, url string, timeout time.Duration, concurrency int, dialerTargets ...string) {
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.DialerProxy() != "" {
			continue
		}
		wg.Add(1)
		go func(n *node.Node) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				// 整体取消时不再排队，直接标记死亡
				n.Alive = false
				n.Delay = 0
				return
			}
			checkOne(ctx, n, url, timeout)
		}(n)
	}
	wg.Wait()
	resolveDialerCandidates(nodes, dialerTargets)
}

// checkOne 检测一个不含 dialer-proxy 的普通节点。
//
// 参数：
//   - ctx: context.Context，继承整轮检测取消信号。
//   - n: *node.Node，待检测节点，结果原地写回。
//   - url: string，URLTest 目标地址。
//   - timeout: time.Duration，该节点允许占用的最大检测时间。
//
// 返回值：无；成功写入延迟并标记 Alive，失败写入首行错误。
//
// 错误情况：配置无法解析、网络失败或超时都将节点标记为不可用，不向调用方抛错。
func checkOne(ctx context.Context, n *node.Node, url string, timeout time.Duration) {
	n.Alive = false
	n.Delay = 0
	n.FailReason = ""

	proxy, err := adapter.ParseProxy(n.Mapping)
	if err != nil {
		// 配置无法解析的节点视为不可用，跳过探测
		n.FailReason = "配置解析失败: " + err.Error()
		return
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
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
		if _, err := adapter.ParseProxy(n.Mapping); err != nil {
			n.Alive = false
			n.Delay = 0
			n.FailReason = "配置解析失败: " + err.Error()
			states[n] = 2
			return false
		}

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
