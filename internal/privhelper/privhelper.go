// Package privhelper 是 proxyd 的统一 root 特权助手（macOS LaunchDaemon）：
// 一个常驻进程同时为各功能模块的 unix socket 提供特权操作（当前为 TUN 设备
// 创建与 LAN 网关 pf/转发管理），取代早期每模块一个 LaunchDaemon 的形态
// （com.proxyd.tun-helper / com.proxyd.gateway-helper）。
// 各模块的 socket 路径、协议、指令集与看门狗不变，仍由模块包实现并随
// `proxyd helper` 子命令在同一进程内启动；本包只负责 plist 渲染、
// 安装/卸载编排（含旧版 helper 迁移）与属主 UID 记录解析。
package privhelper

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	// Label 是统一特权助手的 launchd 服务标识。
	Label = "com.proxyd.helper"
	// PlistPath 是统一助手的 LaunchDaemon plist 路径。
	PlistPath = "/Library/LaunchDaemons/" + Label + ".plist"
	// OwnerUIDEnv 是安装链路经 plist 环境变量记录的属主 UID。
	OwnerUIDEnv = "PROXYD_HELPER_OWNER_UID"
	// HelperSubcommand 是 plist ProgramArguments 中启动统一服务端的子命令。
	HelperSubcommand = "helper"
)

// 旧版按模块拆分的 helper 标识；安装/卸载统一助手时一并迁移清理（幂等）。
const (
	// LegacyTunLabel 是旧版 TUN helper 的 launchd 服务标识。
	LegacyTunLabel = "com.proxyd.tun-helper"
	// LegacyGatewayLabel 是旧版网关 helper 的 launchd 服务标识。
	LegacyGatewayLabel = "com.proxyd.gateway-helper"
	// LegacyTunPlistPath 是旧版 TUN helper 的 plist 路径。
	LegacyTunPlistPath = "/Library/LaunchDaemons/" + LegacyTunLabel + ".plist"
	// LegacyGatewayPlistPath 是旧版网关 helper 的 plist 路径。
	LegacyGatewayPlistPath = "/Library/LaunchDaemons/" + LegacyGatewayLabel + ".plist"
)

// ErrUnsupported 表示当前平台不支持统一特权助手（非 macOS 平台）。
var ErrUnsupported = errors.New("当前平台不支持统一特权助手")

// helperRun 执行外部命令；包级变量便于测试替换。
var helperRun = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// RenderPlist 生成统一助手 LaunchDaemon 的 plist 内容。
//
// 参数：
//   - exePath: string，proxyd 二进制绝对路径（helper 与主进程同二进制）。
//   - ownerUID: int，安装时记录的属主 UID（helper 只放行该用户与 root）。
//
// 返回值：
//   - string：完整 plist 文本；RunAtLoad + KeepAlive 保证常驻。
//
// 错误情况：无；路径经 XML 转义后嵌入。
func RenderPlist(exePath string, ownerUID int) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(exePath) + `</string>
		<string>` + HelperSubcommand + `</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>` + OwnerUIDEnv + `</key>
		<string>` + strconv.Itoa(ownerUID) + `</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/var/log/` + Label + `.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/` + Label + `.log</string>
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

// Installed 报告统一助手 plist 是否已安装。参数：无。返回：bool。
// 错误：查询失败按未安装处理。
func Installed() bool {
	_, err := os.Stat(PlistPath)
	return err == nil
}

// Running 报告统一助手是否已被 launchd 加载。参数：无。返回：bool。
// 错误：非 darwin 无 launchctl，查询失败恒为 false。
func Running() bool {
	_, err := helperRun("/bin/launchctl", "print", "system/"+Label)
	return err == nil
}

// LegacyInstalled 报告是否存在旧版按模块拆分的 helper 残留（用于迁移提示）。
// 参数：无。返回：bool。错误：查询失败按无残留处理。
func LegacyInstalled() bool {
	for _, path := range []string{LegacyTunPlistPath, LegacyGatewayPlistPath} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// ResolveOwnerUID 读取安装链路记录的属主 UID：优先统一变量 OwnerUIDEnv，
// 回退模块旧版变量（旧 plist 在重装统一助手前仍可运行）。
//
// 参数：
//   - getenv: func(string) string，环境变量读取（测试注入）。
//   - legacyEnv: string，模块旧版属主环境变量名。
//   - installHint: string，属主缺失时给出的安装指引（如 "proxyd helper install"）。
//
// 返回值：
//   - int：允许连接的非 root 属主 UID。
//   - error：环境变量缺失或非法时返回；helper 拒绝在无属主记录的情况下运行。
//
// 错误情况：属主缺失意味着 helper 被手工/异常启动，直接拒绝服务。
func ResolveOwnerUID(getenv func(string) string, legacyEnv, installHint string) (int, error) {
	raw := strings.TrimSpace(getenv(OwnerUIDEnv))
	if raw == "" {
		raw = strings.TrimSpace(getenv(legacyEnv))
	}
	if raw == "" {
		return -1, fmt.Errorf("缺少属主记录（环境变量 %s）；请通过 %s 安装", OwnerUIDEnv, installHint)
	}
	uid, err := strconv.Atoi(raw)
	if err != nil || uid < 0 {
		return -1, fmt.Errorf("属主 UID %q 非法", raw)
	}
	return uid, nil
}
