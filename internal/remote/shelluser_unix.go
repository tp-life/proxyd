//go:build linux || darwin

package remote

// 本文件实现 Unix 远程会话用户解析：shell-user 配置账户经系统账户数据库解析，
// 子进程凭据（UID/GID/附加组）随会话命令降权；未配置且非 root 时保持进程用户。

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"proxyd/internal/config"
)

// sessionUser 是一次远程 shell 会话的运行身份：账户记录加可选的降权凭据。
// cred 为 nil 表示子进程沿用进程自身身份，无需 setuid 切换。
type sessionUser struct {
	user *user.User
	cred *syscall.Credential
}

// resolveSessionUser 解析远程会话应使用的本机用户（不接受客户端覆盖，用于 Web 终端等内部入口）。
// 参数说明：shellUser 为 string，配置 remote.shell-user 原值，可为空。
// 返回值说明：*sessionUser 和 error；账户不存在或纯 root 未配置 shell-user 时返回错误。
// 错误情况：纯 root 运行且无 shell-user 与普通 SUDO_USER 时 fail-closed（errRootShellUserRequired），
// 绝不默认提供 root shell。
func resolveSessionUser(shellUser string) (*sessionUser, error) {
	return resolveSessionUserForSSH(shellUser, "")
}

// resolveSessionUserForSSH 解析内嵌 SSH/SFTP/诊断会话应使用的本机用户。
// 优先级与安全边界：
//  1. 显式配置 remote.shell-user 时严格锁定到该非 root 账户，忽略客户端请求用户名。
//  2. 未配置 remote.shell-user 且通过 sudo 运行（euid == 0 且存在合法非 root SUDO_USER）：
//     若客户端指定了合法且存在的本机非 root 账户（如 proxyd ssh bob@host 或 -l bob），
//     则降权到该账户；否则（跨机连接未指定用户、或请求 root/不存在账户）自动回退到 SUDO_USER。
//  3. 未配置 remote.shell-user 且为纯 root（无合法 SUDO_USER）：fail-closed 返回 errRootShellUserRequired。
//  4. 非 root 进程运行时保持进程自身身份（无 setuid 权限）。
func resolveSessionUserForSSH(shellUser, requestedUser string) (*sessionUser, error) {
	return resolveSessionUserWithEnv(os.Geteuid(), shellUser, requestedUser, os.Getenv, user.Current, user.Lookup, sessionCredential)
}

// resolveSessionUserWithEnv 是 resolveSessionUserForSSH 的可注入依赖实现，便于单测覆盖 root/sudo 场景。
func resolveSessionUserWithEnv(
	euid int,
	shellUser, requestedUser string,
	getenv func(string) string,
	currentUser func() (*user.User, error),
	lookup func(string) (*user.User, error),
	buildCred func(*user.User) (*syscall.Credential, error),
) (*sessionUser, error) {
	shellUser = strings.TrimSpace(shellUser)
	if shellUser != "" {
		u, err := lookup(shellUser)
		if err != nil {
			return nil, fmt.Errorf("远程会话用户 %q 不存在: %w", shellUser, err)
		}
		if u.Uid == "0" {
			return nil, fmt.Errorf("远程会话用户 %q 不允许使用 root（降权目标必须是普通用户）", shellUser)
		}
		cred, err := buildCred(u)
		if err != nil {
			return nil, fmt.Errorf("解析远程会话用户 %q 凭据失败: %w", shellUser, err)
		}
		return &sessionUser{user: u, cred: cred}, nil
	}

	if euid == 0 {
		sudoUser := sudoInvokerUserFrom(euid, getenv, lookup)
		if sudoUser == "" {
			return nil, errRootShellUserRequired
		}
		var target *user.User
		if req := strings.TrimSpace(requestedUser); req != "" && !strings.EqualFold(req, "root") && req != "proxyd-web-terminal" && config.ValidateRemoteShellUser(req) == nil {
			if u, err := lookup(req); err == nil && u != nil && u.Uid != "0" {
				target = u
			}
		}
		if target == nil {
			u, err := lookup(sudoUser)
			if err != nil || u == nil || u.Uid == "0" {
				return nil, errRootShellUserRequired
			}
			target = u
		}
		cred, err := buildCred(target)
		if err != nil {
			return nil, fmt.Errorf("解析远程会话用户 %q 凭据失败: %w", target.Username, err)
		}
		return &sessionUser{user: target, cred: cred}, nil
	}

	u, err := currentUser()
	if err != nil {
		return nil, fmt.Errorf("获取当前用户失败: %w", err)
	}
	return &sessionUser{user: u}, nil
}

// sessionCredential 构造目标用户的子进程凭据；目标与进程 euid 相同时返回 nil。
// 参数说明：u 为 *user.User，服务端已确认的降权目标账户。
// 返回值说明：*syscall.Credential 和 error；含 UID、GID 与全部附加组（等价 initgroups）。
// 错误情况：UID/GID 不是数字或附加组查询失败时返回错误，不降级为缺失组的半身份。
func sessionCredential(u *user.User) (*syscall.Credential, error) {
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("UID %q 非法", u.Uid)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("GID %q 非法", u.Gid)
	}
	if int(uid) == os.Geteuid() {
		return nil, nil
	}
	var groups []uint32
	groupIDs, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("查询附加组失败: %w", err)
	}
	for _, text := range groupIDs {
		g, perr := strconv.ParseUint(text, 10, 32)
		if perr != nil {
			return nil, fmt.Errorf("附加组 %q 非法", text)
		}
		groups = append(groups, uint32(g))
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}, nil
}
