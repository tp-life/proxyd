package app

import (
	"path/filepath"
	"testing"

	"proxyd/internal/config"
	"proxyd/internal/proxy/groupstate"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/pool"
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

// TestSetGroupSelectedBuiltinProxy 验证内置 PROXY 组（默认出口）的选中语义：
// 存活节点、DIRECT 恒可选；无 assigns（无可用节点分配）时 AUTO 拒绝；
// 补齐 assigns 后 AUTO 可选；未知节点名拒绝。选中项以节点名持久化到
// group-selected.json 的 "PROXY" 键。
func TestSetGroupSelectedBuiltinProxy(t *testing.T) {
	application, _ := selectGroupTestApp(t, config.GroupTypeSelect)

	for _, target := range []string{"self", "DIRECT"} {
		if err := application.SetGroupSelected("PROXY", target); err != nil {
			t.Fatalf("PROXY 选 %q 失败: %v", target, err)
		}
		selected, err := groupstate.Load(application.groupSelectedPath())
		if err != nil {
			t.Fatalf("读取选中状态失败: %v", err)
		}
		if selected["PROXY"] != target {
			t.Fatalf("PROXY 选中状态 = %v, 期望 %q", selected, target)
		}
	}

	if err := application.SetGroupSelected("PROXY", "AUTO"); err == nil {
		t.Error("无可用节点分配时 AUTO 应不可选")
	}
	application.assigns = []pool.Assignment{{Port: 42000, Node: application.nodes[0]}}
	if err := application.SetGroupSelected("PROXY", "AUTO"); err != nil {
		t.Fatalf("有可用节点后 AUTO 应可选: %v", err)
	}
	if got := application.GroupSelected()["PROXY"]; got != "AUTO" {
		t.Fatalf("PROXY 选中项 = %q, 期望 AUTO", got)
	}

	if err := application.SetGroupSelected("PROXY", "不存在节点"); err == nil {
		t.Error("未知节点名应报错")
	}
	application.nodes[0].Alive = false
	if err := application.SetGroupSelected("PROXY", "self"); err == nil {
		t.Error("失效节点应不可选为默认出口")
	}
}

// TestSetGroupSelectedBuiltinProxyRollback 验证 PROXY 选择热更新失败时状态文件
// 回滚到修改前内容：通过注入一个 mihomo 无法识别的存活节点使生成自检失败，
// 旧选中值（DIRECT）必须原样保留。
func TestSetGroupSelectedBuiltinProxyRollback(t *testing.T) {
	application, _ := selectGroupTestApp(t, config.GroupTypeSelect)
	if err := application.SetGroupSelected("PROXY", "DIRECT"); err != nil {
		t.Fatalf("PROXY 选 DIRECT 失败: %v", err)
	}

	application.nodes = append(application.nodes, &node.Node{
		Name: "无效节点", Subscription: "manual", Alive: true,
		Mapping: map[string]any{"name": "无效节点", "type": "not-a-real-proxy", "server": "127.0.0.1", "port": 1080},
	})
	if err := application.SetGroupSelected("PROXY", "self"); err == nil {
		t.Fatal("无效节点应触发热更新失败")
	}
	selected, err := groupstate.Load(application.groupSelectedPath())
	if err != nil {
		t.Fatalf("读取选中状态失败: %v", err)
	}
	if selected["PROXY"] != "DIRECT" {
		t.Fatalf("热更失败后选中状态未回滚: %v, 期望 PROXY->DIRECT", selected)
	}
}
