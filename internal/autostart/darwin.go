//go:build darwin

package autostart

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// macOS 使用系统级 LaunchDaemon：plist 固定安装到 /Library/LaunchDaemons，
// 因而无需用户登录即可在冷启动、断电恢复或系统重启后拉起 proxyd。服务本身通过
// UserName 降权为注册时的普通用户，避免改变配置文件和状态目录的所有权。

const daemonPlistPath = "/Library/LaunchDaemons/" + plistLabel + ".plist"

// serviceAccount 描述 LaunchDaemon 实际运行使用的本机账户。
type serviceAccount struct {
	UserName string
	HomeDir  string
	UID      string
}

// privilegedCommand 是一次必须以管理员权限执行的固定外部命令。
type privilegedCommand struct {
	Name        string
	Args        []string
	IgnoreError bool
}

// currentServiceAccount 解析应由 LaunchDaemon 使用的账户。
//
// 参数说明：无。
//
// 返回值说明：serviceAccount 和 error；优先使用 sudo 调用者，否则使用当前进程账户。
//
// 错误情况：系统用户数据库不可用、账户没有用户名/主目录/UID 时返回错误，避免生成
// 会以错误身份运行或无法读取用户配置的系统服务。
func currentServiceAccount() (serviceAccount, error) {
	var current *user.User
	var err error
	if sudoUser := strings.TrimSpace(os.Getenv("SUDO_USER")); os.Geteuid() == 0 && sudoUser != "" && sudoUser != "root" {
		current, err = user.Lookup(sudoUser)
	} else {
		current, err = user.Current()
	}
	if err != nil {
		return serviceAccount{}, fmt.Errorf("解析 LaunchDaemon 运行账户失败: %w", err)
	}
	if strings.TrimSpace(current.Username) == "" || strings.TrimSpace(current.HomeDir) == "" || strings.TrimSpace(current.Uid) == "" {
		return serviceAccount{}, fmt.Errorf("LaunchDaemon 运行账户信息不完整")
	}
	return serviceAccount{UserName: current.Username, HomeDir: current.HomeDir, UID: current.Uid}, nil
}

// legacyPlistPath 返回旧版登录级 LaunchAgent 的路径。
//
// 参数说明：
//   - account: serviceAccount，旧自启项所属账户。
//
// 返回值说明：string，旧 plist 的绝对路径。
//
// 错误情况：无；账户主目录已由 currentServiceAccount 校验。
func legacyPlistPath(account serviceAccount) string {
	return filepath.Join(account.HomeDir, "Library", "LaunchAgents", plistLabel+".plist")
}

// on 安装系统级 LaunchDaemon 并立即启动服务。
//
// 参数说明：
//   - opt: Options，包含 proxyd、配置文件和状态目录的绝对路径。
//
// 返回值说明：error，plist 安装、system 域注册与旧 LaunchAgent 清理全部成功时为 nil。
//
// 错误情况：目录/临时文件创建、管理员授权、launchctl bootstrap 或旧项清理失败时
// 返回错误。管理员取消授权不会绕过系统权限模型。
func on(opt Options) error {
	account, err := currentServiceAccount()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opt.StateDir, 0o755); err != nil {
		return fmt.Errorf("创建状态目录失败: %w", err)
	}
	logPath := filepath.Join(opt.StateDir, "proxyd.log")
	temporary, err := os.CreateTemp("", "proxyd-launchdaemon-*.plist")
	if err != nil {
		return fmt.Errorf("创建 LaunchDaemon 临时文件失败: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	content := RenderPlist(opt.Exe, opt.ConfigPath, logPath, account.UserName, account.HomeDir)
	if _, err := temporary.WriteString(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("写入 LaunchDaemon 临时文件失败: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭 LaunchDaemon 临时文件失败: %w", err)
	}

	// 先卸载可能存在的旧 daemon，再以 root:wheel、0644 安装新 plist。bootstrap
	// system 会立即启动 RunAtLoad 服务；enable 清除用户曾通过 launchctl 设置的禁用标记。
	commands := []privilegedCommand{
		{Name: "/bin/launchctl", Args: []string{"bootout", "system/" + plistLabel}, IgnoreError: true},
		{Name: "/usr/bin/install", Args: []string{"-o", "root", "-g", "wheel", "-m", "0644", temporaryPath, daemonPlistPath}},
		{Name: "/bin/launchctl", Args: []string{"enable", "system/" + plistLabel}},
		{Name: "/bin/launchctl", Args: []string{"bootstrap", "system", daemonPlistPath}},
	}
	if err := runPrivileged(commands...); err != nil {
		return err
	}
	if err := removeLegacyLaunchAgent(account); err != nil {
		return fmt.Errorf("系统 LaunchDaemon 已启用，但旧 LaunchAgent 清理失败: %w", err)
	}
	return nil
}

// off 卸载并删除系统级 LaunchDaemon，同时清理旧版登录自启项。
//
// 参数说明：无。
//
// 返回值说明：error，目标不存在或系统项与旧项均清理成功时为 nil。
//
// 错误情况：账户解析、管理员授权、launchctl 卸载或 plist 删除失败时返回错误。
func off() error {
	account, err := currentServiceAccount()
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(daemonPlistPath); statErr == nil {
		commands := []privilegedCommand{
			{Name: "/bin/launchctl", Args: []string{"bootout", "system/" + plistLabel}, IgnoreError: true},
			{Name: "/bin/rm", Args: []string{"-f", daemonPlistPath}},
		}
		if err := runPrivileged(commands...); err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("查询 LaunchDaemon 失败: %w", statErr)
	}
	return removeLegacyLaunchAgent(account)
}

// status 报告系统级 LaunchDaemon plist 是否已安装。
//
// 参数说明：无。
//
// 返回值说明：bool 和 error；仅 /Library/LaunchDaemons 中的新语义服务算作已开启。
//
// 错误情况：plist 状态查询失败时返回错误；旧 LaunchAgent 不再被视为有效开机自启。
func status() (bool, error) {
	_, err := os.Stat(daemonPlistPath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// removeLegacyLaunchAgent 卸载并删除旧版用户登录自启项，避免与 LaunchDaemon 双实例运行。
//
// 参数说明：
//   - account: serviceAccount，旧 LaunchAgent 所属用户及其 launchd GUI domain。
//
// 返回值说明：error，旧项不存在或成功删除时为 nil。
//
// 错误情况：旧 plist 存在但无法删除时返回错误；bootout/unload 的“未加载”错误可忽略。
func removeLegacyLaunchAgent(account serviceAccount) error {
	path := legacyPlistPath(account)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	domain := "gui/" + account.UID
	_, _ = run("/bin/launchctl", "bootout", domain, path)
	_, _ = run("/bin/launchctl", "unload", "-w", path)
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("删除旧 LaunchAgent %s 失败: %w", path, err)
	}
	return nil
}

// runPrivileged 执行一组系统级服务命令；root 直接执行，普通用户通过 macOS
// 管理员授权对话框一次性执行，兼容 CLI 与 Web 设置页触发场景。
//
// 参数说明：
//   - commands: ...privilegedCommand，按顺序执行的固定命令与参数。
//
// 返回值说明：error，全部必需命令成功时为 nil。
//
// 错误情况：命令失败、管理员拒绝授权或 osascript 不可用时返回包含操作上下文的错误。
func runPrivileged(commands ...privilegedCommand) error {
	if os.Geteuid() == 0 {
		for _, command := range commands {
			if _, err := run(command.Name, command.Args...); err != nil && !command.IgnoreError {
				return fmt.Errorf("注册系统 LaunchDaemon 失败: %w", err)
			}
		}
		return nil
	}
	parts := make([]string, 0, len(commands))
	for _, command := range commands {
		part := renderShellCommand(command)
		if command.IgnoreError {
			part = "(" + part + " >/dev/null 2>&1 || true)"
		}
		parts = append(parts, part)
	}
	shellScript := strings.Join(parts, " && ")
	appleScript := `do shell script "` + escapeAppleScript(shellScript) + `" with administrator privileges`
	if _, err := run("/usr/bin/osascript", "-e", appleScript); err != nil {
		return fmt.Errorf("注册系统 LaunchDaemon 需要管理员授权: %w", err)
	}
	return nil
}

// renderShellCommand 把命令与参数渲染为可交给 do shell script 的安全文本。
//
// 参数说明：
//   - command: privilegedCommand，命令路径和独立参数。
//
// 返回值说明：string，每一段均经过 POSIX shell 单引号转义的命令行。
//
// 错误情况：无；输入不会未经引用进入 shell，从而避免路径字符被解释为操作符。
func renderShellCommand(command privilegedCommand) string {
	parts := make([]string, 0, len(command.Args)+1)
	parts = append(parts, quoteShellArgument(command.Name))
	for _, argument := range command.Args {
		parts = append(parts, quoteShellArgument(argument))
	}
	return strings.Join(parts, " ")
}

// quoteShellArgument 使用 POSIX 单引号规则引用一个完整 shell 参数。
//
// 参数说明：
//   - value: string，未经信任边界处理的单个参数。
//
// 返回值说明：string，可安全拼入 /bin/sh 命令行的单一参数。
//
// 错误情况：无；单引号通过结束引用、插入转义单引号、重新开始引用的方式编码。
func quoteShellArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// escapeAppleScript 转义 AppleScript 双引号字符串中的反斜杠和双引号。
//
// 参数说明：
//   - value: string，已经完成 shell 参数引用的完整命令文本。
//
// 返回值说明：string，可安全放入 do shell script 双引号字面量的内容。
//
// 错误情况：无；换行不会由本函数生成，调用方只传入单行命令。
func escapeAppleScript(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `"`, `\"`)
}
