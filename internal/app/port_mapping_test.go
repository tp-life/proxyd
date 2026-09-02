package app

import (
	"testing"
	"time"

	"proxyd/internal/config"
)

// TestSetPortMappingRollsBackWhenPersistenceFails 验证端口映射开关采用运行态与配置文件
// 一致提交语义：新运行态已经生成、但磁盘保存失败时，必须把内存配置和 mihomo 运行态
// 一并恢复，不能让 API 报错后实际仍保持新开关状态。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，负责创建隔离状态目录并报告断言失败。
//
// 返回值：无。
//
// 错误情况：测试把 cfgPath 指向目录以稳定制造原子重命名失败；若方法未返回错误、
// 开关没有恢复为默认开启，或回滚过程中发生未处理异常，测试失败。
func TestSetPortMappingRollsBackWhenPersistenceFails(t *testing.T) {
	stateDir := t.TempDir()
	cfg := &config.Config{
		Listen:        "127.0.0.1",
		Mode:          "rule",
		LogLevel:      "silent",
		StateDir:      stateDir,
		HealthURL:     "http://www.gstatic.com/generate_204",
		HealthTimeout: config.Duration(3 * time.Second),
		Rules:         []string{"MATCH,PROXY"},
	}
	a, err := New(cfg, stateDir)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.runner.Shutdown)

	if err := a.SetPortMapping(false); err == nil {
		t.Fatal("配置路径是目录时切换端口映射应返回持久化错误")
	}
	if !a.Config().PortMappingEnabled() {
		t.Fatal("持久化失败后端口映射开关未恢复为原状态")
	}
}
