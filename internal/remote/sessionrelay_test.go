package remote

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"proxyd/internal/config"
)

// grantSecret 从签发的环境变量中取出凭据明文。
func grantSecret(t *testing.T, env []string) string {
	t.Helper()
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, SessionEnvSecret+"="); ok {
			return v
		}
	}
	t.Fatalf("环境变量缺少 %s: %q", SessionEnvSecret, env)
	return ""
}

// TestSessionGrantLifecycle 验证未配置地址不签发、通配地址改写为回环、凭据校验与撤销。
func TestSessionGrantLifecycle(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	env, revoke, err := m.issueSessionGrant()
	if err != nil || env != nil {
		t.Fatalf("未配置中转地址时不应签发: env=%q err=%v", env, err)
	}
	revoke()

	m.SetSessionRelayAddr("0.0.0.0:19091")
	m.cfg.Remotes = []config.RemotePeer{{Name: "mac", Token: "tcInvalidToken"}}
	env, revoke, err = m.issueSessionGrant()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(env, " "), SessionEnvAPI+"=127.0.0.1:19091") {
		t.Errorf("通配监听应改写为回环: %q", env)
	}
	secret := grantSecret(t, env)
	ctx := context.Background()

	if _, err := m.DialSessionRemote(ctx, "forged", "mac", 22); !errors.Is(err, ErrSessionGrantInvalid) {
		t.Errorf("伪造凭据应被拒: %v", err)
	}
	if _, err := m.DialSessionRemote(ctx, "", "mac", 22); !errors.Is(err, ErrSessionGrantInvalid) {
		t.Errorf("空凭据应被拒: %v", err)
	}
	if _, err := m.DialSessionRemote(ctx, secret, "tcAnyRawToken", 22); err == nil || errors.Is(err, ErrSessionGrantInvalid) {
		t.Errorf("裸 token 应以参数错误拒绝: %v", err)
	}
	if _, err := m.DialSessionRemote(ctx, secret, "nope", 22); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("未保存远端应报未找到: %v", err)
	}

	// 撤销会关闭经该授权登记的连接。
	a, b := net.Pipe()
	defer b.Close()
	m.relay.mu.Lock()
	for _, g := range m.relay.grants {
		g.conns[a] = struct{}{}
	}
	m.relay.mu.Unlock()
	revoke()
	revoke() // 幂等
	if _, err := a.Write([]byte("x")); err == nil {
		t.Error("撤销后登记的连接应已关闭")
	}
	if _, err := m.DialSessionRemote(ctx, secret, "mac", 22); !errors.Is(err, ErrSessionGrantInvalid) {
		t.Errorf("撤销后凭据应失效: %v", err)
	}
}
