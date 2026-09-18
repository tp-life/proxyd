//go:build darwin

package tunhelper

// helper 服务端 darwin 执行能力与安装链路单测（fake syscall 注入 + 事件序列断言）。

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// syscallRecorder 记录 CreateTUN 的调用序列，并可按事件前缀注入失败。
type syscallRecorder struct {
	events []string
	closed []int
	failAt string // 事件前缀匹配时注入错误
	nextFD int
}

func (r *syscallRecorder) record(event string) error {
	r.events = append(r.events, event)
	if r.failAt != "" && strings.HasPrefix(event, r.failAt) {
		return fmt.Errorf("注入失败: %s", event)
	}
	return nil
}

// newFakeDarwinOps 构造带 fake syscall 的执行能力，返回事件记录器。
func newFakeDarwinOps() (*darwinHelperOps, *syscallRecorder) {
	rec := &syscallRecorder{nextFD: 100}
	ops := newDarwinHelperOps(nil)
	ops.socket = func(domain, typ, proto int) (int, error) {
		rec.nextFD++
		if err := rec.record(fmt.Sprintf("socket(%d,%d,%d)->%d", domain, typ, proto, rec.nextFD)); err != nil {
			return -1, err
		}
		return rec.nextFD, nil
	}
	ops.closeFD = func(fd int) {
		rec.closed = append(rec.closed, fd)
		rec.events = append(rec.events, fmt.Sprintf("close(%d)", fd))
	}
	ops.ioctlCtlInfo = func(fd int, name string) (uint32, error) {
		if err := rec.record(fmt.Sprintf("ctlinfo(%d,%s)", fd, name)); err != nil {
			return 0, err
		}
		return 7, nil
	}
	ops.connectCtl = func(fd int, ctlID uint32, unit uint32) error {
		return rec.record(fmt.Sprintf("connect(%d,id=%d,unit=%d)", fd, ctlID, unit))
	}
	ops.utunIfName = func(fd int) (string, error) {
		if err := rec.record(fmt.Sprintf("ifname(%d)", fd)); err != nil {
			return "", err
		}
		return "utun5", nil
	}
	ops.setMTU = func(socketFd int, ifName string, mtu int) error {
		return rec.record(fmt.Sprintf("mtu(%s,%d)", ifName, mtu))
	}
	ops.addAddr4 = func(socketFd int, ifName string, prefix netip.Prefix) error {
		return rec.record(fmt.Sprintf("addr4(%s,%s)", ifName, prefix))
	}
	ops.addAddr6 = func(socketFd int, ifName string, prefix netip.Prefix) error {
		return rec.record(fmt.Sprintf("addr6(%s,%s)", ifName, prefix))
	}
	ops.routeOp = func(rtmType int, destination netip.Prefix, gateway netip.Addr) error {
		op := "ADD"
		if rtmType == unix.RTM_DELETE {
			op = "DEL"
		}
		return rec.record(fmt.Sprintf("route(%s,%s,%s)", op, destination, gateway))
	}
	return ops, rec
}

// validCreateParams 返回一组合法创建参数（含 v6 与两条路由段）。
func validCreateParams() CreateTUNParams {
	return CreateTUNParams{
		MTU:             1500,
		Inet4Address:    "198.18.0.1/30",
		Inet6Address:    "fd00::1/64",
		AutoRoute:       true,
		AutoRouteRanges: []string{"0.0.0.0/1", "128.0.0.0/1"},
	}
}

func TestCreateTUNSequence(t *testing.T) {
	ops, rec := newFakeDarwinOps()
	fd, ifName, err := ops.CreateTUN(validCreateParams())
	if err != nil {
		t.Fatalf("CreateTUN: %v", err)
	}
	if fd != 101 || ifName != "utun5" {
		t.Fatalf("fd=%d ifName=%q", fd, ifName)
	}
	want := []string{
		"socket(32,2,2)->101", // AF_SYSTEM/SOCK_DGRAM/SYSPROTO_CONTROL
		"ctlinfo(101,com.apple.net.utun_control)",
		"connect(101,id=7,unit=0)",
		"ifname(101)",
		"socket(2,2,0)->102", // AF_INET 临时 socket
		"mtu(utun5,1500)",
		"addr4(utun5,198.18.0.1/30)",
		"close(102)",
		"socket(30,2,0)->103", // AF_INET6 临时 socket
		"addr6(utun5,fd00::1/64)",
		"close(103)",
		"route(ADD,0.0.0.0/1,198.18.0.1)",
		"route(ADD,128.0.0.0/1,198.18.0.1)",
	}
	if len(rec.events) != len(want) {
		t.Fatalf("事件序列 = %v", rec.events)
	}
	for i := range want {
		if rec.events[i] != want[i] {
			t.Errorf("events[%d] = %q, want %q", i, rec.events[i], want[i])
		}
	}
	// 成功路径不关闭设备 fd。
	for _, closed := range rec.closed {
		if closed == fd {
			t.Errorf("成功路径不应关闭设备 fd %d", fd)
		}
	}
}

func TestCreateTUNWithoutInet6AndAutoRoute(t *testing.T) {
	ops, rec := newFakeDarwinOps()
	if _, _, err := ops.CreateTUN(CreateTUNParams{MTU: 1500, Inet4Address: "198.18.0.1/30"}); err != nil {
		t.Fatalf("CreateTUN: %v", err)
	}
	for _, event := range rec.events {
		if strings.HasPrefix(event, "addr6(") || strings.HasPrefix(event, "route(") {
			t.Errorf("未配置 v6/auto_route 不应出现 %q", event)
		}
	}
}

func TestCreateTUNRollbackOnRouteFailure(t *testing.T) {
	ops, rec := newFakeDarwinOps()
	rec.failAt = "route(ADD,128.0.0.0/1"
	_, _, err := ops.CreateTUN(validCreateParams())
	if err == nil || !strings.Contains(err.Error(), "添加路由") {
		t.Fatalf("应返回路由失败错误: %v", err)
	}
	// 已添加的第一条路由被 RTM_DEL 回滚，随后关闭设备 fd。
	var del, closeDevice int = -1, -1
	for i, event := range rec.events {
		if event == "route(DEL,0.0.0.0/1,198.18.0.1)" {
			del = i
		}
		if event == "close(101)" {
			closeDevice = i
		}
	}
	if del < 0 {
		t.Errorf("缺少回滚 RTM_DEL: %v", rec.events)
	}
	if closeDevice < 0 {
		t.Errorf("失败路径应关闭设备 fd: %v", rec.events)
	}
	if del >= 0 && closeDevice >= 0 && del > closeDevice {
		t.Errorf("回滚应先于关闭 fd: %v", rec.events)
	}
}

func TestCreateTUNConnectFailureClosesFD(t *testing.T) {
	ops, rec := newFakeDarwinOps()
	rec.failAt = "connect("
	_, _, err := ops.CreateTUN(validCreateParams())
	if err == nil || !strings.Contains(err.Error(), "连接 utun 控制失败") {
		t.Fatalf("应返回 connect 失败错误: %v", err)
	}
	if len(rec.closed) != 1 || rec.closed[0] != 101 {
		t.Errorf("应仅关闭设备 fd: closed=%v", rec.closed)
	}
	for _, event := range rec.events {
		if strings.HasPrefix(event, "mtu(") || strings.HasPrefix(event, "route(") {
			t.Errorf("connect 失败后不应继续配置: %q", event)
		}
	}
}

func TestCreateTUNRejectsInvalidParams(t *testing.T) {
	ops, rec := newFakeDarwinOps()
	if _, _, err := ops.CreateTUN(CreateTUNParams{MTU: 0, Inet4Address: "198.18.0.1/30"}); err == nil {
		t.Fatal("mtu=0 应拒绝")
	}
	if _, _, err := ops.CreateTUN(CreateTUNParams{MTU: 1500, Inet4Address: "bad"}); err == nil {
		t.Fatal("非法 CIDR 应拒绝")
	}
	if len(rec.events) != 0 {
		t.Errorf("参数非法不应触达 syscall: %v", rec.events)
	}
}

func TestHelperUIDAuthorization(t *testing.T) {
	if !authorizedHelperUID(0, 501) {
		t.Error("root 应放行")
	}
	if !authorizedHelperUID(501, 501) {
		t.Error("属主应放行")
	}
	if authorizedHelperUID(502, 501) {
		t.Error("其他用户应拒绝")
	}
}

func TestInstalledStatusUnreachable(t *testing.T) {
	origDial := helperDial
	helperDial = func() (net.Conn, error) { return nil, fmt.Errorf("dial unix: no such file") }
	defer func() { helperDial = origDial }()
	origInstalled, origRunning, origLegacy := privhelperInstalled, privhelperRunning, privhelperLegacyInstalled
	privhelperInstalled = func() bool { return false }
	privhelperRunning = func() bool { return false }
	privhelperLegacyInstalled = func() bool { return false }
	defer func() {
		privhelperInstalled, privhelperRunning, privhelperLegacyInstalled = origInstalled, origRunning, origLegacy
	}()

	status := InstalledStatus()
	if status.Reachable || status.Running {
		t.Errorf("不可达场景状态 = %+v", status)
	}
	if !strings.Contains(status.Detail, "install") {
		t.Errorf("Detail 应含安装指引: %q", status.Detail)
	}
}

// TestInstalledStatusLegacyMigration 验证旧版独立 helper 残留时给出迁移指引。
// 参数：t 为 *testing.T。返回：无。错误：未识别残留或 Detail 缺指引时失败。
func TestInstalledStatusLegacyMigration(t *testing.T) {
	origDial := helperDial
	helperDial = func() (net.Conn, error) { return nil, fmt.Errorf("dial unix: no such file") }
	defer func() { helperDial = origDial }()
	origInstalled, origRunning, origLegacy := privhelperInstalled, privhelperRunning, privhelperLegacyInstalled
	privhelperInstalled = func() bool { return false }
	privhelperRunning = func() bool { return false }
	privhelperLegacyInstalled = func() bool { return true }
	defer func() {
		privhelperInstalled, privhelperRunning, privhelperLegacyInstalled = origInstalled, origRunning, origLegacy
	}()

	status := InstalledStatus()
	if !strings.Contains(status.Detail, "旧版") || !strings.Contains(status.Detail, "helper install") {
		t.Errorf("旧版残留应给出迁移指引: %q", status.Detail)
	}
}
