package remote

// 本文件承载远程会话运行用户（shell-user）的平台无关规则：
// root 运行时必须显式配置降权账户，拒绝默认提供 root shell。

import (
	"errors"
	"fmt"
	"os/user"
	"runtime"
	"strings"

	"proxyd/internal/config"
)

// errRootShellUserRequired 表示 root 运行下未配置 shell-user 时的 fail-closed 拒绝：
// 内嵌 SSH 与 Web 终端绝不默认提供 root shell。
var errRootShellUserRequired = errors.New("proxyd 以 root 运行：必须先在 remote.shell-user 配置远程会话用户，才能开启内嵌 SSH 或 Web 终端（拒绝默认提供 root shell）")

// checkSessionUserAllowed 判定当前进程权限下是否允许开启远程 shell 会话。
// 参数说明：euid 为 int，进程有效 UID（Windows 传 -1）；shellUser 为 string，配置原值。
// 返回值说明：error，允许时为 nil。
// 错误情况：euid 为 0 且 shell-user 为空时返回 errRootShellUserRequired。
func checkSessionUserAllowed(euid int, shellUser string) error {
	if euid == 0 && strings.TrimSpace(shellUser) == "" {
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
