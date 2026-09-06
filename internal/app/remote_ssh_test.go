package app

// 本文件验证 SSH 授权配置事务的并发合并、持久化与失败回滚。

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"proxyd/internal/config"
)

// appSSHTestPublic 生成只用于配置事务测试的独立公钥。
// 参数说明：t 为 *testing.T，用于报告密码学随机数错误。
// 返回值说明：string，OpenSSH 公钥行。
// 错误情况：密钥生成或编码失败时终止测试。
func appSSHTestPublic(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return string(ssh.MarshalAuthorizedKey(key))
}

// TestRemoteSSHKeyTransactions 验证并发添加不会丢失授权，最后一项撤销不会放开登录。
// 参数说明：t 为 *testing.T，提供独立状态与配置文件。
// 返回值说明：无。
// 错误情况：并发写丢失、持久化不一致、名称歧义未拒绝或删除后认证关闭时失败。
func TestRemoteSSHKeyTransactions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// 使用真实配置解析器填充默认端口等字段，确保保存后的文件能完整重载。
	cfg, err := config.Parse([]byte("manual-nodes:\n  - http://127.0.0.1:18888#test\nport-range: [19001, 19010]\nrules:\n  - MATCH,PROXY\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateDir = dir
	a, err := New(cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopRemote()
	keys := []string{appSSHTestPublic(t), appSSHTestPublic(t)}
	var wg sync.WaitGroup
	errors := make(chan error, len(keys))
	for _, public := range keys {
		wg.Go(func() { errors <- a.AddRemoteSSHKeys(public, "same-name") })
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := a.SetRemoteSSHAuth(true); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteRemoteSSHKey("same-name"); err == nil {
		t.Fatal("同名公钥应要求明确指纹")
	}
	snapshot := a.RemoteStatus()
	if len(snapshot.SSHKeys) != 2 {
		t.Fatal("并发添加丢失公钥")
	}
	for _, key := range snapshot.SSHKeys {
		if err := a.DeleteRemoteSSHKey(key.Fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Remote.SSHKeys) != 0 || !loaded.Remote.SSHAuthRequired || !a.RemoteStatus().SSHAuthRequired {
		t.Fatal("删除全部公钥后必须持久化拒绝全部的认证状态")
	}
}

// TestRemoteSSHKeyRollback 验证落盘失败不会留下已应用但未保存的授权集合。
// 参数说明：t 为 *testing.T，使用普通文件占据配置父路径来稳定制造失败。
// 返回值说明：无。
// 错误情况：错误被忽略、内存或 Manager 保留失败操作时测试失败。
func TestRemoteSSHKeyRollback(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(&config.Config{StateDir: dir}, filepath.Join(blocked, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopRemote()
	if err := a.AddRemoteSSHKeys(appSSHTestPublic(t), "laptop"); err == nil {
		t.Fatal("预期配置落盘失败")
	}
	if len(a.Config().Remote.SSHKeys) != 0 || len(a.RemoteStatus().SSHKeys) != 0 {
		t.Fatal("授权添加失败未回滚")
	}
	if err := a.SetRemoteSSHAuth(true); err == nil {
		t.Fatal("预期认证开关落盘失败")
	}
	if a.Config().Remote.SSHAuthRequired || a.RemoteStatus().SSHAuthRequired {
		t.Fatal("认证开关失败未回滚")
	}
}

// TestRemoteSSHPolicyRollback 验证热更新落盘失败后恢复原始启用状态和到期时间。
// 参数说明：t 为 *testing.T；返回值说明：无。
// 错误情况：配置或实时授权在失败事务后保留新值时失败。
func TestRemoteSSHPolicyRollback(t *testing.T) {
	dir := t.TempDir()
	a, err := New(&config.Config{StateDir: dir}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopRemote()
	if err := a.AddRemoteSSHKeys(appSSHTestPublic(t), "laptop"); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	a.cfgPath = filepath.Join(blocked, "config.yaml")
	disabled, expiry := true, "2020-01-01T00:00:00Z"
	if err := a.UpdateRemoteSSHKey("laptop", &disabled, &expiry); err == nil {
		t.Fatal("应报告落盘失败")
	}
	entry := a.RemoteStatus().SSHKeys[0]
	if entry.Disabled || entry.ExpiresAt != nil || a.Config().Remote.SSHKeys[0].Disabled {
		t.Fatal("失败的热更新没有整体回滚")
	}
}
