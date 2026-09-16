package app

import (
	"testing"

	"proxyd/internal/proxy/node"
)

// TestNeedsMihomoRuntimeProbe 验证二阶段探测边界：普通链式节点与 Tailscale Exit Node
// 必须通过正式 mihomo 代理表测速，仅访问 Tailnet/子网路由的 Tailscale 不得用公网
// health-url 判死。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无；通过表驱动断言表达结果。
//
// 错误情况：nil/失效节点被探测、链式节点漏测，或无 Exit Node 的 Tailscale 被误测时失败。
func TestNeedsMihomoRuntimeProbe(t *testing.T) {
	tests := []struct {
		name string
		node *node.Node
		want bool
	}{
		{name: "nil", node: nil, want: false},
		{name: "普通直连节点", node: &node.Node{Alive: true, Mapping: map[string]any{"type": "socks5"}}, want: false},
		{name: "普通链式节点", node: &node.Node{Alive: true, Mapping: map[string]any{"type": "socks5", "dialer-proxy": "上游"}}, want: true},
		{name: "失效链式节点", node: &node.Node{Alive: false, Mapping: map[string]any{"type": "socks5", "dialer-proxy": "上游"}}, want: false},
		{name: "Tailnet 节点", node: &node.Node{Alive: true, Mapping: map[string]any{"type": "tailscale", "auth-key": "k"}}, want: false},
		{name: "Tailnet 链式节点", node: &node.Node{Alive: true, Mapping: map[string]any{"type": "tailscale", "auth-key": "k", "dialer-proxy": "上游"}}, want: false},
		{name: "Exit Node", node: &node.Node{Alive: true, Mapping: map[string]any{"type": "tailscale", "auth-key": "k", "exit-node": "auto:any"}}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := needsMihomoRuntimeProbe(test.node); got != test.want {
				t.Fatalf("needsMihomoRuntimeProbe() = %t, want %t", got, test.want)
			}
		})
	}
}
