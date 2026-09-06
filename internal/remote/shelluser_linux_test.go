package remote

// 本文件验证 Linux 登录 shell 来自账户记录，避免 systemd 缺少 SHELL 时误用 /bin/sh。

import (
	"os/user"
	"testing"
)

// TestPasswdLoginShell 验证账户匹配、空 shell 默认值以及异常记录边界。
// 参数说明：t 为 *testing.T，执行独立的 passwd 文本样例。
// 返回值说明：无。
// 错误情况：选择其他账户、接受相对路径或未保留合法账户 shell 时失败。
func TestPasswdLoginShell(t *testing.T) {
	u := &user.User{Username: "alice", Uid: "1000"}
	for _, tc := range []struct {
		name, data, want string
	}{
		{"account", "root:x:0:0::/root:/bin/sh\nalice:x:1000:1000::/home/alice:/bin/bash\n", "/bin/bash"},
		{"empty", "alice:x:1000:1000::/home/alice:", "/bin/sh"},
		{"uid mismatch", "alice:x:1001:1000::/home/alice:/bin/zsh", ""},
		{"name mismatch", "bob:x:1000:1000::/home/bob:/bin/zsh", ""},
		{"malformed", "alice:x:1000", ""},
		{"relative", "alice:x:1000:1000::/home/alice:bash", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := passwdLoginShell(tc.data, u); got != tc.want {
				t.Fatalf("登录 shell=%q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestLinuxLoginShellUsesAccount 验证守护进程 SHELL 与账户配置冲突时仍选择账户 shell。
// 参数说明：t 为 *testing.T，临时覆盖父进程 SHELL，不修改系统账户。
// 返回值说明：无。
// 错误情况：账户查询失败或错误继承父进程 shell 时失败。
func TestLinuxLoginShellUsesAccount(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	want := lookupLoginShell(u)
	if want == "" {
		t.Fatal("当前用户的账户 shell 不可用")
	}
	t.Setenv("SHELL", "/incorrect-daemon-shell")
	if got := loginShell(u); got != want {
		t.Fatalf("登录 shell=%q，期望账户配置 %q", got, want)
	}
}
