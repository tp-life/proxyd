//go:build linux || darwin || windows

package remote

// 本文件通过真实 SSH 握手验证附加公钥认证，不启动 shell，也不访问 DERP 网络。

import (
	"context"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"proxyd/internal/config"
)

// TestSSHKeyAuthentication 验证正确私钥、错误私钥、无密钥、撤销全部与旧免密模式。
// 参数说明：t 为 *testing.T，负责临时 host key 与五秒握手边界。
// 返回值说明：无。
// 错误情况：认证结果与策略不一致时失败，尤其禁止空授权集合回退为免认证。
func TestSSHKeyAuthentication(t *testing.T) {
	allowed, public := newSSHTestSigner(t)
	unknown, _ := newSSHTestSigner(t)
	entries := []config.RemoteSSHKey{{Name: "laptop", PublicKey: public}}
	for _, tc := range []struct {
		name     string
		required bool
		keys     []config.RemoteSSHKey
		signer   ssh.Signer
		want     bool
	}{
		{"legacy", false, nil, nil, true},
		{"staged keys", false, entries, nil, true},
		{"allowed", true, entries, allowed, true},
		{"unknown", true, entries, unknown, false},
		{"no key", true, entries, nil, false},
		{"revoked all", true, nil, allowed, false},
		{"empty no key", true, nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := configuredShellSSHHandler(t.TempDir(), tc.required, tc.keys, "")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			conn, err := openAuthenticatedLoopback(ctx, handler)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			clientConfig := &ssh.ClientConfig{
				User: "test-client",
				// 一次性认证回环由本测试创建，主机密钥只用于加密此测试连接。
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			}
			if tc.signer != nil {
				clientConfig.Auth = []ssh.AuthMethod{ssh.PublicKeys(tc.signer)}
			}
			client, _, _, err := ssh.NewClientConn(conn, "ssh-key-test", clientConfig)
			if err == nil {
				_ = client.Close()
			}
			if (err == nil) != tc.want {
				t.Fatalf("认证结果不符合策略：success=%v，err=%v", err == nil, err)
			}
		})
	}
}
