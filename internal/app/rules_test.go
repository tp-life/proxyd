package app

import (
	"path/filepath"
	"testing"

	"proxyd/internal/config"
)

// TestCustomRuleEditAndReorder 验证自定义规则支持原位编辑和顺序调整，且变更会同步
// 写入运行配置与磁盘配置；远程规则和内置 rules 不在该用例的修改范围内。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于创建隔离配置并报告断言失败。
//
// 返回值：无。
//
// 错误情况：编辑/移动失败、目标顺序错误或磁盘 custom-rules 不一致时测试失败。
func TestCustomRuleEditAndReorder(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		ManualNodes: []string{"socks5://127.0.0.1:1080#manual"},
		Listen:      "127.0.0.1",
		PortRange:   [2]int{42000, 42010},
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
		CustomRules: []string{
			"DOMAIN-SUFFIX,first.example,DIRECT",
			"DOMAIN-SUFFIX,second.example,PROXY",
		},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("保存初始配置失败: %v", err)
	}
	a, err := New(cfg, cfgPath)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)

	if err := a.UpdateRule(1, "DOMAIN-SUFFIX,edited.example,REJECT"); err != nil {
		t.Fatalf("编辑自定义规则失败: %v", err)
	}
	if err := a.MoveRule(1, 0); err != nil {
		t.Fatalf("移动自定义规则失败: %v", err)
	}
	want := []string{
		"DOMAIN-SUFFIX,edited.example,REJECT",
		"DOMAIN-SUFFIX,first.example,DIRECT",
	}
	got := a.CustomRules()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("内存规则顺序异常: %#v", got)
	}
	onDisk, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("读取提交后的配置失败: %v", err)
	}
	if len(onDisk.CustomRules) != len(want) || onDisk.CustomRules[0] != want[0] || onDisk.CustomRules[1] != want[1] {
		t.Fatalf("磁盘规则顺序异常: %#v", onDisk.CustomRules)
	}
}

// TestCustomRulePersistenceFailureRollsBack 验证自定义规则热更新成功但持久化失败时，
// 内存配置和 mihomo 运行态都会恢复，不能留下只在当前进程有效的幽灵规则。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用目录路径稳定制造配置保存失败。
//
// 返回值：无。
//
// 错误情况：方法未返回错误或失败后规则列表发生变化时测试失败。
func TestCustomRulePersistenceFailureRollsBack(t *testing.T) {
	cfg := &config.Config{
		Listen:      "127.0.0.1",
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
		CustomRules: []string{"DOMAIN-SUFFIX,old.example,DIRECT"},
	}
	a, err := New(cfg, cfg.StateDir)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)
	if err := a.UpdateRule(0, "DOMAIN-SUFFIX,new.example,PROXY"); err == nil {
		t.Fatal("配置路径是目录时编辑规则应返回持久化错误")
	}
	if got := a.CustomRules(); len(got) != 1 || got[0] != "DOMAIN-SUFFIX,old.example,DIRECT" {
		t.Fatalf("持久化失败后规则未恢复: %#v", got)
	}
}
