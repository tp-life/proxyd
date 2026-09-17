//go:build darwin

package tunhelper

// helper 服务端 darwin 执行能力与安装链路单测（fake syscall 注入 + 事件序列断言）。

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"proxyd/internal/autostart"

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

func TestRenderHelperPlist(t *testing.T) {
	plist := RenderHelperPlist("/usr/local/bin/proxyd", 501)
	for _, want := range []string{
		"<string>com.proxyd.tun-helper</string>",
		"<string>/usr/local/bin/proxyd</string>",
		"<string>tun-helper</string>",
		"<key>PROXYD_TUN_OWNER_UID</key>",
		"<string>501</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"/var/log/com.proxyd.tun-helper.log",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist 缺 %q:\n%s", want, plist)
		}
	}
	// XML 转义。
	escaped := RenderHelperPlist("/opt/a&b/proxyd", 501)
	if !strings.Contains(escaped, "/opt/a&amp;b/proxyd") {
		t.Error("路径未做 XML 转义")
	}
}

// TestRenderHelperPlistPassesPlutil 使用 macOS 原生解析器验证 plist（仿 autostart darwin_test.go）。
func TestRenderHelperPlistPassesPlutil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "com.proxyd.tun-helper.plist")
	if err := os.WriteFile(path, []byte(RenderHelperPlist("/usr/local/bin/proxyd", 501)), 0o600); err != nil {
		t.Fatalf("写入 plist 测试文件失败: %v", err)
	}
	if output, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil 拒绝 tun-helper plist: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

func TestHelperInstallOwnerUID(t *testing.T) {
	getenv := func(env map[string]string) func(string) string {
		return func(key string) string { return env[key] }
	}
	// sudo 提权场景：取 SUDO_UID（真实登录用户）。
	if uid := helperInstallOwnerUID(0, getenv(map[string]string{"SUDO_UID": "501"}), 0); uid != 501 {
		t.Errorf("sudo 场景属主 = %d, want 501", uid)
	}
	// sudo 但 SUDO_UID 缺失/非法：回退当前 UID。
	if uid := helperInstallOwnerUID(0, getenv(nil), 0); uid != 0 {
		t.Errorf("缺 SUDO_UID 应回退: %d", uid)
	}
	if uid := helperInstallOwnerUID(0, getenv(map[string]string{"SUDO_UID": "abc"}), 0); uid != 0 {
		t.Errorf("非法 SUDO_UID 应回退: %d", uid)
	}
	// 非 root 直接运行：忽略 SUDO_UID，取当前 UID。
	if uid := helperInstallOwnerUID(501, getenv(map[string]string{"SUDO_UID": "1"}), 501); uid != 501 {
		t.Errorf("非 root 场景属主 = %d, want 501", uid)
	}
}

func TestInstallCommandSequence(t *testing.T) {
	var got []autostart.PrivilegedCommand
	orig := runPrivilegedCommands
	runPrivilegedCommands = func(commands ...autostart.PrivilegedCommand) error {
		got = append(got, commands...)
		return nil
	}
	defer func() { runPrivilegedCommands = orig }()

	if err := Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("命令数 = %d: %v", len(got), got)
	}
	if got[0].Name != "/usr/bin/install" || got[0].Args[len(got[0].Args)-1] != helperPlistPath {
		t.Errorf("install 命令异常: %+v", got[0])
	}
	if got[1].Name != "/bin/launchctl" || got[1].Args[0] != "bootout" || !got[1].IgnoreError {
		t.Errorf("bootout 命令异常: %+v", got[1])
	}
	if got[2].Name != "/bin/launchctl" || got[2].Args[0] != "bootstrap" {
		t.Errorf("bootstrap 命令异常: %+v", got[2])
	}
}

func TestUninstallSequence(t *testing.T) {
	var got []autostart.PrivilegedCommand
	orig := runPrivilegedCommands
	runPrivilegedCommands = func(commands ...autostart.PrivilegedCommand) error {
		got = append(got, commands...)
		return nil
	}
	defer func() { runPrivilegedCommands = orig }()

	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("命令数 = %d: %v", len(got), got)
	}
	if got[0].Args[0] != "bootout" || !got[0].IgnoreError {
		t.Errorf("bootout 命令异常: %+v", got[0])
	}
	joined := fmt.Sprint(got)
	for _, want := range []string{helperPlistPath, helperSocketPath} {
		if !strings.Contains(joined, want) {
			t.Errorf("卸载命令缺 %q: %v", want, got)
		}
	}
}

func TestInstalledStatusUnreachable(t *testing.T) {
	origDial := helperDial
	helperDial = func() (net.Conn, error) { return nil, fmt.Errorf("dial unix: no such file") }
	defer func() { helperDial = origDial }()
	origRun := helperRun
	helperRun = func(string, ...string) (string, error) { return "", fmt.Errorf("no such service") }
	defer func() { helperRun = origRun }()

	status := InstalledStatus()
	if status.Reachable || status.Running {
		t.Errorf("不可达场景状态 = %+v", status)
	}
	if !strings.Contains(status.Detail, "install") {
		t.Errorf("Detail 应含安装指引: %q", status.Detail)
	}
}
