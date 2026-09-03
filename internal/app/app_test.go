package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"proxyd/internal/config"
)

// TestMigrationWriteBack 验证启动时执行过兼容迁移后，迁移结果被一次性写回配置文件：
// 旧默认 health-url 在磁盘上变为 HTTPS，再次加载不再触发迁移。
func TestMigrationWriteBack(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := `
subscriptions:
  - name: a
    url: https://example.com/sub
port-range: [42000, 42010]
health-url: http://www.gstatic.com/generate_204
rules:
  - MATCH,PROXY
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.MigrationApplied() {
		t.Fatal("旧默认 health-url 应触发迁移")
	}
	a, err := New(cfg, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopRemote()

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "health-url: https://www.gstatic.com/generate_204") {
		t.Fatalf("迁移结果未写回配置文件:\n%s", data)
	}

	// 再次加载：磁盘已是新值，不应再标记迁移。
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	if reloaded.MigrationApplied() {
		t.Fatal("写回后再次加载不应触发迁移")
	}
}

// TestNoMigrationNoWriteBack 验证未发生迁移时启动不会改写配置文件（保留注释等内容）。
func TestNoMigrationNoWriteBack(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := `# 用户注释
subscriptions:
  - name: a
    url: https://example.com/sub
port-range: [42000, 42010]
rules:
  - MATCH,PROXY
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MigrationApplied() {
		t.Fatal("新配置不应触发迁移")
	}
	a, err := New(cfg, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopRemote()

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Fatal("未迁移时不应改写配置文件")
	}
}
