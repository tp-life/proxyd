//go:build darwin

package autostart

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFdIsTerminalRejectsDevNull 回归验证：/dev/null 是字符设备但不是终端，
// 托管进程（stdin=/dev/null）不得被误判为可交互终端而走 sudo 通道。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；/dev/null 判定为非终端时测试通过。
//
// 错误情况：误判为终端时测试失败。
func TestFdIsTerminalRejectsDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if fdIsTerminal(f.Fd()) {
		t.Fatal("/dev/null 不得判定为终端")
	}
}

// TestRenderPlistPassesPlutil 使用 macOS 原生解析器验证最终 LaunchDaemon 文件。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；plutil 接受生成的 plist 时测试通过。
//
// 错误情况：临时文件写入失败、plutil 不可执行或系统拒绝 plist 结构时测试失败。
func TestRenderPlistPassesPlutil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "com.proxyd.plist")
	content := RenderPlist("/usr/local/bin/proxyd", "/Users/x/.config/proxyd/config.yaml", "/Users/x/.local/state/proxyd/proxyd.log", "x", "/Users/x", false)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入 plist 测试文件失败: %v", err)
	}
	if output, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil 拒绝 LaunchDaemon plist: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

// TestDaemonRegisterCommands 验证注册命令链按服务状态取舍：未注册直接 bootstrap，
// 健康运行只刷新文件，崩溃循环强制 bootout 后重注册（修复 LWCR 签名绑定失效）。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；三种状态的命令链与预期完全一致时测试通过。
//
// 错误情况：bootout 缺失/误加或命令顺序错误时测试失败。
func TestDaemonRegisterCommands(t *testing.T) {
	chain := func(commands []privilegedCommand) []string {
		var names []string
		for _, c := range commands {
			names = append(names, filepath.Base(c.Name)+" "+strings.Join(c.Args, " "))
		}
		return names
	}
	exitCode := 78

	// 未注册：install → enable → bootstrap，不含 bootout。
	got := chain(daemonRegisterCommands("/tmp/x.plist", RuntimeStatus{}))
	want := []string{
		"install -o root -g wheel -m 0644 /tmp/x.plist " + daemonPlistPath,
		"launchctl enable system/" + plistLabel,
		"launchctl bootstrap system " + daemonPlistPath,
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("未注册命令链 = %v", got)
	}

	// 健康运行：仅刷新 plist 文件，不打扰运行中实例。
	got = chain(daemonRegisterCommands("/tmp/x.plist", RuntimeStatus{Loaded: true, Running: true, PID: 42}))
	if len(got) != 1 || !strings.HasPrefix(got[0], "install ") {
		t.Fatalf("健康服务应只刷新文件: %v", got)
	}

	// 崩溃循环：install → bootout → enable → bootstrap。
	got = chain(daemonRegisterCommands("/tmp/x.plist", RuntimeStatus{Loaded: true, State: "spawn scheduled", LastExitCode: &exitCode}))
	if len(got) != 4 || !strings.Contains(got[1], "bootout") || !strings.Contains(got[3], "bootstrap") {
		t.Fatalf("崩溃循环应强制重注册: %v", got)
	}

	// 干净停止（退出码 0 或无记录）：KeepAlive 仅崩溃拉起，也必须重注册才能启动。
	zeroCode := 0
	for _, s := range []RuntimeStatus{
		{Loaded: true, State: "not running", LastExitCode: &zeroCode},
		{Loaded: true, State: "not running"},
	} {
		got = chain(daemonRegisterCommands("/tmp/x.plist", s))
		if len(got) != 4 || !strings.Contains(got[1], "bootout") || !strings.Contains(got[3], "bootstrap") {
			t.Fatalf("干净停止应重新 bootstrap: %v", got)
		}
	}
}

// TestRenderShellCommandQuotesArguments 验证管理员授权脚本不会把路径内容解释为 shell 操作符。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；命令与参数均正确引用时测试通过。
//
// 错误情况：空格、单引号或分号未被安全引用时测试失败。
func TestRenderShellCommandQuotesArguments(t *testing.T) {
	command := privilegedCommand{
		Name: "/usr/bin/install",
		Args: []string{"/tmp/proxy d'aemon;touch bad", daemonPlistPath},
	}
	got := renderShellCommand(command)
	for _, want := range []string{
		"'/usr/bin/install'",
		"'/tmp/proxy d'\\''aemon;touch bad'",
		"'/Library/LaunchDaemons/com.proxyd.plist'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("管理员命令缺少安全引用 %q: %s", want, got)
		}
	}
}

// TestRenderElevatePlistPassesPlutil 验证一次性提权助手 LaunchAgent 的 plist 结构合法。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；plutil 接受生成的 plist 时测试通过。
//
// 错误情况：临时文件写入失败或系统拒绝 plist 结构时测试失败。
func TestRenderElevatePlistPassesPlutil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "com.proxyd.elevate.plist")
	if err := os.WriteFile(path, []byte(renderElevatePlist("/Users/x/Library/Caches/proxyd-elevate-a&b/wrapper.sh")), 0o600); err != nil {
		t.Fatalf("写入 plist 测试文件失败: %v", err)
	}
	if output, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil 拒绝提权助手 plist: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

// TestInteractionNotAllowed 验证仅授权窗口不可达的错误会触发图形会话回退。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；-60007 类错误被识别、其余错误不误判时测试通过。
//
// 错误情况：识别结果与预期不符时测试失败。
func TestInteractionNotAllowed(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("osascript: execution error: 管理员用户名或密码不正确。 (-60007)"), true},
		{errors.New("execution error: User interaction is not allowed. (-60007)"), true},
		{errors.New("execution error: User canceled. (-128)"), false},
		{errors.New("command not found"), false},
	}
	for _, c := range cases {
		if got := interactionNotAllowed(c.err); got != c.want {
			t.Fatalf("interactionNotAllowed(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestEscapeAppleScript 验证 shell 文本嵌入 AppleScript 时正确转义。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；反斜杠与双引号均只作为字符串内容时测试通过。
//
// 错误情况：任一字符可能提前结束 AppleScript 字面量时测试失败。
func TestEscapeAppleScript(t *testing.T) {
	got := escapeAppleScript(`a\b"c`)
	if got != `a\\b\"c` {
		t.Fatalf("AppleScript 转义结果 = %q", got)
	}
}
