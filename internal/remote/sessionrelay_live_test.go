package remote

// 会话中转的联网冒烟测试，默认跳过：真实 tailcat 服务端 + Manager 共享客户端池，
// 验证凭据拨号、字节往返与撤销断连。可用 PROXYD_REMOTE_DERP_MAP 指定自建中继地图。
//
// 运行方式：PROXYD_REMOTE_LIVE=1 go test -tags "with_gvisor ts_omit_acme" -run TestLiveSessionRelay ./internal/remote

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/tailscale/tailcat"

	"proxyd/internal/config"
)

// TestLiveSessionRelay 验证中转凭据经共享客户端拨通远端、回显往返，撤销后连接被关闭。
func TestLiveSessionRelay(t *testing.T) {
	if os.Getenv("PROXYD_REMOTE_LIVE") != "1" {
		t.Skip("需要联网访问 DERP 中继；设置 PROXYD_REMOTE_LIVE=1 后运行")
	}
	srv := &tailcat.Server{
		Key:        tailcat.NewPrivateKey().Private,
		Logf:       func(string, ...any) {},
		DERPMapURL: os.Getenv("PROXYD_REMOTE_DERP_MAP"),
		OnTCP: func(port uint16) func(net.Conn) {
			if port != 22 {
				return nil
			}
			return func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}
		},
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("启动 tailcat 服务端失败: %v", err)
	}
	defer srv.Close()

	m := NewManager(t.TempDir(), nil)
	defer m.Close()
	m.cfg.Remotes = []config.RemotePeer{{Name: "live", Token: string(srv.TailcatAddr())}}
	m.SetSessionRelayAddr("127.0.0.1:19091")
	env, revoke, err := m.issueSessionGrant()
	if err != nil {
		t.Fatal(err)
	}
	secret := grantSecret(t, env)

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	// 刚启动的服务端连上中继需数秒，而 tailcat 首次 meow 固定 10 秒超时（不随 ctx），
	// 冷启动时首拨可能超时：在总期限内重试，直到服务端就绪。
	var conn net.Conn
	for {
		conn, err = m.DialSessionRemote(ctx, secret, "live", 22)
		if err == nil || ctx.Err() != nil {
			break
		}
		t.Logf("拨号未就绪，重试: %v", err)
	}
	if err != nil {
		t.Fatalf("会话中转拨号失败: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("回显 %q err=%v", line, err)
	}

	revoke()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("撤销后中转连接应被关闭")
	}
}
