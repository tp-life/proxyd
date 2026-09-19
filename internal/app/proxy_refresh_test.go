package app

import (
	"os"
	"path/filepath"
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

// TestApplyConfigSkipsUnchangedReload 验证配置字节未变化时不会重复热更新 mihomo。
//
// 功能说明：
// 后台健康检查每 5 分钟触发一轮刷新，节点可用性没有变化时生成的配置与当前运行态完全
// 一致。此前的实现会让每一轮都重建整棵 tunnel（重新加载 geo 与规则集），既造成周期性
// CPU 尖峰，也是进程 RSS 高水位的主要来源。这里用 Runner 的成功应用计数验证稳态快路径
// 确实跳过了热更新，同时确认配置真的变化时仍然必须重新应用。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，负责创建临时配置路径并报告断言失败。
//
// 返回值：无。
//
// 错误情况：相同配置重复应用仍触发热更新，或配置变化后未热更新时测试失败。
func TestApplyConfigSkipsUnchangedReload(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("rules:\n  - MATCH,PROXY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newLANShareTestApp(t, "127.0.0.1", cfgPath)

	if err := a.Regenerate(); err != nil {
		t.Fatalf("首次生成并应用配置失败: %v", err)
	}
	first := a.runner.Reloads()
	if first == 0 {
		t.Fatal("首次应用必须真正热更新 mihomo")
	}
	applied := a.appliedConfig
	if len(applied) == 0 {
		t.Fatal("成功应用后必须记录已生效配置")
	}
	if !a.configApplied(applied) {
		t.Fatal("记录的已生效配置必须能通过等值比较")
	}

	if err := a.Regenerate(); err != nil {
		t.Fatalf("重复应用相同配置失败: %v", err)
	}
	if got := a.runner.Reloads(); got != first {
		t.Fatalf("配置未变化时不应再热更新: reloads %d -> %d", first, got)
	}

	// 清空记录模拟核心被停用/替换：之后必须重新热更新，而不是继续跳过。
	a.clearAppliedConfig()
	if a.configApplied(applied) {
		t.Fatal("clearAppliedConfig 之后不应再认为配置已生效")
	}
	if err := a.Regenerate(); err != nil {
		t.Fatalf("清空记录后重新应用失败: %v", err)
	}
	if got := a.runner.Reloads(); got <= first {
		t.Fatalf("清空记录后必须重新热更新: reloads %d -> %d", first, got)
	}

	before := a.runner.Reloads()
	if err := a.SetLANShare(true); err != nil {
		t.Fatalf("开启局域网共享失败: %v", err)
	}
	if got := a.runner.Reloads(); got <= before {
		t.Fatalf("配置真实变化后必须热更新: reloads %d -> %d", before, got)
	}
}
