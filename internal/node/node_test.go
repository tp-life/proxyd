// Package node 提供代理节点领域模型的单元测试。
package node

import "testing"

// TestNodeKeyIncludesDialerProxy 验证链式拨号目标属于节点稳定身份，同时普通节点
// 继续使用历史 Key 格式，避免升级后丢失已有端口快照。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：不同链路被生成相同 Key，或普通节点 Key 发生兼容性变化时测试失败。
func TestNodeKeyIncludesDialerProxy(t *testing.T) {
	mapping := map[string]any{
		"type": "socks5", "server": "127.0.0.1", "port": 1080, "password": "secret",
	}
	plain := &Node{Mapping: mapping}
	if got := plain.Key(); got != "socks5|127.0.0.1|1080|secret" {
		t.Fatalf("普通节点 Key 兼容性被破坏: %q", got)
	}

	viaA := &Node{Mapping: map[string]any{
		"type": "socks5", "server": "127.0.0.1", "port": 1080,
		"password": "secret", "dialer-proxy": "入口 A",
	}}
	viaB := &Node{Mapping: map[string]any{
		"type": "socks5", "server": "127.0.0.1", "port": 1080,
		"password": "secret", "dialer-proxy": "入口 B",
	}}
	if viaA.Key() == viaB.Key() {
		t.Fatalf("不同 dialer-proxy 的节点不应被去重: %q", viaA.Key())
	}
}
