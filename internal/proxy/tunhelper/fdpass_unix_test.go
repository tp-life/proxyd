//go:build darwin || linux

package tunhelper

// fd 回传机制单测：unix.Socketpair 上的 SCM_RIGHTS 收发回路。

import (
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// unixConnPair 创建一对 stream unix socket 连接（*net.UnixConn）。
func unixConnPair(t *testing.T) (client *net.UnixConn, server *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair: %v", err)
	}
	toConn := func(fd int) *net.UnixConn {
		file := os.NewFile(uintptr(fd), "socketpair")
		conn, err := net.FileConn(file)
		_ = file.Close()
		if err != nil {
			t.Fatalf("FileConn: %v", err)
		}
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			t.Fatalf("FileConn 类型 = %T", conn)
		}
		return unixConn
	}
	return toConn(fds[0]), toConn(fds[1])
}

func TestFDPassingRoundTrip(t *testing.T) {
	client, server := unixConnPair(t)
	defer client.Close()
	defer server.Close()

	payload, err := unix.Open("/dev/null", unix.O_RDWR, 0)
	if err != nil {
		t.Fatalf("打开载荷 fd: %v", err)
	}
	defer unix.Close(payload)

	sendDone := make(chan error, 1)
	go func() { sendDone <- sendHelperFD(server, payload) }()

	got, err := recvHelperFD(client)
	if err != nil {
		t.Fatalf("recvHelperFD: %v", err)
	}
	if err := <-sendDone; err != nil {
		t.Fatalf("sendHelperFD: %v", err)
	}
	defer unix.Close(got)

	// 收到的 fd 可 fstat。
	var stat unix.Stat_t
	if err := unix.Fstat(got, &stat); err != nil {
		t.Fatalf("接收 fd 不可 fstat: %v", err)
	}
	// dup 到 >=10 的高位 fd（避开 0 哨兵）。
	if got < 10 {
		t.Errorf("dup 后 fd = %d, want >= 10", got)
	}
	// 原始接收 fd 已被 recvHelperFD 关闭：进程内不应存在第二个等效低位 fd
	// （直接断言发送侧副本仍由调用方持有，未被误关）。
	if err := unix.Fstat(payload, &stat); err != nil {
		t.Errorf("发送侧 fd 不应被接收逻辑关闭: %v", err)
	}
}

func TestFDPassingRejectsNonUnixConn(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	if err := sendHelperFD(client, 0); err == nil {
		t.Error("net.Pipe 上发送应拒绝")
	}
	if _, err := recvHelperFD(client); err == nil {
		t.Error("net.Pipe 上接收应拒绝")
	}
}
