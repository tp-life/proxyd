//go:build darwin

package privhelper

// macOS 统一助手安装链路：生成 LaunchDaemon plist（属主 UID 记入环境变量），
// 经 autostart 导出的管理员授权通道写入 /Library/LaunchDaemons 并 bootstrap；
// 安装/卸载都会迁移清理旧版按模块拆分的 helper（tun-helper/gateway-helper）。
// 本链路是本地特权操作，不经 HTTP API。

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"proxyd/internal/autostart"
)

// runPrivilegedCommands 以管理员权限执行固定命令链；包级变量便于测试替换。
var runPrivilegedCommands = autostart.RunPrivilegedCommands

// uninstallCleanupProviders 是各模块注册的卸载清理命令提供者（仅 darwin 注册）；
// 统一助手被卸载时按注册顺序执行，保证共享助手卸载不留下模块级残留
// （gateway 的 pf 规则/转发原值、各模块 socket 文件）。
var uninstallCleanupProviders []func() []autostart.PrivilegedCommand

// RegisterUninstallCleanup 登记模块级卸载清理命令提供者。
//
// 参数：
//   - fn: 返回该模块需要以 root 执行的清理命令（必须幂等，目标不存在不算失败）。
//
// 返回值：无。错误：无；模块在 init 中调用，重复注册会重复执行同一清理。
func RegisterUninstallCleanup(fn func() []autostart.PrivilegedCommand) {
	uninstallCleanupProviders = append(uninstallCleanupProviders, fn)
}

// installOwnerUID 解析应记录为属主的 UID：sudo 提权场景取 SUDO_UID
// （真实登录用户），否则取当前进程 UID。注入 euid/getenv/fallbackUID 便于单测。
func installOwnerUID(euid int, getenv func(string) string, fallbackUID int) int {
	if raw := strings.TrimSpace(getenv("SUDO_UID")); euid == 0 && raw != "" {
		if uid, err := strconv.Atoi(raw); err == nil && uid > 0 {
			return uid
		}
	}
	return fallbackUID
}

// migrationCommands 返回旧版按模块拆分 helper 的迁移清理命令（幂等）：
// bootout 旧服务并删除旧 plist；旧 socket 与新服务端同路径，启动时自动接管，
// 无需单独删除。
func migrationCommands() []autostart.PrivilegedCommand {
	return []autostart.PrivilegedCommand{
		{Name: "/bin/launchctl", Args: []string{"bootout", "system/" + LegacyTunLabel}, IgnoreError: true},
		{Name: "/bin/rm", Args: []string{"-f", LegacyTunPlistPath}},
		{Name: "/bin/launchctl", Args: []string{"bootout", "system/" + LegacyGatewayLabel}, IgnoreError: true},
		{Name: "/bin/rm", Args: []string{"-f", LegacyGatewayPlistPath}},
	}
}

// Install 安装并启动统一特权助手（LaunchDaemon，root 常驻）。
//
// 参数：无。
//
// 返回值：
//   - error：安装完成（launchctl bootstrap 成功）时返回 nil。
//
// 错误情况：二进制路径解析、临时文件、管理员授权或 launchctl 注册失败时返回错误；
// 重复安装视为升级（先 bootout 再以当前二进制路径重新 bootstrap）；
// 旧版按模块拆分的 helper 在同一命令链中迁移清理。
func Install() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("解析二进制路径失败: %w", err)
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return fmt.Errorf("解析二进制绝对路径失败: %w", err)
	}
	ownerUID := installOwnerUID(os.Geteuid(), os.Getenv, os.Getuid())
	temporary, err := os.CreateTemp("", "proxyd-helper-*.plist")
	if err != nil {
		return fmt.Errorf("创建临时 plist 失败: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.WriteString(RenderPlist(exe, ownerUID)); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("写入临时 plist 失败: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭临时 plist 失败: %w", err)
	}
	commands := []autostart.PrivilegedCommand{
		{Name: "/usr/bin/install", Args: []string{"-o", "root", "-g", "wheel", "-m", "0644", temporaryPath, PlistPath}},
	}
	commands = append(commands, migrationCommands()...)
	commands = append(commands,
		autostart.PrivilegedCommand{Name: "/bin/launchctl", Args: []string{"bootout", "system/" + Label}, IgnoreError: true},
		autostart.PrivilegedCommand{Name: "/bin/launchctl", Args: []string{"bootstrap", "system", PlistPath}},
	)
	return runPrivilegedCommands(commands...)
}

// Uninstall 卸载统一助手：bootout 服务、删除 plist，迁移清理旧版 helper，
// 并执行各模块注册的清理（gateway 清 pf 规则/恢复转发、各模块 socket 残留）。
// TUN 的 utun 设备与路由随主进程 fd 关闭由内核回收，无额外清理项。
//
// 参数：无。
//
// 返回值：
//   - error：全部清理完成时返回 nil；未安装时同样返回 nil（幂等）。
//
// 错误情况：单项清理失败返回错误，其余项仍按顺序尽力执行（命令级容错由
// IgnoreError 标记控制，launchctl/rm 对不存在目标不算失败）。
func Uninstall() error {
	commands := []autostart.PrivilegedCommand{
		{Name: "/bin/launchctl", Args: []string{"bootout", "system/" + Label}, IgnoreError: true},
		{Name: "/bin/rm", Args: []string{"-f", PlistPath}},
	}
	commands = append(commands, migrationCommands()...)
	for _, provider := range uninstallCleanupProviders {
		commands = append(commands, provider()...)
	}
	return runPrivilegedCommands(commands...)
}
