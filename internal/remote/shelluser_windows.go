//go:build windows

package remote

// 本文件提供 Windows 的远程会话用户解析：不支持 shell-user 降权，
// 会话始终以 proxyd 进程用户运行（Windows TUN 也不触发 Unix 的 root 冲突）。

import (
	"fmt"
	"os/user"
	"strings"
)

// sessionUser 是一次远程 shell 会话的运行身份；Windows 仅保留账户记录。
type sessionUser struct {
	user *user.User
}

// resolveSessionUser 解析远程会话应使用的本机用户。
// 参数说明：shellUser 为 string，配置 remote.shell-user 原值。
// 返回值说明：*sessionUser 和 error；配置 shell-user 时返回不支持错误。
// 错误情况：shell-user 非空时报平台不支持；否则返回进程用户。
func resolveSessionUser(shellUser string) (*sessionUser, error) {
	if strings.TrimSpace(shellUser) != "" {
		return nil, fmt.Errorf("remote.shell-user 暂不支持 Windows")
	}
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("获取当前用户失败: %w", err)
	}
	return &sessionUser{user: u}, nil
}
