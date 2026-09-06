package remote

// 本文件读取 Linux 账户数据库中的登录 shell，支持 NSS 目录用户与本地 passwd 用户。

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// lookupLoginShell 查询 Linux 用户的登录 shell，优先 NSS，再回退本地 passwd 文件。
// 参数说明：u 为 *user.User，目标是已经确认的本机进程用户。
// 返回值说明：string，成功时为账户记录中的 shell，失败时为空供调用方回退。
// 错误情况：getent 不存在、NSS 查询失败或超过两秒时读取 /etc/passwd；
// 两条路径均失败则返回空字符串，不因目录服务不可用而永久卡住 SSH 登录。
func lookupLoginShell(u *user.User) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// 使用已解析的数字 UID，避免用户名被目录查询工具解释成命令选项。
	cmd := exec.CommandContext(ctx, "getent", "passwd", u.Uid)
	cmd.WaitDelay = 100 * time.Millisecond
	if out, err := cmd.Output(); err == nil {
		if shell := passwdLoginShell(string(out), u); shell != "" {
			return shell
		}
	}
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	return passwdLoginShell(string(data), u)
}

// passwdLoginShell 从 passwd 格式记录提取目标用户的登录 shell。
// 参数说明：data 为 string，账户数据库内容；u 为 *user.User，匹配用户名和 UID。
// 返回值说明：string，绝对 shell 路径；空 shell 字段按账户语义返回 /bin/sh。
// 错误情况：忽略损坏、不匹配或非绝对路径记录，未找到可用记录时返回空字符串。
func passwdLoginShell(data string, u *user.User) string {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) != 7 || fields[0] != u.Username || fields[2] != u.Uid {
			continue
		}
		if fields[6] == "" {
			return "/bin/sh"
		}
		if filepath.IsAbs(fields[6]) {
			return fields[6]
		}
	}
	return ""
}
