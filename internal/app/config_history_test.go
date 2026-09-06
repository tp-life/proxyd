package app

// 配置恢复测试覆盖预检冲突、恢复前备份、待重启保护与历史写入失败时的主配置完整性。
import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"proxyd/internal/config"
	"proxyd/internal/infrastructure/confighistory"
	"testing"
)

// TestHistorySkipsUnchangedSaveAndRestart 验证首次格式化后重复保存和重启不增加历史。
// 参数：t 为 *testing.T；返回无；重复构造实例后历史继续增长或首次原文快照丢失时失败，状态目录完全隔离。
func TestHistorySkipsUnchangedSaveAndRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := []byte(fmt.Sprintf("# 保留用户注释\nstate-dir: %q\nproxy-disabled: true\n", dir))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		a, err := New(cfg, path)
		if err != nil {
			t.Fatal(err)
		}
		a.mu.Lock()
		err = a.persistLocked()
		a.mu.Unlock()
		a.Shutdown()
		if err != nil {
			t.Fatal(err)
		}
		// 原始简写配置首次保存会补齐默认值并改变 MD5；后续三轮持久化只保留该回滚点。
		versions, err := a.ConfigHistory()
		if err != nil || len(versions) != 1 {
			t.Fatalf("首次规范化后保存/重启继续生成历史: count=%d err=%v", len(versions), err)
		}
		original, err := a.configHistory.Read(versions[0].ID)
		if err != nil || !bytes.Equal(original, raw) {
			t.Fatal("首次规范化丢失了原始配置回滚点")
		}
	}
}

// TestHistorySkipsConsecutiveSnapshotButKeepsRealTransitions 验证连续快照去重及 A→B→A 的回滚点。
// 参数：t 为 *testing.T；返回无；重复归档或密码变更丢失时失败，不比较脱敏副本。
func TestHistorySkipsConsecutiveSnapshotButKeepsRealTransitions(t *testing.T) {
	a := newSystemProxyTestApp(t, filepath.Join(t.TempDir(), "config.yaml"))
	a.cfg.ProxyDisabled = true
	a.cfg.APISecret = "history-secret-a"
	if err := a.cfg.Save(a.cfgPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(a.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟归档成功、主文件保存失败后重试；最新历史已包含当前磁盘配置。
	if _, err = a.configHistory.Archive(before, nil, "设置变更前", []string{"api-secret"}); err != nil {
		t.Fatal(err)
	}
	for index, secret := range []string{"history-secret-b", "history-secret-a", "history-secret-b"} {
		a.mu.Lock()
		a.cfg.APISecret = secret
		err = a.persistLocked()
		a.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		versions, err := a.ConfigHistory()
		if err != nil || len(versions) != index+1 {
			t.Fatalf("连续去重或真实变更记录异常: count=%d want=%d err=%v", len(versions), index+1, err)
		}
	}
}

// TestConfigHistoryRestoreRequiresFreshPreview 验证恢复绑定磁盘版本；参数 t 为测试对象；无返回，误覆盖或运行态被提前替换时失败。
func TestConfigHistoryRestoreRequiresFreshPreview(t *testing.T) {
	a := newSystemProxyTestApp(t, filepath.Join(t.TempDir(), "config.yaml"))
	a.cfg.ProxyDisabled = true
	a.cfg.APISecret = "first-secret"
	if err := a.cfg.Save(a.cfgPath); err != nil {
		t.Fatal(err)
	}
	a.cfg.APISecret = "second-secret"
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	versions, err := a.ConfigHistory()
	if err != nil || len(versions) != 1 {
		t.Fatalf("未生成历史: %v", err)
	}
	preview, err := a.PreviewConfigRestore(versions[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.APISecret = "third-secret"
	a.mu.Lock()
	err = a.persistLocked()
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.RestoreConfigVersion(preview.ID, preview.Digest, preview.BaseDigest); err == nil {
		t.Fatal("旧预检覆盖了新配置")
	}
	preview, err = a.PreviewConfigRestore(versions[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.RestoreConfigVersion(preview.ID, preview.Digest, preview.BaseDigest); err != nil {
		t.Fatal(err)
	}
	restored, err := config.Load(a.cfgPath)
	if err != nil || restored.APISecret != "first-secret" {
		t.Fatalf("磁盘未恢复: %v", err)
	}
	if a.Config().APISecret != "third-secret" || !a.ConfigRestartPending() {
		t.Fatal("待重启配置错误地进入了运行态")
	}
	a.mu.Lock()
	err = a.persistLocked()
	a.mu.Unlock()
	if err == nil {
		t.Fatal("旧运行配置覆盖了待重启配置")
	}
	exported, err := a.ExportConfigHistory(preview.ID)
	if err != nil || bytes.Contains(exported, []byte("first-secret")) {
		t.Fatal("历史导出泄露凭据")
	}
}

// TestHistoryFailurePreservesMainConfig 验证归档失败时停止主配置写入；参数 t 为测试对象；无返回，原文件被覆盖时失败。
func TestHistoryFailurePreservesMainConfig(t *testing.T) {
	a := newSystemProxyTestApp(t, filepath.Join(t.TempDir(), "config.yaml"))
	a.cfg.ProxyDisabled = true
	if err := a.cfg.Save(a.cfgPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(a.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err = os.WriteFile(blocked, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	a.configHistory = confighistory.New(blocked)
	a.cfg.APISecret = "new-secret"
	a.mu.Lock()
	err = a.persistLocked()
	a.mu.Unlock()
	if err == nil {
		t.Fatal("归档失败未阻止保存")
	}
	after, err := os.ReadFile(a.cfgPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("主配置被破坏")
	}
}

// TestInvalidHistoryNeverExportsUnvalidatedFields 验证旧配置的非法凭据字段仅进入加密快照。
// 参数 t 为测试对象；无返回，导出或摘要泄露未校验内容时失败，修复配置仍应正常保存。
func TestInvalidHistoryNeverExportsUnvalidatedFields(t *testing.T) {
	a := newSystemProxyTestApp(t, filepath.Join(t.TempDir(), "config.yaml"))
	a.cfg.ProxyDisabled = true
	raw := []byte("proxy-disabled: true\nsecret-accidentally-used-as-field: value\nremote:\n  ssh-keys:\n    - name: invalid\n      public-key: '-----BEGIN OPENSSH PRIVATE KEY-----'\n")
	if err := os.WriteFile(a.cfgPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	versions, err := a.ConfigHistory()
	if err != nil || len(versions) != 1 {
		t.Fatal("无法归档旧配置")
	}
	exported, err := a.ExportConfigHistory(versions[0].ID)
	if err != nil || bytes.Contains(exported, []byte("PRIVATE KEY")) {
		t.Fatal("未校验字段进入公开导出")
	}
	for _, section := range versions[0].Sections {
		if section == "secret-accidentally-used-as-field" {
			t.Fatal("未知配置键进入公开摘要")
		}
	}
	if _, err = a.PreviewConfigRestore(versions[0].ID); err == nil {
		t.Fatal("非法历史通过恢复校验")
	}
}
