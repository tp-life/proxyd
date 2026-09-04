package autostart

import (
	"fmt"
	"strings"
)

// plistLabel 是 macOS LaunchDaemon 的 Label / plist 文件基名。
const plistLabel = "com.proxyd"

// 平台自启项内容的纯渲染函数：与写文件/注册动作分离，供各平台实现与单元测试共用。

// plistEscape 编码 plist XML 文本中的保留字符。
//
// 参数说明：
//   - s: string，待嵌入 plist 的原始文本。
//
// 返回值说明：string，&、<、> 已替换为实体引用的文本。
//
// 错误情况：无；plist 是 XML 文本而不是 shell 命令，仍必须编码保留字符，
// 否则用户名或路径中的这些字符会让 launchd 拒绝整个服务定义。
func plistEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// RenderPlist 生成 macOS 系统级 LaunchDaemon plist 内容。
//
// 参数说明：
//   - exe: string，proxyd 可执行文件绝对路径。
//   - cfgPath: string，守护进程读取的配置文件绝对路径。
//   - logPath: string，launchd 标准输出与错误日志路径。
//   - userName: string，LaunchDaemon 降权运行时使用的本机账户名。
//   - homeDir: string，该账户的 HOME，用于保持默认配置与状态目录语义。
//
// 返回值说明：string，可写入 /Library/LaunchDaemons/com.proxyd.plist 的 XML。
//
// 错误情况：无；所有外部文本都会执行 XML 转义，文件写入与 launchctl 错误由调用方处理。
func RenderPlist(exe, cfgPath, logPath, userName, homeDir string) string {
	esc := plistEscape
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
		<string>-c</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>UserName</key>
	<string>%s</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>%s</string>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, plistLabel, esc(exe), esc(cfgPath), esc(userName), esc(homeDir), esc(logPath), esc(logPath))
}

// RenderUnit 生成 Linux systemd user unit 内容。
//
// 参数说明：
//   - exe: string，proxyd 可执行文件绝对路径。
//   - cfgPath: string，proxyd 配置文件绝对路径。
//
// 返回值说明：string，可写入 systemd user unit 的配置文本。
//
// 错误情况：无；调用方负责文件写入和 systemctl 错误处理。
func RenderUnit(exe, cfgPath string) string {
	return fmt.Sprintf(`[Unit]
Description=proxyd — multi-node port-mapping proxy
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s serve -c %s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, exe, cfgPath)
}

// RenderRunCommand 生成 Windows 注册表 Run 项的命令行：
// 登录时执行 `proxyd start`（派生 detached 后台 serve 进程后退出），避免常驻控制台窗口。
//
// 参数说明：
//   - exe: string，proxyd 可执行文件绝对路径。
//   - cfgPath: string，proxyd 配置文件绝对路径。
//
// 返回值说明：string，可直接写入 HKCU Run 项的命令行。
//
// 错误情况：无；注册表写入错误由 Windows 平台实现处理。
func RenderRunCommand(exe, cfgPath string) string {
	return fmt.Sprintf(`"%s" start -c "%s"`, exe, cfgPath)
}
