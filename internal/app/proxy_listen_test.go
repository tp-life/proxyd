package app

// 代理域：局域网共享开关（listen 热切换）事务语义测试。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"proxyd/internal/config"
)

// newLANShareTestApp 构造最小可用应用：仅提供热更新所需的配置骨架。
func newLANShareTestApp(t *testing.T, listen, cfgPath string) *App {
	t.Helper()
	stateDir := t.TempDir()
	cfg := &config.Config{
		Listen:        listen,
		Mode:          "rule",
		LogLevel:      "silent",
		StateDir:      stateDir,
		HealthURL:     "http://www.gstatic.com/generate_204",
		HealthTimeout: config.Duration(3 * time.Second),
		Rules:         []string{"MATCH,PROXY"},
	}
	a, err := New(cfg, cfgPath)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.runner.Shutdown)
	return a
}

// TestSetLANShareToggleAndPersist 验证开关在 127.0.0.1 ↔ 0.0.0.0 间热切换，
// 且开启结果写入配置文件（listen 值正确落盘）。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，负责创建临时配置路径并报告断言失败。
//
// 返回值：无。
//
// 错误情况：切换未生效、落盘文件不含新 listen，或关闭后未恢复 127.0.0.1 时测试失败。
func TestSetLANShareToggleAndPersist(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("rules:\n  - MATCH,PROXY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newLANShareTestApp(t, "127.0.0.1", cfgPath)

	if err := a.SetLANShare(true); err != nil {
		t.Fatalf("开启局域网共享失败: %v", err)
	}
	if got := a.Config().Listen; got != "0.0.0.0" {
		t.Fatalf("开启后 listen = %q, want 0.0.0.0", got)
	}
	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "listen: 0.0.0.0") {
		t.Errorf("配置文件未写入新 listen:\n%s", saved)
	}
	if enabled, listen := a.LANShare(); !enabled || listen != "0.0.0.0" {
		t.Errorf("LANShare() = (%v, %q), want (true, 0.0.0.0)", enabled, listen)
	}

	if err := a.SetLANShare(false); err != nil {
		t.Fatalf("关闭局域网共享失败: %v", err)
	}
	if got := a.Config().Listen; got != "127.0.0.1" {
		t.Errorf("关闭后 listen = %q, want 127.0.0.1", got)
	}
}

// TestSetLANShareCustomAddress 验证自定义非回环地址的语义：开启为幂等 no-op
// （保留用户地址），关闭统一重置回 127.0.0.1。
func TestSetLANShareCustomAddress(t *testing.T) {
	a := newLANShareTestApp(t, "192.168.1.10", "")

	if err := a.SetLANShare(true); err != nil {
		t.Fatalf("自定义地址下开启应成功（no-op）: %v", err)
	}
	if got := a.Config().Listen; got != "192.168.1.10" {
		t.Errorf("开启不应覆盖自定义地址, listen = %q", got)
	}
	if enabled, _ := a.LANShare(); !enabled {
		t.Error("自定义非回环地址应报告共享已开启")
	}

	if err := a.SetLANShare(false); err != nil {
		t.Fatalf("自定义地址下关闭失败: %v", err)
	}
	if got := a.Config().Listen; got != "127.0.0.1" {
		t.Errorf("关闭应重置回 127.0.0.1, listen = %q", got)
	}
}

// TestSetLANShareRollsBackWhenPersistenceFails 验证运行态已生成但磁盘保存失败时，
// 内存配置与 mihomo 运行态一并恢复为旧 listen，不能让 API 报错后实际仍保持新绑址。
func TestSetLANShareRollsBackWhenPersistenceFails(t *testing.T) {
	a := newLANShareTestApp(t, "127.0.0.1", t.TempDir()) // 配置路径指向目录：原子重命名必失败

	if err := a.SetLANShare(true); err == nil {
		t.Fatal("配置路径是目录时开启局域网共享应返回持久化错误")
	}
	if got := a.Config().Listen; got != "127.0.0.1" {
		t.Errorf("持久化失败后 listen 未恢复, got %q", got)
	}
	if enabled, _ := a.LANShare(); enabled {
		t.Error("持久化失败后共享开关应恢复为关闭")
	}
}
