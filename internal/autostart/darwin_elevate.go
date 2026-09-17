//go:build darwin

package autostart

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// elevateLabel 是一次性提权助手 LaunchAgent 的标识；与主服务区分避免误清理。
const elevateLabel = plistLabel + ".elevate"

const (
	// elevatePollInterval 是轮询授权结果文件的间隔。
	elevatePollInterval = 250 * time.Millisecond
	// elevateTimeout 是等待用户完成管理员授权对话框的最长时间。
	elevateTimeout = 3 * time.Minute
)

// runPrivilegedInGUISession 在当前进程无法直接弹出管理员授权窗口时（进程由系统域
// LaunchDaemon 托管，无窗口服务器访问权限），把授权脚本交给用户图形会话中的一次性
// LaunchAgent 执行：该会话内 osascript 可以正常展示 SecurityAgent 密码对话框。
//
// 参数说明：
//   - shellScript: string，已经完成参数引用、需以管理员身份执行的 shell 命令链。
//   - account: serviceAccount，提供主目录与 gui/<uid> 目标 domain。
//
// 返回值说明：error，授权命令链全部成功时为 nil。
//
// 错误情况：助手文件写入、launchctl bootstrap（用户未登录图形会话）、授权被拒绝
// 或等待超时返回错误；超时会 bootout 助手，对话框随之关闭。助手 wrapper 结束时会
// 自删 plist，即使本进程随后被延迟 bootout 终止也不会残留登录项。
func runPrivilegedInGUISession(shellScript string, account serviceAccount) error {
	dir, err := os.MkdirTemp(filepath.Join(account.HomeDir, "Library", "Caches"), "proxyd-elevate-*")
	if err != nil {
		return fmt.Errorf("创建提权助手目录失败: %w", err)
	}
	defer os.RemoveAll(dir)

	appleScriptPath := filepath.Join(dir, "elevate.applescript")
	appleScript := `do shell script "` + escapeAppleScript(shellScript) + `" with administrator privileges` + "\n"
	if err := os.WriteFile(appleScriptPath, []byte(appleScript), 0o600); err != nil {
		return fmt.Errorf("写入提权脚本失败: %w", err)
	}

	resultPath := filepath.Join(dir, "result")
	stderrPath := filepath.Join(dir, "stderr")
	plistPath := filepath.Join(account.HomeDir, "Library", "LaunchAgents", elevateLabel+".plist")
	wrapperPath := filepath.Join(dir, "wrapper.sh")
	wrapper := "#!/bin/sh\n" +
		"/usr/bin/osascript " + quoteShellArgument(appleScriptPath) + " 2>" + quoteShellArgument(stderrPath) + "\n" +
		`printf '%s' "$?" >` + quoteShellArgument(resultPath) + "\n" +
		"/bin/rm -f " + quoteShellArgument(plistPath) + "\n"
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o700); err != nil {
		return fmt.Errorf("写入提权助手脚本失败: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("创建 LaunchAgents 目录失败: %w", err)
	}
	if err := os.WriteFile(plistPath, []byte(renderElevatePlist(wrapperPath)), 0o644); err != nil {
		return fmt.Errorf("写入提权助手 plist 失败: %w", err)
	}
	defer os.Remove(plistPath)

	domain := "gui/" + account.UID
	// 清掉可能残留的同名助手（上次异常退出），再注册本次任务。
	_, _ = run("/bin/launchctl", "bootout", domain+"/"+elevateLabel)
	log.Printf("[autostart] 已请求在用户图形会话弹出管理员授权窗口，请留意密码对话框；若未看到对话框，请在「终端」中重跑本命令改用 sudo 授权")
	if _, err := run("/bin/launchctl", "bootstrap", domain, plistPath); err != nil {
		return fmt.Errorf("当前用户未登录图形会话，无法弹出授权窗口；请在「终端」中直接执行 sudo 命令: %w", err)
	}
	defer func() { _, _ = run("/bin/launchctl", "bootout", domain+"/"+elevateLabel) }()

	deadline := time.Now().Add(elevateTimeout)
	for {
		if data, readErr := os.ReadFile(resultPath); readErr == nil {
			if code, _ := strconv.Atoi(strings.TrimSpace(string(data))); code == 0 {
				return nil
			}
			detail, _ := os.ReadFile(stderrPath)
			return fmt.Errorf("管理员授权失败: %s", strings.TrimSpace(string(detail)))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待管理员授权超时（%v）：未检测到密码输入，授权窗口可能未弹出或被忽略；请改在「终端」中直接执行本命令，将改用 sudo 在终端内输入密码", elevateTimeout)
		}
		time.Sleep(elevatePollInterval)
	}
}

// renderElevatePlist 生成一次性提权助手 LaunchAgent 的 plist 内容。
//
// 参数说明：
//   - wrapperPath: string，包装脚本的绝对路径。
//
// 返回值说明：string，仅含 Label/ProgramArguments/RunAtLoad 的最小 plist。
//
// 错误情况：无；路径经 plistEscape 转义后嵌入。
func renderElevatePlist(wrapperPath string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + elevateLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>` + plistEscape(wrapperPath) + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`
}

// PrivilegedCommand 是一次必须以管理员权限执行的固定外部命令
// （gateway helper 安装链路等复用本提权通道）。
type PrivilegedCommand struct {
	Name        string
	Args        []string
	IgnoreError bool
}

// RunPrivilegedCommands 以管理员权限顺序执行固定命令链：当前进程为 root 时
// 直接执行，普通用户经 osascript 管理员授权对话框执行（系统域进程回退到用户
// 图形会话）。命令参数经单引号转义，不接受任意脚本文本。
//
// 参数说明：
//   - commands: ...PrivilegedCommand，按顺序执行的固定命令与参数。
//
// 返回值说明：error，全部命令成功时为 nil。
//
// 错误情况：命令失败、管理员拒绝授权或会话无法弹窗时返回带上下文的错误。
func RunPrivilegedCommands(commands ...PrivilegedCommand) error {
	account, err := currentServiceAccount()
	if err != nil {
		return err
	}
	mapped := make([]privilegedCommand, 0, len(commands))
	for _, command := range commands {
		mapped = append(mapped, privilegedCommand(command))
	}
	return runPrivileged(account, mapped...)
}
