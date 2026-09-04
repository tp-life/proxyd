package remote

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// dialTimeout 是隧道拨号的默认超时（含 DERP 连接与 WireGuard 握手）。
const dialTimeout = 30 * time.Second

// ValidateToken 校验一个 tc... 连接 token 的格式（仅本地解析，不发起网络连接）。
func ValidateToken(token string) error {
	if _, err := tailcat.ParseConnBlob(tailcat.ConnBlob(token)); err != nil {
		return fmt.Errorf("token 无效: %w", err)
	}
	return nil
}

// Dial 拨号远端隧道端口，返回的 conn 关闭时自动回收隧道资源。
// clientKey 为本机客户端身份密钥（对端 --allow 白名单按它的公钥放行）；
// 传零值则每次连接生成临时身份，无法被白名单识别。
// 供一次性命令（pipe/ssh）使用；常驻转发请走 Manager 的 forwardRunner（复用隧道）。
func Dial(ctx context.Context, token string, port int, clientKey key.NodePrivate) (net.Conn, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("端口 %d 超出 1-65535", port)
	}
	cl := tailcat.NewClient(tailcat.ConnBlob(token))
	cl.Logf = logger.Discard
	if !clientKey.IsZero() {
		cl.Key = clientKey
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	c, err := cl.DialTCPPort(ctx, uint16(port))
	if err != nil {
		_ = cl.Close()
		return nil, err
	}
	return &clientConn{Conn: c, cl: cl}, nil
}

// clientConn 在底层连接关闭时连带关闭临时隧道客户端。
type clientConn struct {
	net.Conn
	cl *tailcat.Client
}

// Close 关闭连接并回收隧道客户端。
func (c *clientConn) Close() error {
	err := c.Conn.Close()
	_ = c.cl.Close()
	return err
}

// Pipe 把 stdin/stdout 与远端隧道端口双向管道化（OpenSSH ProxyCommand 用法）。
//
// 参数说明：
//   - ctx: context.Context，取消后主动关闭隧道和 stdin，解阻塞双向拷贝。
//   - token: string，tailcat 远端连接 token。
//   - port: int，要连接的远端 TCP 端口。
//   - clientKey: key.NodePrivate，客户端白名单身份；零值使用临时身份。
//   - stdin: io.ReadCloser，输入所有权在 Pipe 运行期间交给本函数，收尾时会关闭。
//   - stdout: io.Writer，接收远端返回数据。
//
// 返回值说明：error，拨号失败或远端读取侧出错时返回。
//
// 错误情况：对端关闭或 ctx 取消会立即关闭 stdin；这是为了解开
// 可能正阻塞在终端读取上的拷贝协程，否则远端先断开时 CLI 会永久卡在收尾。
func Pipe(ctx context.Context, token string, port int, clientKey key.NodePrivate, stdin io.ReadCloser, stdout io.Writer) error {
	conn, err := Dial(ctx, token, port, clientKey)
	if err != nil {
		return err
	}
	return pipeConnection(ctx, conn, stdin, stdout)
}

// pipeConnection 在已建立的连接上执行可取消的双向拷贝。
//
// 参数说明：
//   - ctx: context.Context，用于中断两个拷贝方向。
//   - conn: net.Conn，已建立的远端连接，函数返回前会关闭。
//   - stdin: io.ReadCloser，可关闭输入，用于解阻塞读操作。
//   - stdout: io.Writer，远端数据的输出目标。
//
// 返回值说明：error，返回远端到 stdout 方向的非 EOF 拷贝错误。
//
// 错误情况：输入方向在收尾时因 Close 返回的错误不覆盖主读方向结果；
// 函数会等待输入拷贝退出，保证没有持有 conn/stdin 的残留协程。
func pipeConnection(ctx context.Context, conn net.Conn, stdin io.ReadCloser, stdout io.Writer) error {
	defer conn.Close()
	defer stdin.Close()

	copyDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, stdin)
		copyDone <- err
	}()
	wakeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
			_ = stdin.Close()
		case <-wakeDone:
		}
	}()
	_, readErr := io.Copy(stdout, conn)
	// 远端读方向结束时同时关闭连接和输入：只关闭 conn 不能
	// 唤醒正阻塞在 os.Stdin.Read 上的协程。
	_ = conn.Close()
	_ = stdin.Close()
	<-copyDone
	close(wakeDone)
	if readErr != nil {
		return readErr
	}
	return nil
}

// Ping 测量到远端隧道的握手往返延迟（经 DERP 中继）。clientKey 语义同 Dial。
func Ping(ctx context.Context, token string, clientKey key.NodePrivate) (time.Duration, error) {
	cl := tailcat.NewClient(tailcat.ConnBlob(token))
	cl.Logf = logger.Discard
	if !clientKey.IsZero() {
		cl.Key = clientKey
	}
	defer cl.Close()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	res, err := cl.Ping(ctx)
	if err != nil {
		return 0, err
	}
	return res.Latency, nil
}
