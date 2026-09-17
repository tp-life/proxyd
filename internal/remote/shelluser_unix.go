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
)

// sessionUser 是一次远程 shell 会话的运行身份：账户记录加可选的降权凭据。
// cred 为 nil 表示子进程沿用进程自身身份，无需 setuid 切换。
type sessionUser struct {
	user *user.User
	cred *syscall.Credential
}

// resolveSessionUser 解析远程会话应使用的本机用户。
// 参数说明：shellUser 为 string，配置 remote.shell-user 原值，可为空。
// 返回值说明：*sessionUser 和 error；账户不存在或 root 未配置 shell-user 时返回错误。
// 错误情况：root 运行且未配置 shell-user 时 fail-closed（errRootShellUserRequired），
// 绝不默认提供 root shell；客户端 SSH 用户名从不参与解析。
func resolveSessionUser(shellUser string) (*sessionUser, error) {
	shellUser = strings.TrimSpace(shellUser)
	if shellUser == "" {
		if err := checkSessionUserAllowed(os.Geteuid(), ""); err != nil {
			return nil, err
		}
		u, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("获取当前用户失败: %w", err)
		}
		return &sessionUser{user: u}, nil
	}
	u, err := user.Lookup(shellUser)
	if err != nil {
		return nil, fmt.Errorf("远程会话用户 %q 不存在: %w", shellUser, err)
	}
	cred, err := sessionCredential(u)
	if err != nil {
		return nil, fmt.Errorf("解析远程会话用户 %q 凭据失败: %w", shellUser, err)
	}
	return &sessionUser{user: u, cred: cred}, nil
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
