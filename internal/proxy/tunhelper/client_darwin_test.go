//go:build darwin

package tunhelper

// helper 客户端单测：helperDial 注入假连接，含 socketpair 全回路 fd 传递。

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRequestTUNDialFailure(t *testing.T) {
	orig := helperDial
	helperDial = func() (net.Conn, error) {
		return nil, errors.New("dial unix: no such file or directory")
	}
	defer func() { helperDial = orig }()

	_, _, err := RequestTUN(CreateTUNParams{MTU: 1500, Inet4Address: "198.18.0.1/30"})
	if err == nil {
		t.Fatal("拨号失败应返回错误")
	}
	if !errors.Is(err, ErrHelperUnavailable) {
		t.Errorf("应包装 ErrHelperUnavailable: %v", err)
	}
	if !strings.Contains(err.Error(), helperSocketPath) || !strings.Contains(err.Error(), "helper 未安装") {
		t.Errorf("错误应含 socket 路径与安装指引: %v", err)
	}
	if err := Precheck(); !errors.Is(err, ErrHelperUnavailable) {
		t.Errorf("Precheck 应包装 ErrHelperUnavailable: %v", err)
	}
}

func TestRequestTUNRejectsInvalidParamsLocally(t *testing.T) {
	dialed := false
	orig := helperDial
	helperDial = func() (net.Conn, error) {
		dialed = true
		return nil, errors.New("不应拨号")
	}
	defer func() { helperDial = orig }()

	if _, _, err := RequestTUN(CreateTUNParams{MTU: 0, Inet4Address: "198.18.0.1/30"}); err == nil {
		t.Fatal("非法参数应本地拒绝")
	}
	if dialed {
		t.Error("非法参数不应触达拨号")
	}
}

// TestRequestTUNRoundTrip 经 socketpair 跑完整回路：握手 → tun.create →
// 确认字节 → SCM_RIGHTS 收 fd，断言接收 fd 可用且 helper 侧副本已关闭。
func TestRequestTUNRoundTrip(t *testing.T) {
	client, server := unixConnPair(t)
	defer client.Close()
	defer server.Close()

	// fake ops 返回一个真实 fd（/dev/null），供服务端经 SCM_RIGHTS 发出。
	payload, err := unix.Open("/dev/null", unix.O_RDWR, 0)
	if err != nil {
		t.Fatalf("打开载荷 fd: %v", err)
	}
	ops := newFakeHelperOps()
	ops.fd = payload
	ops.ifName = "utun7"
	go serveHelperConn(server, ops)

	orig := helperDial
	helperDial = func() (net.Conn, error) { return client, nil }
	defer func() { helperDial = orig }()

	fd, ifName, err := RequestTUN(CreateTUNParams{MTU: 1500, Inet4Address: "198.18.0.1/30"})
	if err != nil {
		t.Fatalf("RequestTUN: %v", err)
	}
	defer unix.Close(fd)
	if ifName != "utun7" {
		t.Errorf("ifName = %q, want utun7", ifName)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		t.Errorf("接收 fd 不可 fstat: %v", err)
	}
	if fd < 10 {
		t.Errorf("dup 后 fd = %d, want >= 10", fd)
	}
	// helper 发出后即关闭自己的副本（主进程关 fd 即销毁 utun 的前提）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := unix.Fstat(payload, &stat)
		if errors.Is(err, syscall.EBADF) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper 侧 fd 副本未关闭（fstat 仍成功: %v）", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
