package app

import (
	"context"
	"testing"

	"proxyd/internal/config"
)

// TestTerminateOpenVPNSetupRemovesAggregate 验证一体化删除会同时清理 OpenVPN
// 节点、已清空的单成员策略组和该组的受管 TUN 路由，并保留无关规则。
//
// 参数说明：t 是 Go 测试上下文，用于创建隔离状态目录并报告断言失败。
//
// 返回值说明：无；通过删除摘要和应用配置快照表达成功。
//
// 错误情况：节点类型识别失败、组或路由残留、无关规则被误删、空运行态无法重建，
// 或同名重复删除未报错时测试失败。
func TestTerminateOpenVPNSetupRemovesAggregate(t *testing.T) {
	a, err := New(&config.Config{
		Listen:    "127.0.0.1",
		PortRange: [2]int{42000, 42010},
		MixedPort: 41999,
		Mode:      "rule",
		LogLevel:  "silent",
		StateDir:  t.TempDir(),
		Rules:     []string{"MATCH,PROXY"},
		ManualNodes: []any{map[string]any{
			"name": "office", "type": "openvpn", "server": "vpn.example.com", "port": 1194,
			"ca": "certificate", "username": "alice", "password": "secret",
		}},
		Groups:      []config.NodeGroup{{Name: "office-access", Port: 43000, Type: config.GroupTypeSelect, Nodes: []string{"office"}}},
		CustomRules: []string{"IP-CIDR,10.20.0.0/16,office-access,no-resolve", "DOMAIN,keep.example,DIRECT"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)

	result, err := a.TerminateOpenVPNSetup(context.Background(), "office")
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "office" || len(result.RemovedGroups) != 1 || result.RemovedGroups[0] != "office-access" || result.RemovedRules != 1 {
		t.Fatalf("删除摘要异常: %+v", result)
	}
	cfg := a.Config()
	if len(cfg.ManualNodes) != 0 || len(cfg.Groups) != 0 {
		t.Fatalf("OpenVPN 聚合仍有残留: manual=%d groups=%d", len(cfg.ManualNodes), len(cfg.Groups))
	}
	if len(cfg.CustomRules) != 1 || cfg.CustomRules[0] != "DOMAIN,keep.example,DIRECT" {
		t.Fatalf("无关规则被误删: %#v", cfg.CustomRules)
	}
	if _, err := a.TerminateOpenVPNSetup(context.Background(), "office"); err == nil {
		t.Fatal("重复删除应返回节点不存在")
	}
}
