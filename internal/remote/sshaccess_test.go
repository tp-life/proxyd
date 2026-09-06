//go:build linux || darwin || windows

package remote

// 本文件用真实 SSH 握手验证策略热更新、过期、审计及显式会话撤销，无需真实 DERP 服务。

import (
	"context"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"proxyd/internal/config"
)

// TestSSHAccessHotUpdate 验证同一处理器在授权变化后立即改变新认证，同时保留原连接。
// 参数说明：t 为 *testing.T，提供临时 host key 与测试超时。
// 返回值说明：无。
// 错误情况：策略更新需要重建处理器、撤销后还能认证、已有连接被隐式关闭或断开失败时测试失败。
func TestSSHAccessHotUpdate(t *testing.T) {
	signer, public := newSSHTestSigner(t)
	key := config.RemoteSSHKey{Name: "laptop", PublicKey: public}
	audit := newAuditLog(50)
	policy := newSSHAccess(audit)
	policy.update(true, []config.RemoteSSHKey{key})
	handler, err := managedShellSSHHandler(t.TempDir(), policy)
	if err != nil {
		t.Fatal(err)
	}
	// 每次使用同一 handler 建立新的传输，确保测试的确覆盖运行中策略读取。
	connect := func(want bool) *ssh.Client {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		conn, err := openAuthenticatedLoopback(ctx, handler)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		cc, ch, r, err := ssh.NewClientConn(conn, "test", &ssh.ClientConfig{User: "test", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
		if err != nil {
			conn.Close()
			if want {
				t.Fatal(err)
			}
			return nil
		}
		if !want {
			cc.Close()
			t.Fatal("失效公钥仍获准认证")
		}
		_ = conn.SetDeadline(time.Time{})
		client := ssh.NewClient(cc, ch, r)
		t.Cleanup(func() { client.Close() })
		return client
	}
	client := connect(true)
	info := policy.infos([]config.RemoteSSHKey{key})[0]
	if info.LastUsedAt == nil || info.Active != 1 {
		t.Fatal("已验证的签名未记录成功认证")
	}
	key.Disabled = true
	policy.update(true, []config.RemoteSSHKey{key})
	connect(false)
	if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("禁用公钥影响已有传输: %v", err)
	}
	if policy.disconnect(info.Fingerprint) != 1 {
		t.Fatal("未定位已认证连接")
	}
	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("显式断开未生效")
	}
	key.Disabled = false
	expired := time.Now().Add(-time.Second)
	key.ExpiresAt = &expired
	normalized, err := NormalizeSSHKeys([]config.RemoteSSHKey{key})
	if err != nil {
		t.Fatal(err)
	}
	policy.update(true, normalized)
	connect(false)
	normalized[0].ExpiresAt = nil
	policy.update(true, normalized)
	connect(true)
	policy.update(true, nil)
	connect(false)
	success := 0
	for _, entry := range audit.Tail(50) {
		if entry.Action == "ssh_authenticated" {
			success++
			if entry.SSHFingerprint != info.Fingerprint {
				t.Fatal("成功认证缺少指纹")
			}
		}
	}
	if success != 2 {
		t.Fatalf("认证探测或失败被当作成功: %d", success)
	}
}

// TestSSHPolicyDoesNotRebuildTunnel 验证 SSH 策略不参与 tailcat 生命周期等价判断。
// 参数说明：t 为 *testing.T；返回值说明：无。
// 错误情况：更新认证方式或公钥引发隧道重建、token 变化时测试失败。
func TestSSHPolicyDoesNotRebuildTunnel(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	cfg := config.RemoteConfig{SSHAuthRequired: true, SSHKeys: []config.RemoteSSHKey{{Name: "new"}}}
	if !m.serverConfigEqual(cfg) {
		t.Fatal("SSH 策略变化不应重建隧道")
	}
}

// TestSSHKeyCloneAndExpiry 验证到期时间不会跨配置快照共享，边界时刻立即失效。
// 参数说明：t 为 *testing.T；返回值说明：无。
// 错误情况：Clone 修改污染原值或时间边界错误时失败。
func TestSSHKeyCloneAndExpiry(t *testing.T) {
	now := time.Now()
	cfg := config.RemoteConfig{SSHKeys: []config.RemoteSSHKey{{ExpiresAt: &now, Disabled: true}}}
	cloned := cfg.Clone()
	*cloned.SSHKeys[0].ExpiresAt = now.Add(time.Hour)
	if !cfg.SSHKeys[0].ExpiresAt.Equal(now) || !cfg.SSHKeys[0].IsExpired(now) || cfg.SSHKeys[0].IsExpired(now.Add(-time.Nanosecond)) {
		t.Fatal("克隆或过期边界不正确")
	}
}

// falseProofSigner 声明一把授权公钥，但用另一把私钥签名，模拟未持有私钥的探测者。
type falseProofSigner struct {
	ssh.Signer
	public ssh.PublicKey
}

// PublicKey 返回探测阶段声明的授权公钥。
// 参数说明：无；返回值说明：ssh.PublicKey；错误情况：无，仅用于验证签名证明边界。
func (s falseProofSigner) PublicKey() ssh.PublicKey { return s.public }

// TestSSHKeyProbeDoesNotCountAsLogin 验证公钥探测获准但签名无效时不更新登录记录。
// 参数说明：t 为 *testing.T；返回值说明：无。
// 错误情况：尚未验证私钥持有权就写入最近登录时间或成功审计时失败。
func TestSSHKeyProbeDoesNotCountAsLogin(t *testing.T) {
	allowed, public := newSSHTestSigner(t)
	other, _ := newSSHTestSigner(t)
	entries := []config.RemoteSSHKey{{PublicKey: public}}
	policy := newSSHAccess(newAuditLog(20))
	policy.update(true, entries)
	handler, err := managedShellSSHHandler(t.TempDir(), policy)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, err := openAuthenticatedLoopback(ctx, handler)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	client, _, _, err := ssh.NewClientConn(conn, "proof-test", &ssh.ClientConfig{User: "test", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Auth: []ssh.AuthMethod{ssh.PublicKeys(falseProofSigner{Signer: other, public: allowed.PublicKey()})}})
	if err == nil {
		client.Close()
		t.Fatal("未持有私钥却通过签名验证")
	}
	info := policy.infos(entries)[0]
	if info.LastUsedAt != nil || info.Active != 0 {
		t.Fatal("仅公钥探测就记录成登录")
	}
	for _, event := range policy.audit.Tail(20) {
		if event.Action == "ssh_authenticated" {
			t.Fatal("错误记录成功审计")
		}
	}
}
