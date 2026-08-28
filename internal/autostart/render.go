package autostart

import (
	"fmt"
	"strings"
)

// plistLabel 是 macOS LaunchAgents 的 Label / plist 文件基名。
const plistLabel = "com.proxyd"

// 平台自启项内容的纯渲染函数：与写文件/注册动作分离，供各平台实现与单元测试共用。

// RenderPlist 生成 macOS LaunchAgents plist 内容。
func RenderPlist(exe, cfgPath, logPath string) string {
	esc := func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
		return r.Replace(s)
	}
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
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, plistLabel, esc(exe), esc(cfgPath), esc(logPath), esc(logPath))
}

// RenderUnit 生成 Linux systemd user unit 内容。
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
func RenderRunCommand(exe, cfgPath string) string {
	return fmt.Sprintf(`"%s" start -c "%s"`, exe, cfgPath)
}
