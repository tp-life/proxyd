package node

import (
	"strings"
	"testing"
)

// TestIsTunnel 验证隧道类出站类型集合的判定边界：五种 VPN 语义类型归类为隧道，
// 普通代理协议与缺失/未知类型不受影响。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：任一类型归类错误时测试失败。
func TestIsTunnel(t *testing.T) {
	tunnel := []string{"tailscale", "openvpn", "zerotier", "wireguard", "ssh"}
	for _, typ := range tunnel {
		if !IsTunnel(map[string]any{"type": typ}) {
			t.Errorf("类型 %q 应归类为隧道出站", typ)
		}
		n := &Node{Mapping: map[string]any{"type": typ}}
		if !n.IsTunnel() {
			t.Errorf("Node.IsTunnel(%q) 应为 true", typ)
		}
	}

	ordinary := []string{"ss", "ssr", "vmess", "vless", "trojan", "hysteria2", "tuic", "http", "socks5", "anytls", "gost_relay"}
	for _, typ := range ordinary {
		if IsTunnel(map[string]any{"type": typ}) {
			t.Errorf("类型 %q 不应归类为隧道出站", typ)
		}
	}
	if IsTunnel(nil) || IsTunnel(map[string]any{}) || IsTunnel(map[string]any{"type": 42}) {
		t.Error("nil 映射、缺失 type 或非字符串 type 应按普通节点处理")
	}
	var nilNode *Node
	if nilNode.IsTunnel() {
		t.Error("nil 节点不应归类为隧道出站")
	}
}

// TestNodeKeyTunnelCredentials 验证隧道类节点的稳定身份扩展：
// 凭据按 uuid → password → auth-key → private-key 优先级提取，
// tailscale 无 server 时以 control-url 顶替 server 位置。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：任一优先级、control-url 顶替或普通节点兼容性格式被破坏时测试失败。
func TestNodeKeyTunnelCredentials(t *testing.T) {
	ts := &Node{Mapping: map[string]any{
		"type": "tailscale", "auth-key": "tskey-auth-xxx",
	}}
	if got := ts.Key(); got != "tailscale|||tskey-auth-xxx" {
		t.Fatalf("tailscale Key = %q, want tailscale|||tskey-auth-xxx", got)
	}

	tsHeadscale := &Node{Mapping: map[string]any{
		"type": "tailscale", "control-url": "https://headscale.example.com", "auth-key": "tskey-auth-xxx",
	}}
	if got := tsHeadscale.Key(); got != "tailscale|https://headscale.example.com||tskey-auth-xxx" {
		t.Fatalf("tailscale(control-url) Key = %q", got)
	}
	if ts.Key() == tsHeadscale.Key() {
		t.Fatal("不同控制面的 tailscale 节点不应共享身份")
	}

	wg := &Node{Mapping: map[string]any{
		"type": "wireguard", "server": "wg.example.com", "port": 51820, "private-key": "wg-private",
	}}
	if got := wg.Key(); got != "wireguard|wg.example.com|51820|wg-private" {
		t.Fatalf("wireguard Key = %q", got)
	}

	ssh := &Node{Mapping: map[string]any{
		"type": "ssh", "server": "ssh.example.com", "port": 22, "username": "u", "private-key": "ssh-private",
	}}
	if got := ssh.Key(); got != "ssh|ssh.example.com|22|ssh-private" {
		t.Fatalf("ssh Key = %q", got)
	}

	// 优先级：uuid/password 存在时 auth-key/private-key 不参与身份。
	mixed := &Node{Mapping: map[string]any{
		"type": "ssh", "server": "ssh.example.com", "port": 22, "password": "pw", "private-key": "ssh-private",
	}}
	if got := mixed.Key(); got != "ssh|ssh.example.com|22|pw" {
		t.Fatalf("password 应优先于 private-key: %q", got)
	}

	// 普通节点 Key 格式保持不变：新增凭据字段不影响旧格式（缺省即为空串，与旧版一致）。
	plain := &Node{Mapping: map[string]any{
		"type": "socks5", "server": "127.0.0.1", "port": 1080, "password": "secret",
	}}
	if got := plain.Key(); got != "socks5|127.0.0.1|1080|secret" {
		t.Fatalf("普通节点 Key 兼容性被破坏: %q", got)
	}
}

// TestWithTunnelStateDir 验证 tsnet 状态目录改写的边界：仅 tailscale 类型且未显式
// 设置 state-dir 时改写；改写发生在副本上不污染原映射；隔离目录带 Key 哈希后缀。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：任一边界（类型、显式目录、空 stateDir、nil 节点、副本语义、目录隔离）
// 被破坏时测试失败。
func TestWithTunnelStateDir(t *testing.T) {
	ts := &Node{Name: "ts-exit", Mapping: map[string]any{
		"type": "tailscale", "auth-key": "tskey-auth-xxx",
	}}
	out, rewritten := WithTunnelStateDir("/state", ts)
	if !rewritten {
		t.Fatal("未显式设置 state-dir 的 tailscale 节点应被改写")
	}
	dir, _ := out["state-dir"].(string)
	if !strings.HasPrefix(dir, "/state/tsnet/ts-exit-") {
		t.Fatalf("改写后的 state-dir = %q，应为 /state/tsnet/ts-exit-<哈希>", dir)
	}
	if _, exists := ts.Mapping["state-dir"]; exists {
		t.Fatal("改写不得污染原映射（Key/快照语义依赖原始 Mapping）")
	}

	// 同名不同凭据的节点获得隔离目录。
	other := &Node{Name: "ts-exit", Mapping: map[string]any{
		"type": "tailscale", "auth-key": "tskey-auth-yyy",
	}}
	otherOut, _ := WithTunnelStateDir("/state", other)
	if otherOut["state-dir"] == out["state-dir"] {
		t.Fatal("不同凭据的同名节点不应共享 tsnet 状态目录")
	}

	// 显式设置 state-dir 时尊重用户配置。
	custom := &Node{Name: "ts-custom", Mapping: map[string]any{
		"type": "tailscale", "auth-key": "tskey-auth-xxx", "state-dir": "/my/ts",
	}}
	if got, rewritten := WithTunnelStateDir("/state", custom); rewritten || got["state-dir"] != "/my/ts" {
		t.Fatalf("显式 state-dir 不应被改写: rewritten=%v dir=%v", rewritten, got["state-dir"])
	}

	// 非隧道类型、空 stateDir、nil 节点均不改写。
	plain := &Node{Name: "ss", Mapping: map[string]any{"type": "ss"}}
	if _, rewritten := WithTunnelStateDir("/state", plain); rewritten {
		t.Fatal("非 tailscale 节点不应被改写")
	}
	if _, rewritten := WithTunnelStateDir("", ts); rewritten {
		t.Fatal("空 stateDir 不应触发改写")
	}
	if got, rewritten := WithTunnelStateDir("/state", nil); rewritten || got != nil {
		t.Fatal("nil 节点应原样返回 nil")
	}
}
