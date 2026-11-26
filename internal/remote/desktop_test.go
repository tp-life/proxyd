package remote

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"tailscale.com/types/key"
)

// TestTransientForwardLifecycle 验证临时转发能够传输数据并在关闭后释放监听端口。
//
// 参数说明：
//   - t: *testing.T，Go 单元测试上下文。
//
// 返回值说明：无；echo 数据往返、幂等关闭和端口释放全部成立时测试成功。
//
// 错误情况：监听、拨号、数据复制或关闭生命周期不符合预期时测试失败。
func TestTransientForwardLifecycle(t *testing.T) {
	echoAddress := startEchoServer(t)
	forward, err := startTransientForward("127.0.0.1:0", "tc-test-only", 3389, key.NodePrivate{}, dialDirect(echoAddress))
	if err != nil {
		t.Fatalf("启动临时转发失败: %v", err)
	}
	address := forward.Address()

	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("连接临时转发失败: %v", err)
	}
	message := []byte("desktop-forward")
	if _, err := connection.Write(message); err != nil {
		t.Fatalf("写入临时转发失败: %v", err)
	}
	buffer := make([]byte, len(message))
	if _, err := io.ReadFull(connection, buffer); err != nil {
		t.Fatalf("读取临时转发响应失败: %v", err)
	}
	if string(buffer) != string(message) {
		t.Fatalf("临时转发响应 = %q，期望 %q", buffer, message)
	}
	_ = connection.Close()

	if err := forward.Close(); err != nil {
		t.Fatalf("关闭临时转发失败: %v", err)
	}
	if err := forward.Close(); err != nil {
		t.Fatalf("重复关闭临时转发失败: %v", err)
	}
	if connection, err := net.Dial("tcp", address); err == nil {
		_ = connection.Close()
		t.Fatalf("临时转发关闭后地址 %s 仍可连接", address)
	}
}

// TestTransientForwardCloseReleasesActiveConnections 验证停止临时转发会主动
// 中断已建立的本地连接，而不是只关闭 listener 并等待客户端自行退出。
//
// 参数说明：
//   - t: *testing.T，Go 单元测试上下文。
//
// 返回值说明：无；活跃连接被关闭且计数回到零时测试成功。
//
// 错误情况：转发启动、数据往返、关闭通知或连接计数收敛超时时测试失败。
func TestTransientForwardCloseReleasesActiveConnections(t *testing.T) {
	echoAddress := startEchoServer(t)
	forward, err := startTransientForward("127.0.0.1:0", "tc-test-only", 3389, key.NodePrivate{}, dialDirect(echoAddress))
	if err != nil {
		t.Fatalf("启动临时转发失败: %v", err)
	}
	t.Cleanup(func() { _ = forward.Close() })

	connection, err := net.Dial("tcp", forward.Address())
	if err != nil {
		t.Fatalf("连接临时转发失败: %v", err)
	}
	defer connection.Close()
	message := []byte("keep-active")
	if _, err := connection.Write(message); err != nil {
		t.Fatalf("写入临时转发失败: %v", err)
	}
	buffer := make([]byte, len(message))
	if _, err := io.ReadFull(connection, buffer); err != nil {
		t.Fatalf("读取临时转发响应失败: %v", err)
	}
	if forward.ActiveConnections() != 1 {
		t.Fatalf("关闭前活跃连接数 = %d，期望 1", forward.ActiveConnections())
	}

	if err := forward.Close(); err != nil {
		t.Fatalf("关闭临时转发失败: %v", err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("设置关闭检测超时失败: %v", err)
	}
	if _, err := connection.Read(make([]byte, 1)); err == nil {
		t.Fatal("临时转发关闭后，已建立连接仍然可读")
	}

	deadline := time.Now().Add(time.Second)
	for forward.ActiveConnections() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if active := forward.ActiveConnections(); active != 0 {
		t.Fatalf("临时转发关闭后活跃连接数 = %d，期望 0", active)
	}
}

// TestUDPForwardRunnerEcho 验证客户端 UDP 转发器能双向保留数据报边界并在 stop 后释放资源。
func TestUDPForwardRunnerEcho(t *testing.T) {
	echoLn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("监听 UDP echo 失败: %v", err)
	}
	defer echoLn.Close()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := echoLn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echoLn.WriteToUDP(buf[:n], addr)
		}
	}()

	runner, err := startUDPForwardRunner("127.0.0.1:0", func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", echoLn.LocalAddr().String())
	}, nil)
	if err != nil {
		t.Fatalf("启动 UDP 转发器失败: %v", err)
	}
	defer runner.stop()

	client, err := net.Dial("udp4", runner.localConn.LocalAddr().String())
	if err != nil {
		t.Fatalf("连接本地 UDP 转发失败: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write([]byte("rdp-udp-frame")); err != nil {
		t.Fatalf("写入 UDP 数据报失败: %v", err)
	}
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil || string(buf[:n]) != "rdp-udp-frame" {
		t.Fatalf("UDP 回显不匹配: %q err=%v", string(buf[:n]), err)
	}
	runner.stop()
	runner.stop()
}
