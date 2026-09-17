//go:build linux || darwin

package remote

import (
	"os"
	"os/user"
	"syscall"
	"testing"
)

// TestResolveSessionUserEmpty 验证非 root 下未配置 shell-user 时保持进程用户且无凭据切换。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；解析结果为当前用户且 cred 为 nil 时测试通过。
//
// 错误情况：root 测试环境跳过（空配置会被 fail-closed 拒绝）。
func TestResolveSessionUserEmpty(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下空 shell-user 按 fail-closed 拒绝")
	}
	su, err := resolveSessionUser("")
	if err != nil {
		t.Fatal(err)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if su.user.Username != current.Username {
		t.Fatalf("会话用户 = %q, want %q", su.user.Username, current.Username)
	}
	if su.cred != nil {
		t.Fatal("进程用户不应产生降权凭据")
	}
}

// TestResolveSessionUserSelf 验证显式配置为当前用户时解析成功且不产生凭据切换。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；账户解析一致且 cred 为 nil（uid 相同无需 setuid）时测试通过。
//
// 错误情况：解析失败或错误生成凭据时测试失败。
func TestResolveSessionUserSelf(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skip("无法解析当前用户")
	}
	su, err := resolveSessionUser(current.Username)
	if err != nil {
		t.Fatal(err)
	}
	if su.user.Uid != current.Uid {
		t.Fatalf("UID = %q, want %q", su.user.Uid, current.Uid)
	}
	if su.cred != nil {
		t.Fatal("目标与进程同 UID 时不应生成凭据")
	}
}

// TestResolveSessionUserMissing 验证不存在的账户在会话边界被拒绝。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；解析返回错误时测试通过。
//
// 错误情况：不存在账户被静默接受时测试失败。
func TestResolveSessionUserMissing(t *testing.T) {
	if _, err := resolveSessionUser("proxyd-no-such-user-7f3a9b"); err == nil {
		t.Fatal("不存在的账户必须被拒绝")
	}
}

// TestNewShellSessionCommandCredential 验证降权凭据写入 SysProcAttr 且进程组合并不覆盖它。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；Credential 与 Setpgid 同时存在时测试通过。
//
// 错误情况：凭据丢失或被 prepareShellProcess 覆盖时测试失败。
func TestNewShellSessionCommandCredential(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skip("无法解析当前用户")
	}
	su := &sessionUser{user: current, cred: sessionCredentialForTest(t, current)}
	cmd := newShellSessionCommand(su, "")
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("降权凭据未写入 SysProcAttr")
	}
	prepareShellProcess(cmd)
	if cmd.SysProcAttr.Credential == nil {
		t.Fatal("prepareShellProcess 覆盖了降权凭据")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("Setpgid 未合并写入")
	}
}

// sessionCredentialForTest 构造一个指向其他 UID 的凭据，模拟 root 降权场景的命令属性。
// 参数说明：t 为 *testing.T；u 为 *user.User，提供 GID 与用户名。
// 返回值说明：*syscall.Credential，UID 取 nobody（65534，无需真实存在，仅验证属性装配）。
// 错误情况：GID 解析失败时测试失败。
func sessionCredentialForTest(t *testing.T, u *user.User) *syscall.Credential {
	t.Helper()
	cred, err := sessionCredential(&user.User{Uid: "65534", Gid: u.Gid, Username: u.Username, HomeDir: u.HomeDir})
	if os.Geteuid() == 65534 {
		t.Skip("不可能的测试环境")
	}
	if err != nil {
		t.Fatal(err)
	}
	return cred
}
