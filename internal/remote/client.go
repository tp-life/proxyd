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
// clientKey 语义同 Dial。对端关闭或 ctx 取消时返回；读取侧出错（非 EOF）时返回该错误。
func Pipe(ctx context.Context, token string, port int, clientKey key.NodePrivate, stdin io.Reader, stdout io.Writer) error {
	conn, err := Dial(ctx, token, port, clientKey)
	if err != nil {
		return err
	}
	defer conn.Close()

	copyDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, stdin)
		copyDone <- err
	}()
	_, readErr := io.Copy(stdout, conn)
	// 任一方向结束即收尾：关闭连接触发另一方向退出。
	_ = conn.Close()
	<-copyDone
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
