package app

import (
	"path/filepath"
	"testing"

	"proxyd/internal/config"
)

// TestUpdateGroupCommitsAndPersists 验证策略分组编辑会同时提交到内存、mihomo 运行态和磁盘配置。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于创建隔离目录并报告断言失败。
//
// 返回值：无。
//
// 错误情况：分组编辑失败、内存字段不一致或磁盘未保存目标值时测试失败。
func TestUpdateGroupCommitsAndPersists(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		ManualNodes: []string{"socks5://127.0.0.1:1080#manual"},
		Listen:      "127.0.0.1",
		PortRange:   [2]int{42000, 42010},
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
		Groups: []config.NodeGroup{{
			Name: "media", Port: 43000, Type: config.GroupTypeFallback, Subscription: "manual",
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

	next := config.NodeGroup{
		Name: "media", Port: 43001, Type: config.GroupTypeLoadBalance, Subscription: "manual",
	}
	if err := application.UpdateGroup("media", next); err != nil {
		t.Fatalf("编辑策略分组失败: %v", err)
	}
	groups := application.Groups()
	if len(groups) != 1 || groups[0].Port != 43001 || groups[0].Type != config.GroupTypeLoadBalance {
		t.Fatalf("内存分组未提交目标值: %+v", groups)
	}
	onDisk, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("读取提交后的配置失败: %v", err)
	}
	if len(onDisk.Groups) != 1 || onDisk.Groups[0].Port != 43001 || onDisk.Groups[0].Type != config.GroupTypeLoadBalance {
		t.Fatalf("磁盘分组未提交目标值: %+v", onDisk.Groups)
	}
}

// TestUpdateGroupPersistenceFailureRollsBack 验证分组热更新成功但持久化失败时会恢复旧分组。
//
// 参数：
//   - t: *testing.T，Go 测试上下文；配置路径故意使用目录以稳定触发保存失败。
//
// 返回值：无。
//
// 错误情况：编辑未报错，或失败后内存中的端口、类型发生变化时测试失败。
func TestUpdateGroupPersistenceFailureRollsBack(t *testing.T) {
	cfg := &config.Config{
		ManualNodes: []string{"socks5://127.0.0.1:1080#manual"},
		Listen:      "127.0.0.1",
		PortRange:   [2]int{42000, 42010},
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
		Groups: []config.NodeGroup{{
			Name: "media", Port: 43000, Type: config.GroupTypeFallback, Subscription: "manual",
		}},
	}
	application, err := New(cfg, cfg.StateDir)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(application.Shutdown)

	err = application.UpdateGroup("media", config.NodeGroup{
		Name: "media", Port: 43001, Type: config.GroupTypeLoadBalance, Subscription: "manual",
	})
	if err == nil {
		t.Fatal("配置路径是目录时编辑分组应返回持久化错误")
	}
	groups := application.Groups()
	if len(groups) != 1 || groups[0].Port != 43000 || groups[0].Type != config.GroupTypeFallback {
		t.Fatalf("持久化失败后分组未恢复: %+v", groups)
	}
}
