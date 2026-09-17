//go:build darwin

package autostart

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

// TestRunPrivilegedTerminalPrefersSudo 验证「终端优先」：stdin 为终端的普通用户
// 进程经 sudo 执行命令链，不再走 osascript GUI 授权弹窗。
func TestRunPrivilegedTerminalPrefersSudo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下 runPrivileged 直接执行，不经过本分支")
	}
	originalTerminal, originalLookPath, originalRun := stdioIsTerminal, sudoLookPath, runTerminalCommand
	originalRunCmd := run
	t.Cleanup(func() {
		stdioIsTerminal, sudoLookPath, runTerminalCommand = originalTerminal, originalLookPath, originalRun
		run = originalRunCmd
	})

	stdioIsTerminal = func() bool { return true }
	sudoLookPath = func() (string, error) { return "/usr/bin/sudo", nil }
	var got [][]string
	runTerminalCommand = func(name string, args ...string) error {
		got = append(got, append([]string{name}, args...))
		return nil
	}
	run = func(name string, args ...string) (string, error) {
		return "", fmt.Errorf("osascript 不应被调用: %s", name)
	}

	commands := []privilegedCommand{
		{Name: "/usr/bin/install", Args: []string{"-o", "root", "a", "b"}},
		{Name: "/bin/launchctl", Args: []string{"bootout", "system/x"}, IgnoreError: true},
	}
	if err := runPrivileged(serviceAccount{UserName: "u", HomeDir: "/Users/u", UID: "501"}, commands...); err != nil {
		t.Fatalf("终端 sudo 路径应成功: %v", err)
	}
	if len(got) != 2 || got[0][0] != "/usr/bin/sudo" || got[0][1] != "/usr/bin/install" {
		t.Fatalf("命令应经 sudo 逐条执行: %v", got)
	}
}

// TestRunPrivilegedTerminalSudoFailureNoGUI 验证 sudo 执行失败（含用户取消）时
// 直接返回错误，不叠加 GUI 弹窗二次打扰。
func TestRunPrivilegedTerminalSudoFailureNoGUI(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下 runPrivileged 直接执行，不经过本分支")
	}
	originalTerminal, originalLookPath, originalRun := stdioIsTerminal, sudoLookPath, runTerminalCommand
	originalRunCmd := run
	t.Cleanup(func() {
		stdioIsTerminal, sudoLookPath, runTerminalCommand = originalTerminal, originalLookPath, originalRun
		run = originalRunCmd
	})

	stdioIsTerminal = func() bool { return true }
	sudoLookPath = func() (string, error) { return "/usr/bin/sudo", nil }
	runTerminalCommand = func(name string, args ...string) error { return errors.New("用户取消") }
	run = func(name string, args ...string) (string, error) {
		return "", fmt.Errorf("osascript 不应被调用: %s", name)
	}

	err := runPrivileged(serviceAccount{UserName: "u", HomeDir: "/Users/u", UID: "501"},
		privilegedCommand{Name: "/bin/rm", Args: []string{"-f", "/tmp/x"}})
	if err == nil {
		t.Fatal("sudo 失败应返回错误")
	}
}

// TestRunPrivilegedTerminalWithoutSudoFallsBackToGUI 验证终端存在但 sudo 缺失时
// 回退 osascript GUI 授权通道。
func TestRunPrivilegedTerminalWithoutSudoFallsBackToGUI(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下 runPrivileged 直接执行，不经过本分支")
	}
	originalTerminal, originalLookPath := stdioIsTerminal, sudoLookPath
	originalRunCmd := run
	t.Cleanup(func() {
		stdioIsTerminal, sudoLookPath = originalTerminal, originalLookPath
		run = originalRunCmd
	})

	stdioIsTerminal = func() bool { return true }
	sudoLookPath = func() (string, error) { return "", errors.New("not found") }
	osascriptCalled := false
	run = func(name string, args ...string) (string, error) {
		if name == "/usr/bin/osascript" {
			osascriptCalled = true
			return "", nil
		}
		return "", nil
	}

	err := runPrivileged(serviceAccount{UserName: "u", HomeDir: "/Users/u", UID: "501"},
		privilegedCommand{Name: "/bin/rm", Args: []string{"-f", "/tmp/x"}})
	if err != nil {
		t.Fatalf("osascript 回退应成功: %v", err)
	}
	if !osascriptCalled {
		t.Fatal("sudo 缺失时应回退 osascript GUI 授权")
	}
}

// TestRunViaSudoIgnoreError 验证 IgnoreError 命令的失败被忽略。
func TestRunViaSudoIgnoreError(t *testing.T) {
	originalLookPath, originalRun := sudoLookPath, runTerminalCommand
	t.Cleanup(func() { sudoLookPath, runTerminalCommand = originalLookPath, originalRun })

	sudoLookPath = func() (string, error) { return "/usr/bin/sudo", nil }
	calls := 0
	runTerminalCommand = func(name string, args ...string) error {
		calls++
		return errors.New("bootout: 服务不存在")
	}
	err := runViaSudo(privilegedCommand{Name: "/bin/launchctl", Args: []string{"bootout", "system/x"}, IgnoreError: true})
	if err != nil {
		t.Fatalf("IgnoreError 命令失败应被忽略: %v", err)
	}
	if calls != 1 {
		t.Fatalf("应执行一次，实际 %d", calls)
	}
}
