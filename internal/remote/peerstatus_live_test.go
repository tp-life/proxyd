package remote

// 本文件是升级 tailcat 时使用的联网冒烟测试，默认跳过。
//
// proxyd 依赖的上游契约：客户端完成一次握手后，Server.Status().Peer 会包含该客户端
// （含 LastHandshake 与 WireGuard 收发计数）。tailcat v0.7.0 才打开 StatusBuilder 的
// WantPeers，更早版本该集合恒为空；上游若回退，入站客户端观测会静默变空，而纯映射
// 单元测试无法发现，因此保留这条需要真实公共 DERP 的检查。
//
// 运行方式：PROXYD_REMOTE_LIVE=1 go test -run TestLiveServerStatusPeer ./internal/remote

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

// TestLiveServerStatusPeer 验证真实握手后 tailcat 状态里出现该客户端及其计数。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；客户端出现在 Status().Peer 且带握手时间与收发计数时通过。
//
// 错误情况：未设置 PROXYD_REMOTE_LIVE=1 时跳过；网络/DERP 不可达、隧道连接失败，
// 或 15 秒内状态仍未包含该客户端时失败。
func TestLiveServerStatusPeer(t *testing.T) {
	if os.Getenv("PROXYD_REMOTE_LIVE") != "1" {
		t.Skip("需要联网访问公共 DERP 中继；设置 PROXYD_REMOTE_LIVE=1 后运行")
	}

	srv := &tailcat.Server{
		Key:  tailcat.NewPrivateKey().Private,
		Logf: func(string, ...any) {},
		OnTCP: func(port uint16) func(net.Conn) {
			if port != 9 {
				return nil
			}
			// 收到连接即关闭：只要 Peer 出现并带计数，就证明状态采集没有回退。
			return func(conn net.Conn) { _ = conn.Close() }
		},
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("启动 tailcat 服务端失败（检查网络与 DERP 可达性）: %v", err)
	}
	defer srv.Close()

	clientKey := key.NewNode()
	client := tailcat.NewClient(srv.ConnBlob())
	client.Logf = func(string, ...any) {}
	client.Key = clientKey
	defer client.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	conn, err := client.DialTCPPort(ctx, 9)
	if err != nil {
		t.Fatalf("经隧道连接服务端失败: %v", err)
	}
	_ = conn.Close()

	want := clientKey.Public()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if peer := srv.Status().Peer[want]; peer != nil &&
			!peer.LastHandshake.IsZero() && (peer.RxBytes > 0 || peer.TxBytes > 0) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("15 秒内 Server.Status().Peer 未包含客户端 %s：tailcat 可能不再填充 StatusBuilder.WantPeers", want)
}
