package app

import (
	"context"
	"testing"

	"proxyd/internal/config"
	proxytailscale "proxyd/internal/proxy/tailscale"
)

// TestSetupTailscaleCreatesAtomicAccess 验证单次用例会创建无 auth-key 的审批出站、
// 单成员 select 分组和自动端口，并在重复名称失败时保持已提交配置不变。
//
// 参数说明：t 是 Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：真实 mihomo 配置解析失败、审批节点仍强制 auth-key、自动端口错误或
// 第二次失败留下半套配置时失败。
func TestSetupTailscaleCreatesAtomicAccess(t *testing.T) {
	a, err := New(&config.Config{
		Listen:    "127.0.0.1",
		PortRange: [2]int{42000, 42010},
		MixedPort: 41999,
		Mode:      "rule",
		LogLevel:  "silent",
		StateDir:  t.TempDir(),
		Rules:     []string{"MATCH,PROXY"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)

	result, err := a.SetupTailscale(context.Background(), proxytailscale.Setup{
		Name:         "campone",
		ControlURL:   "http://127.0.0.1:1",
		Hostname:     "proxyd-test",
		AuthMode:     proxytailscale.AuthModeApproval,
		AccessMode:   proxytailscale.AccessModeProxy,
		AcceptRoutes: true,
		UDP:          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.GroupName != "campone-access" || result.Port != 43000 || result.Enrollment.State != "starting" {
		t.Fatalf("接入结果异常: %+v", result)
	}
	cfg := a.Config()
	if len(cfg.ManualNodes) != 1 || len(cfg.Groups) != 1 {
		t.Fatalf("一体化配置数量异常: manual=%d groups=%d", len(cfg.ManualNodes), len(cfg.Groups))
	}
	mapping, ok := cfg.ManualNodes[0].(map[string]any)
	if !ok || mapping["auth-key"] != nil || mapping["control-url"] != "http://127.0.0.1:1" {
		t.Fatalf("审批模式出站映射异常: %#v", cfg.ManualNodes[0])
	}
	if cfg.Groups[0].Type != config.GroupTypeSelect || len(cfg.Groups[0].Nodes) != 1 || cfg.Groups[0].Nodes[0] != "campone" {
		t.Fatalf("一体化策略组异常: %+v", cfg.Groups[0])
	}

	if _, err := a.SetupTailscale(context.Background(), proxytailscale.Setup{
		Name: "campone", ControlURL: "http://127.0.0.1:1", AuthMode: proxytailscale.AuthModeApproval, AccessMode: proxytailscale.AccessModeProxy,
	}); err == nil {
		t.Fatal("重复节点名应被拒绝")
	}
	cfg = a.Config()
	if len(cfg.ManualNodes) != 1 || len(cfg.Groups) != 1 {
		t.Fatalf("失败事务污染配置: manual=%d groups=%d", len(cfg.ManualNodes), len(cfg.Groups))
	}
}

// TestTerminateTailscaleSetupRemovesAggregateAndAllowsRecreate 验证失败或待审批的
// Tailscale 接入可以作为一个聚合整体终止：节点、单节点策略组、专属路由和内存注册
// 状态同步删除，并且相同名称随后可以重新创建。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文，用于创建隔离状态目录并报告事务断言失败。
//
// 返回值说明：无；通过终止结果、配置快照、注册状态和同名重建结果表达成功。
//
// 错误情况：任一配置残留、无节点时运行态无法收敛、注册状态未清除，或同名重建仍
// 被拒绝时测试失败。与接入组无关的自定义规则必须保留，避免终止流程误删用户配置。
func TestTerminateTailscaleSetupRemovesAggregateAndAllowsRecreate(t *testing.T) {
	a, err := New(&config.Config{
		Listen:      "127.0.0.1",
		PortRange:   [2]int{42000, 42010},
		MixedPort:   41999,
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
		CustomRules: []string{"IP-CIDR,100.64.0.1/32,campone-access,no-resolve", "DOMAIN,keep.example,DIRECT"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)

	request := proxytailscale.Setup{
		Name:       "campone",
		ControlURL: "http://127.0.0.1:1",
		AuthMode:   proxytailscale.AuthModeApproval,
		AccessMode: proxytailscale.AccessModeProxy,
	}
	if _, err := a.SetupTailscale(context.Background(), request); err != nil {
		t.Fatalf("创建待终止接入失败: %v", err)
	}

	result, err := a.TerminateTailscaleSetup(context.Background(), "campone")
	if err != nil {
		t.Fatalf("终止 Tailscale 接入失败: %v", err)
	}
	if result.Name != "campone" || len(result.RemovedGroups) != 1 || result.RemovedGroups[0] != "campone-access" || result.RemovedRules != 1 {
		t.Fatalf("终止结果异常: %+v", result)
	}
	cfg := a.Config()
	if len(cfg.ManualNodes) != 0 || len(cfg.Groups) != 0 {
		t.Fatalf("终止后仍残留一体化配置: manual=%d groups=%d", len(cfg.ManualNodes), len(cfg.Groups))
	}
	if len(cfg.CustomRules) != 1 || cfg.CustomRules[0] != "DOMAIN,keep.example,DIRECT" {
		t.Fatalf("专属路由清理范围异常: %#v", cfg.CustomRules)
	}
	if _, exists := a.TailscaleEnrollment("campone"); exists {
		t.Fatal("终止后不应保留内存注册状态")
	}
	if len(a.Nodes()) != 0 {
		t.Fatalf("终止最后一个节点后运行态应为空: %+v", a.Nodes())
	}

	if _, err := a.SetupTailscale(context.Background(), request); err != nil {
		t.Fatalf("终止后应允许同名重新创建: %v", err)
	}
}
