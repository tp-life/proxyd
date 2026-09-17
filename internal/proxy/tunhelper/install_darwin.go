//go:build darwin

package tunhelper

// macOS helper 安装链路：生成 LaunchDaemon plist（属主 UID 记入环境变量），
// 经 autostart 导出的管理员授权通道写入 /Library/LaunchDaemons 并 bootstrap；
// 卸载时 bootout 服务并移除 plist 与 socket 残留。
// 本链路是本地特权操作，不经 HTTP API。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"proxyd/internal/autostart"
)

// runPrivilegedCommands 以管理员权限执行固定命令链；包级变量便于测试替换。
var runPrivilegedCommands = autostart.RunPrivilegedCommands

// helperRun 执行外部命令；包级变量便于测试替换。
var helperRun = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// RenderHelperPlist 生成 helper LaunchDaemon 的 plist 内容。
//
// 参数：
//   - exePath: string，proxyd 二进制绝对路径（helper 与主进程同二进制）。
//   - ownerUID: int，安装时记录的属主 UID（helper 只放行该用户与 root）。
//
// 返回值：
//   - string：完整 plist 文本；RunAtLoad + KeepAlive 保证常驻。
//
// 错误情况：无；路径经 XML 转义后嵌入。
func RenderHelperPlist(exePath string, ownerUID int) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + helperLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(exePath) + `</string>
		<string>tun-helper</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>` + helperOwnerUIDEnv + `</key>
		<string>` + strconv.Itoa(ownerUID) + `</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/var/log/` + helperLabel + `.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/` + helperLabel + `.log</string>
</dict>
</plist>
`
}

// xmlEscape 转义 plist 文本节点中的特殊字符。
func xmlEscape(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
	)
	return replacer.Replace(value)
}

// helperInstallOwnerUID 解析应记录为属主的 UID：sudo 提权场景取 SUDO_UID
// （真实登录用户），否则取当前进程 UID。注入 euid/getenv/fallbackUID 便于单测。
func helperInstallOwnerUID(euid int, getenv func(string) string, fallbackUID int) int {
	if raw := strings.TrimSpace(getenv("SUDO_UID")); euid == 0 && raw != "" {
		if uid, err := strconv.Atoi(raw); err == nil && uid > 0 {
			return uid
		}
	}
	return fallbackUID
}

// Install 安装并启动 tun 特权 helper（LaunchDaemon，root 常驻）。
//
// 参数：无。
//
// 返回值：
//   - error：安装完成（launchctl bootstrap 成功）时返回 nil。
//
// 错误情况：二进制路径解析、临时文件、管理员授权或 launchctl 注册失败时返回错误；
// 重复安装视为升级（先 bootout 再以当前二进制路径重新 bootstrap）。
func Install() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("解析二进制路径失败: %w", err)
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return fmt.Errorf("解析二进制绝对路径失败: %w", err)
	}
	ownerUID := helperInstallOwnerUID(os.Geteuid(), os.Getenv, os.Getuid())
	temporary, err := os.CreateTemp("", "proxyd-tun-helper-*.plist")
	if err != nil {
		return fmt.Errorf("创建临时 plist 失败: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.WriteString(RenderHelperPlist(exe, ownerUID)); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("写入临时 plist 失败: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return runPrivilegedCommands(
		autostart.PrivilegedCommand{Name: "/usr/bin/install", Args: []string{"-o", "root", "-g", "wheel", "-m", "0644", temporaryPath, helperPlistPath}},
		autostart.PrivilegedCommand{Name: "/bin/launchctl", Args: []string{"bootout", "system/" + helperLabel}, IgnoreError: true},
		autostart.PrivilegedCommand{Name: "/bin/launchctl", Args: []string{"bootstrap", "system", helperPlistPath}},
	)
}

// Uninstall 卸载 helper：bootout 服务、删除 plist 与 socket 残留。
// utun 设备与路由随主进程 fd 关闭由内核回收，无额外清理项。
//
// 参数：无。
//
// 返回值：
//   - error：全部清理完成时返回 nil；未安装时同样返回 nil（幂等）。
//
// 错误情况：单项清理失败返回错误，其余项仍按顺序尽力执行（命令级容错由
// IgnoreError 标记控制，launchctl/rm 对不存在目标不算失败）。
func Uninstall() error {
	return runPrivilegedCommands(
		autostart.PrivilegedCommand{Name: "/bin/launchctl", Args: []string{"bootout", "system/" + helperLabel}, IgnoreError: true},
		autostart.PrivilegedCommand{Name: "/bin/rm", Args: []string{"-f", helperPlistPath}},
		autostart.PrivilegedCommand{Name: "/bin/rm", Args: []string{"-f", helperSocketPath}},
	)
}

// InstalledStatus 返回 helper 安装链路的本地自查结果。
//
// 参数：无。
//
// 返回值：
//   - HelperStatus：plist 存在性、launchd 加载状态与 socket 可达性。
//
// 错误情况：无；各探测失败折叠为 false/Detail 文本。
func InstalledStatus() HelperStatus {
	status := HelperStatus{}
	if _, err := os.Stat(helperPlistPath); err == nil {
		status.Installed = true
	}
	if _, err := helperRun("/bin/launchctl", "print", "system/"+helperLabel); err == nil {
		status.Running = true
	}
	if err := Precheck(); err == nil {
		status.Reachable = true
	}
	switch {
	case !status.Installed:
		status.Detail = "helper 未安装：执行 proxyd tun helper install"
	case !status.Running:
		status.Detail = "helper 已安装但未加载：尝试 proxyd tun-helper uninstall 后重新 install"
	case !status.Reachable:
		status.Detail = "helper 已加载但握手失败（可能版本不兼容）：请重新 install"
	default:
		status.Detail = "helper 运行正常"
	}
	return status
}
