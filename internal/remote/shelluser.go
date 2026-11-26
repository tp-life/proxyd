package remote

// 本文件承载远程会话运行用户（shell-user）的平台无关规则：
// root 运行时必须显式配置降权账户，拒绝默认提供 root shell。

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strings"

	"proxyd/internal/config"
)

// errRootShellUserRequired 表示 root 运行下既未配置 shell-user 也无法识别普通调用者时的 fail-closed 拒绝：
// 内嵌 SSH 与 Web 终端绝不默认提供 root shell。
var errRootShellUserRequired = errors.New("proxyd 以 root 运行且未检测到默认普通用户：必须先在 remote.shell-user 配置远程会话用户，才能开启内嵌 SSH 或 Web 终端（拒绝默认提供 root shell）")

// defaultShellUserEnv 是系统服务（如 macOS LaunchDaemon）以 root 运行时向 proxyd 透传默认普通用户的环境变量名。
const defaultShellUserEnv = "PROXYD_SHELL_USER"

// sudoInvokerUser 返回以 sudo 或系统自启（PROXYD_SHELL_USER）启动 root 进程时的默认普通用户名。
// 非 root 运行、Windows 平台、变量为空/root/非法或系统账户不存在（或 UID 为 0）时返回空串。
func sudoInvokerUser() string {
	return sudoInvokerUserFrom(os.Geteuid(), os.Getenv, user.Lookup)
}

// sudoInvokerUserFrom 使用可注入的 euid、环境变量与账户查询函数解析 root 进程的默认普通用户。
// 优先使用 SUDO_USER，其次使用 PROXYD_SHELL_USER（macOS LaunchDaemon 注入）。
func sudoInvokerUserFrom(euid int, getenv func(string) string, lookup func(string) (*user.User, error)) string {
	if euid != 0 || runtime.GOOS == "windows" {
		return ""
	}
	for _, key := range []string{"SUDO_USER", defaultShellUserEnv} {
		name := strings.TrimSpace(getenv(key))
		if name == "" || strings.EqualFold(name, "root") {
			continue
		}
		if config.ValidateRemoteShellUser(name) != nil {
			continue
		}
		u, err := lookup(name)
		if err != nil || u == nil || u.Uid == "0" {
			continue
		}
		return u.Username
	}
	return ""
}

// checkSessionUserAllowed 判定当前进程权限下是否允许开启远程 shell 会话。
// 参数说明：euid 为 int，进程有效 UID（Windows 传 -1）；shellUser 为 string，配置原值。
// 返回值说明：error，允许时为 nil。
// 错误情况：euid 为 0、shell-user 为空且无有效普通 SUDO_USER 时返回 errRootShellUserRequired。
func checkSessionUserAllowed(euid int, shellUser string) error {
	return checkSessionUserAllowedWithSudo(euid, shellUser, sudoInvokerUserFrom(euid, os.Getenv, user.Lookup))
}

// checkSessionUserAllowedWithSudo 结合已校验的 sudo 普通用户判定是否允许开启远程 shell 会话。
func checkSessionUserAllowedWithSudo(euid int, shellUser, sudoUser string) error {
	if euid == 0 && strings.TrimSpace(shellUser) == "" && strings.TrimSpace(sudoUser) == "" {
		return errRootShellUserRequired
	}
	return nil
}

// sessionShellActive 判断配置是否启用了会创建本机 shell 的入口（内嵌 SSH 或 Web 终端）。
// 参数说明：cfg 为 config.RemoteConfig，待应用的配置快照。
// 返回值说明：bool，模块停用或两个入口都关闭时为 false。
// 错误情况：无。
func sessionShellActive(cfg config.RemoteConfig) bool {
	return !cfg.Disabled && (cfg.WebTerminal || (cfg.Enabled && cfg.BuiltinSSH))
}

// ValidateShellUser 校验 shell-user 可作为一个普通本机账户：存在且不是 root。
// 参数说明：name 为 string，用户提交的目标账户名（调用方已去除首尾空白并确认非空）。
// 返回值说明：error，账户可用于远程会话降权时为 nil。
// 错误情况：结构非法、平台不支持、账户不存在或 UID 为 0 时返回错误。
func ValidateShellUser(name string) error {
	if err := config.ValidateRemoteShellUser(name); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return fmt.Errorf("remote.shell-user 暂不支持 Windows")
	}
	u, err := user.Lookup(name)
	if err != nil {
		return fmt.Errorf("系统账户 %q 不存在: %w", name, err)
	}
	if u.Uid == "0" {
		return fmt.Errorf("shell-user 不允许使用 root（降权目标必须是普通用户）")
	}
	return nil
}
