//go:build darwin

package gateway

// macOS helper 安装链路：统一由 internal/privhelper 编排（单 LaunchDaemon
// com.proxyd.helper 同进程服务 TUN 与 LAN 网关两个 socket），本文件只做
// 委托、卸载清理注册与本模块状态自查；旧版独立 gateway-helper 由 privhelper
// 在安装/卸载时迁移。本链路是本地特权操作，不经 HTTP API。

import (
	"os"
	"strings"

	"proxyd/internal/autostart"
	"proxyd/internal/privhelper"
)

// init 注册本模块的卸载清理：统一助手卸载时清 pf 规则、按记录恢复 IPv4 转发
// 原值，并删除 socket/anchor/状态文件（全部幂等）。
func init() {
	privhelper.RegisterUninstallCleanup(helperUninstallCleanupCommands)
}

// 可注入的统一助手状态查询，测试替换。
var (
	privhelperInstalled       = privhelper.Installed
	privhelperRunning         = privhelper.Running
	privhelperLegacyInstalled = privhelper.LegacyInstalled
)

// helperUninstallCleanupCommands 返回 gateway 模块的卸载清理命令。
// 参数：无。返回：[]autostart.PrivilegedCommand，按执行顺序排列。
// 错误：无；转发原值文件缺失或非法时跳过恢复项，其余清理不受影响。
func helperUninstallCleanupCommands() []autostart.PrivilegedCommand {
	commands := []autostart.PrivilegedCommand{
		{Name: "/sbin/pfctl", Args: []string{"-a", gatewayPFAnchor, "-F", "all"}, IgnoreError: true},
	}
	// 恢复转发原值（helper 运行时记录；文件由 root 创建但 0644 可读）。
	if data, err := os.ReadFile(helperForwardOrigPath); err == nil {
		if orig := strings.TrimSpace(string(data)); orig == "0" || orig == "1" {
			commands = append(commands, autostart.PrivilegedCommand{
				Name: "/usr/sbin/sysctl", Args: []string{"-w", "net.inet.ip.forwarding=" + orig}, IgnoreError: true,
			})
		}
	}
	return append(commands,
		autostart.PrivilegedCommand{Name: "/bin/rm", Args: []string{"-f", helperSocketPath}},
		autostart.PrivilegedCommand{Name: "/bin/rm", Args: []string{"-f", helperForwardOrigPath}},
		autostart.PrivilegedCommand{Name: "/bin/rm", Args: []string{"-f", gatewayAnchorFile}},
		autostart.PrivilegedCommand{Name: "/bin/rm", Args: []string{"-f", gatewayCombinedPFConf}},
	)
}

// HelperInstall 安装并启动统一特权助手（LAN 网关与 TUN 共用）。
//
// 参数：无。
//
// 返回值：
//   - error：安装完成（launchctl bootstrap 成功）时返回 nil。
//
// 错误情况：二进制路径解析、管理员授权或 launchctl 注册失败时返回错误；
// 重复安装视为升级；旧版独立 gateway-helper 在同一命令链中迁移清理。
func HelperInstall() error {
	return privhelper.Install()
}

// HelperUninstall 卸载统一特权助手（LAN 网关与 TUN 共用）：
// 本模块的 pf 规则清除、IPv4 转发恢复与文件残留经 init 注册的清理提供者执行。
//
// 参数：无。
//
// 返回值：
//   - error：全部清理完成时返回 nil；未安装时同样返回 nil（幂等）。
//
// 错误情况：单项清理失败返回错误，其余项仍按顺序尽力执行。
func HelperUninstall() error {
	return privhelper.Uninstall()
}

// HelperInstalledStatus 返回网关视角的助手安装链路自查结果：安装/加载状态查
// 统一助手，可达性仍拨本模块 socket 握手（协议版本兼容性以本模块为准）。
//
// 参数：无。
//
// 返回值：
//   - HelperStatus：plist 存在性、launchd 加载状态与 socket 可达性。
//
// 错误情况：无；各探测失败折叠为 false/Detail 文本。
func HelperInstalledStatus() HelperStatus {
	status := HelperStatus{}
	if privhelperInstalled() {
		status.Installed = true
	}
	if privhelperRunning() {
		status.Running = true
	}
	if _, err := helperCall(helperOpPing, nil); err == nil {
		status.Reachable = true
	}
	switch {
	case !status.Installed && privhelperLegacyInstalled():
		status.Detail = "检测到旧版独立 helper：执行 proxyd helper install 迁移为统一特权助手"
	case !status.Installed:
		status.Detail = "特权助手未安装：执行 proxyd helper install（LAN 网关与 TUN 共用）"
	case !status.Running:
		status.Detail = "特权助手已安装但未加载：执行 proxyd gateway helper uninstall 后重新 install"
	case !status.Reachable:
		status.Detail = "特权助手已加载但握手失败（可能版本不兼容）：请重新 install"
	default:
		status.Detail = "特权助手运行正常"
	}
	return status
}
