//go:build darwin

package privhelper

// macOS 安装链路单测：plist 过系统解析器、install/uninstall 命令链与属主解析。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"proxyd/internal/autostart"
)

// TestRenderPlistPassesPlutil 使用 macOS 原生解析器验证 plist（仿 autostart darwin_test.go）。
// 参数：t 为 *testing.T。返回：无。错误：plutil 拒绝时失败。
func TestRenderPlistPassesPlutil(t *testing.T) {
	path := filepath.Join(t.TempDir(), Label+".plist")
	if err := os.WriteFile(path, []byte(RenderPlist("/usr/local/bin/proxyd", 501)), 0o600); err != nil {
		t.Fatalf("写入 plist 测试文件失败: %v", err)
	}
	if output, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil 拒绝统一助手 plist: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

// TestInstallOwnerUID 验证安装属主解析：sudo 场景取 SUDO_UID，缺失/非法回退当前 UID。
// 参数：t 为 *testing.T。返回：无。错误：四种场景取值错误时失败。
func TestInstallOwnerUID(t *testing.T) {
	getenv := func(env map[string]string) func(string) string {
		return func(key string) string { return env[key] }
	}
	// sudo 提权场景：取 SUDO_UID（真实登录用户）。
	if uid := installOwnerUID(0, getenv(map[string]string{"SUDO_UID": "501"}), 0); uid != 501 {
		t.Errorf("sudo 场景属主 = %d, want 501", uid)
	}
	// sudo 但 SUDO_UID 缺失/非法：回退当前 UID。
	if uid := installOwnerUID(0, getenv(nil), 0); uid != 0 {
		t.Errorf("缺 SUDO_UID 应回退: %d", uid)
	}
	if uid := installOwnerUID(0, getenv(map[string]string{"SUDO_UID": "abc"}), 0); uid != 0 {
		t.Errorf("非法 SUDO_UID 应回退: %d", uid)
	}
	// 非 root 直接运行：忽略 SUDO_UID，取当前 UID。
	if uid := installOwnerUID(501, getenv(map[string]string{"SUDO_UID": "1"}), 501); uid != 501 {
		t.Errorf("非 root 场景属主 = %d, want 501", uid)
	}
}

// stubPrivileged 替换 runPrivilegedCommands 记录命令链，返回恢复函数。
// 参数：t 为 *testing.T，got 收集执行过的命令。返回：func() 恢复钩子。
func stubPrivileged(t *testing.T, got *[]autostart.PrivilegedCommand) func() {
	t.Helper()
	orig := runPrivilegedCommands
	runPrivilegedCommands = func(commands ...autostart.PrivilegedCommand) error {
		*got = append(*got, commands...)
		return nil
	}
	return func() { runPrivilegedCommands = orig }
}

// TestInstallCommandSequence 验证安装命令链：装 plist → 迁移两个旧版 helper →
// bootout 自身（重装即升级）→ bootstrap。
// 参数：t 为 *testing.T。返回：无。错误：链长度、顺序或迁移项缺失时失败。
func TestInstallCommandSequence(t *testing.T) {
	var got []autostart.PrivilegedCommand
	defer stubPrivileged(t, &got)()

	if err := Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(got) != 7 {
		t.Fatalf("命令数 = %d: %v", len(got), got)
	}
	if got[0].Name != "/usr/bin/install" || got[0].Args[len(got[0].Args)-1] != PlistPath {
		t.Errorf("install 命令异常: %+v", got[0])
	}
	joined := fmt.Sprint(got[1:5])
	for _, want := range []string{LegacyTunLabel, LegacyTunPlistPath, LegacyGatewayLabel, LegacyGatewayPlistPath} {
		if !strings.Contains(joined, want) {
			t.Errorf("迁移清理缺 %q: %v", want, got[1:5])
		}
	}
	if got[5].Name != "/bin/launchctl" || got[5].Args[0] != "bootout" || got[5].Args[1] != "system/"+Label || !got[5].IgnoreError {
		t.Errorf("自身 bootout 命令异常: %+v", got[5])
	}
	if got[6].Name != "/bin/launchctl" || got[6].Args[0] != "bootstrap" || got[6].Args[2] != PlistPath {
		t.Errorf("bootstrap 命令异常: %+v", got[6])
	}
}

// TestUninstallSequence 验证卸载命令链：bootout 自身 → 删 plist → 旧版迁移清理 →
// 各模块注册的清理提供者按序执行。
// 参数：t 为 *testing.T。返回：无。错误：链不完整或模块清理未执行时失败。
func TestUninstallSequence(t *testing.T) {
	var got []autostart.PrivilegedCommand
	defer stubPrivileged(t, &got)()

	origProviders := uninstallCleanupProviders
	defer func() { uninstallCleanupProviders = origProviders }()
	moduleCleaned := false
	RegisterUninstallCleanup(func() []autostart.PrivilegedCommand {
		moduleCleaned = true
		return []autostart.PrivilegedCommand{
			{Name: "/bin/rm", Args: []string{"-f", "/var/run/com.proxyd.tun.sock"}},
		}
	})

	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !moduleCleaned {
		t.Fatal("模块注册的清理提供者未执行")
	}
	joined := fmt.Sprint(got)
	for _, want := range []string{
		"bootout", PlistPath, LegacyTunPlistPath, LegacyGatewayPlistPath, "/var/run/com.proxyd.tun.sock",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("卸载命令缺 %q: %v", want, got)
		}
	}
	if got[0].Args[0] != "bootout" || got[0].Args[1] != "system/"+Label || !got[0].IgnoreError {
		t.Errorf("首条应为自身 bootout: %+v", got[0])
	}
}
