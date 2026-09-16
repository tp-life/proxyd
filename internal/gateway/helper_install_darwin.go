//go:build darwin

package gateway

// macOS helper 安装链路：生成 LaunchDaemon plist（属主 UID 记入环境变量），
// 经 autostart 导出的管理员授权通道写入 /Library/LaunchDaemons 并 bootstrap；
// 卸载时先清 pf 规则、恢复转发原值，再移除服务与全部残留文件。
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
		<string>gateway-helper</string>
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
// （真实登录用户），否则取当前进程 UID。
func helperInstallOwnerUID() (int, error) {
	if raw := strings.TrimSpace(os.Getenv("SUDO_UID")); os.Geteuid() == 0 && raw != "" {
		uid, err := strconv.Atoi(raw)
		if err == nil && uid > 0 {
			return uid, nil
		}
	}
	return os.Getuid(), nil
}

// HelperInstall 安装并启动 gateway 特权 helper（LaunchDaemon，root 常驻）。
//
// 参数：无。
//
// 返回值：
//   - error：安装完成（launchctl bootstrap 成功）时返回 nil。
//
// 错误情况：二进制路径解析、临时文件、管理员授权或 launchctl 注册失败时返回错误；
// 重复安装视为升级（先 bootout 再以当前二进制路径重新 bootstrap）。
func HelperInstall() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("解析二进制路径失败: %w", err)
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return fmt.Errorf("解析二进制绝对路径失败: %w", err)
	}
	ownerUID, err := helperInstallOwnerUID()
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp("", "proxyd-gateway-helper-*.plist")
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

// HelperUninstall 卸载 helper：先清 pf 规则并恢复 IPv4 转发原值，
// 再 bootout 服务、删除 plist、socket 与全部状态文件。
//
// 参数：无。
//
// 返回值：
//   - error：全部清理完成时返回 nil；未安装时同样返回 nil（幂等）。
//
// 错误情况：单项清理失败返回错误，其余项仍按顺序尽力执行（命令级容错由
// IgnoreError 标记控制，launchctl/pfctl 对不存在目标不算失败）。
func HelperUninstall() error {
	commands := []autostart.PrivilegedCommand{
		{Name: "/bin/launchctl", Args: []string{"bootout", "system/" + helperLabel}, IgnoreError: true},
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
	commands = append(commands,
		autostart.PrivilegedCommand{Name: "/bin/rm", Args: []string{"-f", helperPlistPath}},
		autostart.PrivilegedCommand{Name: "/bin/rm", Args: []string{"-f", helperSocketPath}},
		autostart.PrivilegedCommand{Name: "/bin/rm", Args: []string{"-f", helperForwardOrigPath}},
		autostart.PrivilegedCommand{Name: "/bin/rm", Args: []string{"-f", gatewayAnchorFile}},
		autostart.PrivilegedCommand{Name: "/bin/rm", Args: []string{"-f", gatewayCombinedPFConf}},
	)
	return runPrivilegedCommands(commands...)
}

// HelperInstalledStatus 返回 helper 安装链路的本地自查结果。
//
// 参数：无。
//
// 返回值：
//   - HelperStatus：plist 存在性、launchd 加载状态与 socket 可达性。
//
// 错误情况：无；各探测失败折叠为 false/Detail 文本。
func HelperInstalledStatus() HelperStatus {
	status := HelperStatus{}
	if _, err := os.Stat(helperPlistPath); err == nil {
		status.Installed = true
	}
	if _, err := helperRun("/bin/launchctl", "print", "system/"+helperLabel); err == nil {
		status.Running = true
	}
	if _, err := helperCall(helperOpPing, nil); err == nil {
		status.Reachable = true
	}
	switch {
	case !status.Installed:
		status.Detail = "helper 未安装：执行 proxyd gateway helper install"
	case !status.Running:
		status.Detail = "helper 已安装但未加载：尝试 proxyd gateway helper uninstall 后重新 install"
	case !status.Reachable:
		status.Detail = "helper 已加载但握手失败（可能版本不兼容）：请重新 install"
	default:
		status.Detail = "helper 运行正常"
	}
	return status
}
