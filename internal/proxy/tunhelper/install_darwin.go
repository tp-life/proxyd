//go:build darwin

package tunhelper

// macOS helper 安装链路：统一由 internal/privhelper 编排（单 LaunchDaemon
// com.proxyd.helper 同进程服务 TUN 与 LAN 网关两个 socket），本文件只做
// 委托与本模块状态自查；旧版独立 tun-helper 由 privhelper 在安装/卸载时迁移。
// 本链路是本地特权操作，不经 HTTP API。

import (
	"proxyd/internal/autostart"
	"proxyd/internal/privhelper"
)

// init 注册本模块的卸载清理：统一助手卸载时删除本模块 socket 残留（幂等）。
func init() {
	privhelper.RegisterUninstallCleanup(func() []autostart.PrivilegedCommand {
		return []autostart.PrivilegedCommand{
			{Name: "/bin/rm", Args: []string{"-f", helperSocketPath}},
		}
	})
}

// 可注入的统一助手状态查询，测试替换。
var (
	privhelperInstalled       = privhelper.Installed
	privhelperRunning         = privhelper.Running
	privhelperLegacyInstalled = privhelper.LegacyInstalled
)

// Install 安装并启动统一特权助手（TUN 与 LAN 网关共用）。
//
// 参数：无。
//
// 返回值：
//   - error：安装完成（launchctl bootstrap 成功）时返回 nil。
//
// 错误情况：二进制路径解析、管理员授权或 launchctl 注册失败时返回错误；
// 重复安装视为升级；旧版独立 tun-helper 在同一命令链中迁移清理。
func Install() error {
	return privhelper.Install()
}

// Uninstall 卸载统一特权助手（TUN 与 LAN 网关共用）：
// 本模块的 socket 残留经 init 注册的清理提供者一并删除；
// utun 设备与路由随主进程 fd 关闭由内核回收，无额外清理项。
//
// 参数：无。
//
// 返回值：
//   - error：全部清理完成时返回 nil；未安装时同样返回 nil（幂等）。
//
// 错误情况：单项清理失败返回错误，其余项仍按顺序尽力执行。
func Uninstall() error {
	return privhelper.Uninstall()
}

// InstalledStatus 返回 TUN 视角的助手安装链路自查结果：安装/加载状态查统一
// 助手，可达性仍拨本模块 socket 握手（协议版本兼容性以本模块为准）。
//
// 参数：无。
//
// 返回值：
//   - HelperStatus：plist 存在性、launchd 加载状态与 socket 可达性。
//
// 错误情况：无；各探测失败折叠为 false/Detail 文本。
func InstalledStatus() HelperStatus {
	status := HelperStatus{}
	if privhelperInstalled() {
		status.Installed = true
	}
	if privhelperRunning() {
		status.Running = true
	}
	if err := Precheck(); err == nil {
		status.Reachable = true
	}
	switch {
	case !status.Installed && privhelperLegacyInstalled():
		status.Detail = "检测到旧版独立 helper：执行 proxyd helper install 迁移为统一特权助手"
	case !status.Installed:
		status.Detail = "特权助手未安装：执行 proxyd helper install（TUN 与 LAN 网关共用）"
	case !status.Running:
		status.Detail = "特权助手已安装但未加载：执行 proxyd tun helper uninstall 后重新 install"
	case !status.Reachable:
		status.Detail = "特权助手已加载但握手失败（可能版本不兼容）：请重新 install"
	default:
		status.Detail = "特权助手运行正常"
	}
	return status
}
