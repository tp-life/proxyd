package remote

// 本文件验证 serve 语义下本机目标端口的回环兜底拨号。

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDialLoopbackPrefersIPv4 验证 IPv4 与 IPv6 回环都可用时优先连 127.0.0.1。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；成功连到监听器时测试通过。
//
// 错误情况：监听、拨号失败或实际连到的不是 127.0.0.1 时失败。
func TestDialLoopbackPrefersIPv4(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听 127.0.0.1 失败: %v", err)
	}
	defer ln.Close()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	conn, err := dialLoopback(port)
	if err != nil {
		t.Fatalf("dialLoopback: %v", err)
	}
	defer conn.Close()
	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || addr.IP.String() != "127.0.0.1" {
		t.Fatalf("应优先连接 127.0.0.1，got %v", conn.RemoteAddr())
	}
	select {
	case server := <-accepted:
		_ = server.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("监听器没有收到连接")
	}
}

// TestDialLoopbackFallsBackToIPv6 验证目标只监听 ::1 时仍能通过回退连上。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；连到 ::1 监听器时测试通过。
//
// 错误情况：环境不支持 IPv6 回环或同端口被 IPv4 服务占用时跳过；
// 回退失败或连到非 ::1 地址时失败。
func TestDialLoopbackFallsBackToIPv6(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("当前环境不支持 IPv6 回环: %v", err)
	}
	defer ln.Close()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)

	// 同一端口若恰好被本机 IPv4 服务占用，测试前提不成立，跳过而不是误报。
	if probe, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), time.Second); err == nil {
		_ = probe.Close()
		t.Skip("IPv4 回环上该端口已被占用")
	}
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	conn, err := dialLoopback(port)
	if err != nil {
		t.Fatalf("dialLoopback 回退 ::1 失败: %v", err)
	}
	defer conn.Close()
	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() || addr.IP.To4() != nil {
		t.Fatalf("应回退到 ::1，got %v", conn.RemoteAddr())
	}
}

// TestDialLoopbackReportsFailure 验证两个回环地址都不可用时返回错误，
// 且错误里保留 127.0.0.1 的失败原因，便于日志定位。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；拨号必然失败时测试通过。
//
// 错误情况：端口已释放却仍能连上（被其他进程抢占）时失败。
func TestDialLoopbackReportsFailure(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听 127.0.0.1 失败: %v", err)
	}
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	if err := ln.Close(); err != nil {
		t.Fatalf("关闭监听器失败: %v", err)
	}

	conn, err := dialLoopback(port)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("端口 %d 已释放，不应连接成功", port)
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("错误应包含 127.0.0.1 的失败原因: %v", err)
	}
}
