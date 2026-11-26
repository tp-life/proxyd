//go:build linux || darwin

package remote

import (
	"errors"
	"os"
	"os/user"
	"strconv"
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

// TestResolveSessionUserWithEnvSudoAndRequestedUser 验证 sudo 自动降权、SSH 指定普通用户与纯 root fail-closed 规则。
func TestResolveSessionUserWithEnvSudoAndRequestedUser(t *testing.T) {
	alice := &user.User{Username: "alice", Uid: "1000", Gid: "1000", HomeDir: "/home/alice"}
	bob := &user.User{Username: "bob", Uid: "1001", Gid: "1001", HomeDir: "/home/bob"}
	toor := &user.User{Username: "toor", Uid: "0", Gid: "0", HomeDir: "/root"}
	users := map[string]*user.User{
		"alice": alice,
		"bob":   bob,
		"toor":  toor,
		"root":  {Username: "root", Uid: "0", Gid: "0", HomeDir: "/root"},
	}
	lookup := func(name string) (*user.User, error) {
		if u, ok := users[name]; ok {
			return u, nil
		}
		return nil, user.UnknownUserError(name)
	}
	buildCred := func(u *user.User) (*syscall.Credential, error) {
		uid, _ := strconv.ParseUint(u.Uid, 10, 32)
		gid, _ := strconv.ParseUint(u.Gid, 10, 32)
		return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
	}
	envSudo := func(sudoVal string) func(string) string {
		return func(k string) string {
			if k == "SUDO_USER" {
				return sudoVal
			}
			return ""
		}
	}

	// 1. sudo 运行未配置 shell-user，未指定用户或跨机不存在用户名 → 自动降权到 SUDO_USER。
	for _, req := range []string{"", "mac-local-user", "root", "toor", "proxyd-web-terminal"} {
		su, err := resolveSessionUserWithEnv(0, "", req, envSudo("alice"), func() (*user.User, error) { return users["root"], nil }, lookup, buildCred)
		if err != nil {
			t.Fatalf("req=%q 应回退 SUDO_USER，got err=%v", req, err)
		}
		if su.user.Username != "alice" || su.cred == nil || su.cred.Uid != 1000 {
			t.Fatalf("req=%q 期望降权到 alice(1000)，got user=%+v cred=%+v", req, su.user, su.cred)
		}
	}

	// 2. sudo 运行未配置 shell-user，SSH 指定存在的普通用户 bob → 降权到 bob。
	su, err := resolveSessionUserWithEnv(0, "", "bob", envSudo("alice"), func() (*user.User, error) { return users["root"], nil }, lookup, buildCred)
	if err != nil {
		t.Fatalf("指定普通用户 bob 失败: %v", err)
	}
	if su.user.Username != "bob" || su.cred == nil || su.cred.Uid != 1001 {
		t.Fatalf("期望降权到 bob(1001)，got user=%+v cred=%+v", su.user, su.cred)
	}

	// 3. 显式配置 remote.shell-user 严格锁定账户，忽略客户端请求用户名。
	su, err = resolveSessionUserWithEnv(0, "alice", "bob", envSudo("alice"), func() (*user.User, error) { return users["root"], nil }, lookup, buildCred)
	if err != nil {
		t.Fatalf("显式 shell-user 失败: %v", err)
	}
	if su.user.Username != "alice" || su.cred == nil || su.cred.Uid != 1000 {
		t.Fatalf("显式 shell-user 应锁定 alice，got user=%+v", su.user)
	}

	// 4. 显式 shell-user 指向 UID 0 必须拒绝。
	if _, err := resolveSessionUserWithEnv(0, "toor", "", envSudo("alice"), func() (*user.User, error) { return users["root"], nil }, lookup, buildCred); err == nil {
		t.Fatal("显式 shell-user 为 UID 0 时必须拒绝")
	}

	// 5. 纯 root（无 SUDO_USER 且无 shell-user）即使客户端传入 bob 也必须 fail-closed。
	if _, err := resolveSessionUserWithEnv(0, "", "bob", envSudo(""), func() (*user.User, error) { return users["root"], nil }, lookup, buildCred); !errors.Is(err, errRootShellUserRequired) {
		t.Fatalf("纯 root 无配置应返回 errRootShellUserRequired，got %v", err)
	}

	// 6. 非 root 进程保持自身用户且不生成降权凭据。
	su, err = resolveSessionUserWithEnv(1000, "", "bob", envSudo(""), func() (*user.User, error) { return alice, nil }, lookup, buildCred)
	if err != nil {
		t.Fatalf("非 root 进程解析失败: %v", err)
	}
	if su.user.Username != "alice" || su.cred != nil {
		t.Fatalf("非 root 进程应保持自身且无凭据，got user=%+v cred=%+v", su.user, su.cred)
	}
}
