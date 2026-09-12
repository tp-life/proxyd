package app

import (
	"path/filepath"
	"testing"

	"proxyd/internal/config"
	"proxyd/internal/proxy/groupstate"
	"proxyd/internal/proxy/node"
)

// selectGroupTestApp 构造带一个 select 分组和一个存活节点的测试应用。
func selectGroupTestApp(t *testing.T, groupType string) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{
		ManualNodes: []any{"socks5://127.0.0.1:1080#self"},
		Listen:      "127.0.0.1",
		PortRange:   [2]int{42000, 42010},
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    dir,
		Rules:       []string{"MATCH,PROXY"},
		Groups: []config.NodeGroup{{
			Name: "vpn", Port: 43000, Type: groupType, Subscription: "manual",
		}},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("保存初始配置失败: %v", err)
	}
	application, err := New(cfg, cfgPath)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(application.Shutdown)
	application.nodes = []*node.Node{{
		Name:         "self",
		Subscription: "manual",
		Alive:        true,
		Mapping: map[string]any{
			"name": "self", "type": "socks5", "server": "127.0.0.1", "port": 1080,
		},
	}}
	return application, cfgPath
}

// TestSetGroupSelectedPersistsAndReloads 验证 select 分组选中项持久化到
// state-dir/group-selected.json，热更新成功且重启（重新加载）后选中值可读回。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：选中失败、状态文件缺失或内容错误时测试失败。
func TestSetGroupSelectedPersistsAndReloads(t *testing.T) {
	application, _ := selectGroupTestApp(t, config.GroupTypeSelect)
	if err := application.SetGroupSelected("vpn", "self"); err != nil {
		t.Fatalf("SetGroupSelected 失败: %v", err)
	}
	selected, err := groupstate.Load(application.groupSelectedPath())
	if err != nil {
		t.Fatalf("读取选中状态失败: %v", err)
	}
	if selected["vpn"] != "self" {
		t.Fatalf("选中状态 = %v, 期望 vpn->self", selected)
	}
}

// TestSetGroupSelectedValidates 验证选中入口的领域约束：分组必须存在、必须是
// select 类型、节点必须在分组当前可用成员中。
func TestSetGroupSelectedValidates(t *testing.T) {
	application, _ := selectGroupTestApp(t, config.GroupTypeSelect)
	if err := application.SetGroupSelected("不存在", "self"); err == nil {
		t.Error("不存在的分组应报错")
	}
	if err := application.SetGroupSelected("vpn", "其它节点"); err == nil {
		t.Error("不在分组可用成员中的节点应报错")
	}
	// 失效节点不可选中
	application.nodes[0].Alive = false
	if err := application.SetGroupSelected("vpn", "self"); err == nil {
		t.Error("失效节点应不可选中")
	}
	application.nodes[0].Alive = true

	// 非 select 分组不支持手动选中
	application2, _ := selectGroupTestApp(t, config.GroupTypeFallback)
	if err := application2.SetGroupSelected("vpn", "self"); err == nil {
		t.Error("fallback 分组不应支持手动选中")
	}
}
