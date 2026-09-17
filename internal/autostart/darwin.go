//go:build darwin

package autostart

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

// macOS 使用系统级 LaunchDaemon：plist 固定安装到 /Library/LaunchDaemons，
// 因而无需用户登录即可在冷启动、断电恢复或系统重启后拉起 proxyd。始终通过
// UserName 降权为注册时的普通用户（TUN 由 tun-helper 代劳，不再需要 root 实例），
// 避免改变配置文件和状态目录的所有权。KeepAlive 仅崩溃拉起（SuccessfulExit=false），
// 干净退出（proxyd stop）保持停止。

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
	return validateServiceAccount(current)
}

// namedServiceAccount 解析显式指定的 LaunchDaemon 降权账户（旧 root 自启项迁移用）。
//
// 参数说明：
//   - name: string，本机账户名。
//
// 返回值说明：serviceAccount 和 error；语义同 currentServiceAccount。
//
// 错误情况：账户不存在或信息不完整时返回错误。
func namedServiceAccount(name string) (serviceAccount, error) {
	current, err := user.Lookup(name)
	if err != nil {
		return serviceAccount{}, fmt.Errorf("解析 LaunchDaemon 运行账户 %q 失败: %w", name, err)
	}
	return validateServiceAccount(current)
}

// validateServiceAccount 校验账户信息完整性并构造 serviceAccount。
func validateServiceAccount(current *user.User) (serviceAccount, error) {
	if strings.TrimSpace(current.Username) == "" || strings.TrimSpace(current.HomeDir) == "" || strings.TrimSpace(current.Uid) == "" {
		return serviceAccount{}, fmt.Errorf("LaunchDaemon 运行账户信息不完整")
	}
	return serviceAccount{UserName: current.Username, HomeDir: current.HomeDir, UID: current.Uid}, nil
}

// RootDaemonPlistInstalled 报告系统自启项是否仍是旧的 root 模式（plist 无 UserName，
// tun-helper 落地前 TUN 需要 root 时的产物），供启动期迁移为降权运行。
//
// 参数说明：无。
//
// 返回值说明：bool，plist 存在且不含 UserName 键时为 true。
//
// 错误情况：无；读取失败按非 root 模式处理，不做迁移。
func RootDaemonPlistInstalled() bool {
	data, err := os.ReadFile(daemonPlistPath)
	return err == nil && !strings.Contains(string(data), "<key>UserName</key>")
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

// runDeferred 经 /bin/sh 延迟约 1 秒执行固定命令并立即返回。
//
// 参数说明：
//   - name: string，待执行命令的绝对路径。
//   - args: ...string，按 renderShellCommand 规则转义的命令参数。
//
// 返回值说明：无；函数只负责安排延迟任务，不等待目标命令完成。
//
// 错误情况：进程无法启动时记录日志后放弃，保持 best-effort 语义；
// 启动成功后必须在协程中 Wait，否则长期运行的 proxyd 会积累未回收子进程。
// 声明为变量以便测试替换。子进程不继承父进程的标准流。
var runDeferred = func(name string, args ...string) {
	script := "sleep 1; exec " + renderShellCommand(privilegedCommand{Name: name, Args: args})
	cmd := exec.Command("/bin/sh", "-c", script)
	if err := cmd.Start(); err != nil {
		log.Printf("[autostart] 启动延迟清理命令失败: %v", err)
		return
	}
	// Start 后的子进程必须 Wait 才能回收 PID 与内核计账资源。
	// 在独立协程等待不会延迟 HTTP 响应，也不需要 shell 再派生一层
	// 无人回收的后台子进程。
	go func() {
		_ = cmd.Wait()
	}()
}

// on 安装系统级 LaunchDaemon 并立即启动服务。
//
// 参数说明：
//   - opt: Options，包含 proxyd、配置文件和状态目录的绝对路径。
//
// 返回值说明：error，plist 安装、system 域注册与旧 LaunchAgent 清理全部成功时为 nil。
//
// 错误情况：目录/临时文件创建、管理员授权、launchctl bootstrap 或旧项清理失败时
// 返回错误。管理员取消授权不会绕过系统权限模型。注册命令链的取舍见
// daemonRegisterCommands：健康服务只刷新文件，崩溃循环服务强制重注册。
func on(opt Options) error {
	account, err := currentServiceAccount()
	if opt.RunAsUser != "" {
		account, err = namedServiceAccount(opt.RunAsUser)
	}
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
	content := RenderPlist(opt.Exe, opt.ConfigPath, logPath, account.UserName, account.HomeDir, opt.RootDaemon)
	if _, err := temporary.WriteString(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("写入 LaunchDaemon 临时文件失败: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭 LaunchDaemon 临时文件失败: %w", err)
	}

	if err := runPrivileged(account, daemonRegisterCommands(temporaryPath, inspect())...); err != nil {
		return fmt.Errorf("注册系统 LaunchDaemon 失败: %w", err)
	}
	// 旧 LaunchAgent 需要连实例一起停掉，否则与新 bootstrap 的 LaunchDaemon 双实例运行；
	// 延迟 bootout 让本次请求（可能正由旧实例处理）先返回。
	if err := removeLegacyLaunchAgent(account, true); err != nil {
		return fmt.Errorf("系统 LaunchDaemon 已启用，但旧 LaunchAgent 清理失败: %w", err)
	}
	return nil
}

// daemonRegisterCommands 根据服务当前状态构造注册命令链。
//
// 参数说明：
//   - temporaryPath: string，待安装 plist 的临时文件路径。
//   - s: RuntimeStatus，inspect() 的当前快照。
//
// 返回值说明：[]privilegedCommand，按顺序执行的安装与注册命令。
// 未注册时直接 bootstrap（立即启动 RunAtLoad 服务）；已注册且健康运行只刷新
// plist 文件（不打扰可能正在处理本次请求的当前进程）；已注册但未运行一律
// bootout 后重新 bootstrap：崩溃循环要丢弃旧 LWCR 约束，干净停止（KeepAlive
// 仅崩溃拉起）也只有重新 bootstrap 才会启动；bootout 加 IgnoreError 兼容
// 「刚 off 过、定义已不在内存」。
//
// 错误情况：无；命令执行错误由 runPrivileged 返回。
func daemonRegisterCommands(temporaryPath string, s RuntimeStatus) []privilegedCommand {
	commands := []privilegedCommand{
		{Name: "/usr/bin/install", Args: []string{"-o", "root", "-g", "wheel", "-m", "0644", temporaryPath, daemonPlistPath}},
	}
	switch {
	case !s.Loaded:
		commands = append(commands,
			privilegedCommand{Name: "/bin/launchctl", Args: []string{"enable", "system/" + plistLabel}},
			privilegedCommand{Name: "/bin/launchctl", Args: []string{"bootstrap", "system", daemonPlistPath}},
		)
	case !s.Running:
		// 已注册但未运行都要重注册：崩溃循环（如 LWCR 签名失效）需要 bootout 让
		// launchd 丢弃旧约束；干净停止（proxyd stop）则因为 KeepAlive 仅崩溃拉起，
		// 不重新 bootstrap 服务永远不会启动。bootout 加 IgnoreError 兼容定义已卸载。
		commands = append(commands,
			privilegedCommand{Name: "/bin/launchctl", Args: []string{"bootout", "system/" + plistLabel}, IgnoreError: true},
			privilegedCommand{Name: "/bin/launchctl", Args: []string{"enable", "system/" + plistLabel}},
			privilegedCommand{Name: "/bin/launchctl", Args: []string{"bootstrap", "system", daemonPlistPath}},
		)
	}
	return commands
}

// off 卸载系统级开机自启项，同时清理旧版登录自启项。
//
// 参数说明：无。
//
// 返回值说明：error，目标不存在或系统项与旧项均清理成功时为 nil。
//
// 错误情况：账户解析、管理员授权或 plist 删除失败时返回错误。
// 只删除 plist 文件而不 bootout：launchd 内存中的服务定义保留到本次开机结束，
// 正在运行的实例（可能就是处理本次请求的本进程）不受影响，重启后不再拉起。
func off() error {
	account, err := currentServiceAccount()
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(daemonPlistPath); statErr == nil {
		command := privilegedCommand{Name: "/bin/rm", Args: []string{"-f", daemonPlistPath}}
		if err := runPrivileged(account, command); err != nil {
			return fmt.Errorf("卸载系统 LaunchDaemon 失败: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("查询 LaunchDaemon 失败: %w", statErr)
	}
	return removeLegacyLaunchAgent(account, false)
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

// removeLegacyLaunchAgent 删除旧版用户登录自启项，避免与 LaunchDaemon 双实例运行。
//
// 参数说明：
//   - account: serviceAccount，旧 LaunchAgent 所属用户及其 launchd GUI domain。
//   - stopRunning: bool，为 true 时延迟 bootout 旧实例（on 切换场景，避免双实例）；
//     为 false 时只删文件（off 场景，正在运行的实例不受影响）。
//
// 返回值说明：error，旧项不存在或成功删除时为 nil。
//
// 错误情况：旧 plist 存在但无法删除时返回错误。bootout 以 service-target 形式延迟
// 执行：当前进程若正是该 LaunchAgent 托管的实例，同步 bootout 会在删除完成前
// 终止本进程；延迟执行也让本次 API 响应先返回。
func removeLegacyLaunchAgent(account serviceAccount, stopRunning bool) error {
	path := legacyPlistPath(account)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("删除旧 LaunchAgent %s 失败: %w", path, err)
	}
	if stopRunning {
		runDeferred("/bin/launchctl", "bootout", "gui/"+account.UID+"/"+plistLabel)
	}
	return nil
}

// runPrivileged 执行一组系统级服务命令；root 直接执行，普通用户按「终端优先」：
// stdin 是交互终端时经 sudo 在终端完成密码输入（CLI 场景体验一致、可审计）；
// 非终端（Web 控制台触发）经 osascript 管理员授权对话框执行，系统域进程
// （LaunchDaemon 托管）弹不出窗时回退到用户图形会话。
//
// 参数说明：
//   - account: serviceAccount，授权回退路径需要的运行账户及其图形会话 domain。
//   - commands: ...privilegedCommand，按顺序执行的固定命令与参数。
//
// 返回值说明：error，全部命令成功时为 nil。
//
// 错误情况：命令失败、管理员拒绝授权或 osascript 不可用时返回包含操作上下文的错误。
// 当前进程处于系统域（LaunchDaemon 托管）时窗口服务器不可达，授权对话框无法弹出
// （osascript 报 -60007），此时回退到 runPrivilegedInGUISession 经用户图形会话执行。
func runPrivileged(account serviceAccount, commands ...privilegedCommand) error {
	if os.Geteuid() == 0 {
		for _, command := range commands {
			if _, err := run(command.Name, command.Args...); err != nil && !command.IgnoreError {
				return err
			}
		}
		return nil
	}
	if stdioIsTerminal() {
		if err := runViaSudo(commands...); err == nil {
			return nil
		} else if !errors.Is(err, errSudoUnavailable) {
			// sudo 可用但执行失败（含用户取消授权）：不再叠加 GUI 弹窗二次打扰。
			return err
		}
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
	log.Printf("[autostart] 即将弹出管理员授权对话框，请输入密码完成授权；若未看到对话框，请在「终端」中重跑本命令改用 sudo 授权")
	if _, err := run("/usr/bin/osascript", "-e", appleScript); err != nil {
		if interactionNotAllowed(err) {
			return runPrivilegedInGUISession(shellScript, account)
		}
		return fmt.Errorf("需要管理员授权: %w", err)
	}
	return nil
}

// errSudoUnavailable 表示系统没有 sudo 可执行文件，调用方应回退到 osascript 通道。
var errSudoUnavailable = errors.New("sudo 不可用")

// stdioIsTerminal 探测 stdin 是否为可交互终端；声明为变量以便测试替换。
// 必须用 ioctl 级判定（term.IsTerminal）：ModeCharDevice 会把 /dev/null 误判为
// 终端——LaunchDaemon 等托管进程的 stdin 正是 /dev/null，误判会让 sudo 在无终端
// 环境下直接以「a password is required」失败，而不是回退到 GUI 授权。
var stdioIsTerminal = func() bool { return fdIsTerminal(os.Stdin.Fd()) }

// fdIsTerminal 判定文件描述符是否为终端。参数：fd 为 uintptr 文件描述符。
// 返回：bool。错误：无；非终端（含 /dev/null、管道、正则文件）一律为 false。
func fdIsTerminal(fd uintptr) bool { return term.IsTerminal(int(fd)) }

// sudoLookPath 与 runTerminalCommand 是终端 sudo 通道的可注入点，测试替换。
var sudoLookPath = func() (string, error) { return exec.LookPath("sudo") }
var runTerminalCommand = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runViaSudo 经终端 sudo 逐条执行特权命令（密码提示由 sudo 在终端完成，
// 标准流直接挂到终端）。sudo 二进制缺失时返回 errSudoUnavailable。
//
// 参数说明：
//   - commands: ...privilegedCommand，按顺序执行的固定命令与参数。
//
// 返回值说明：error，全部命令成功时为 nil。
//
// 错误情况：命令失败（含用户在 sudo 提示下取消）返回带命令名的错误；
// IgnoreError 命令的失败被忽略，与 runPrivileged 的语义一致。
func runViaSudo(commands ...privilegedCommand) error {
	sudo, err := sudoLookPath()
	if err != nil {
		return errSudoUnavailable
	}
	for _, command := range commands {
		if err := runTerminalCommand(sudo, append([]string{command.Name}, command.Args...)...); err != nil && !command.IgnoreError {
			return fmt.Errorf("sudo %s 失败（已取消或鉴权失败）: %w", command.Name, err)
		}
	}
	return nil
}

// interactionNotAllowed 判断 osascript 失败是否源于当前会话无法弹出授权窗口。
//
// 参数说明：
//   - err: error，run 返回的包含 osascript 标准错误摘要的错误。
//
// 返回值说明：bool，错误文本包含 -60007（errAuthorizationInteractionNotAllowed）时为 true。
//
// 错误情况：无；仅做文本匹配，错误为 nil 时返回 false。
func interactionNotAllowed(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "-60007") || strings.Contains(text, "InteractionNotAllowed") ||
		strings.Contains(text, "interaction is not allowed")
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
