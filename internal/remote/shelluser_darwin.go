package remote

// 本文件查询 macOS 账户的登录 shell，避免守护进程的精简环境改变用户会话类型。

import (
	"context"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// lookupLoginShell 从 macOS 目录服务读取用户登录 shell。
// 参数说明：u 为 *user.User，目标是已经确认的本机进程用户。
// 返回值说明：string，成功时为绝对 shell 路径，失败时为空并交由调用方回退。
// 错误情况：目录服务失败、输出异常或查询超过两秒均返回空字符串；不重试，
// 避免 SSH 握手已经完成后，账户查询仍无限阻塞 shell 启动。
func lookupLoginShell(u *user.User) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-read", filepath.Join("/Users", u.Username), "UserShell")
	// 目录工具的后代可能继承输出管道；进程退出后也应有界等待输出关闭。
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	shell, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "UserShell: ")
	shell = strings.TrimSpace(shell)
	if !ok || !filepath.IsAbs(shell) {
		return ""
	}
	return shell
}
