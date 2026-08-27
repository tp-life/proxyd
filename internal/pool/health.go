// Package pool 提供节点健康检测与本地端口分配能力。
package pool

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"proxyd/internal/node"
)

// defaultConcurrency 是未指定并发数时的默认值。
const defaultConcurrency = 32

// Check 并发地对每个节点做 URL 延迟探测，结果直接写回 node.Alive / node.Delay。
// 单个节点失败不会让 Check 返回错误；失败（含超时、解析失败）的节点
// 统一标记为 Alive=false、Delay=0。
func Check(ctx context.Context, nodes []*node.Node, url string, timeout time.Duration, concurrency int) {
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, n := range nodes {
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
}

// checkOne 检测单个节点：解析出站配置后对 url 发起一次 URLTest。
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

// firstLine 取错误信息首行（避免长堆栈进入 UI）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
