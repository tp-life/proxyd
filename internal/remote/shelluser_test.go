package remote

import (
	"errors"
	"os/user"
	"runtime"
	"strings"
	"testing"

	"proxyd/internal/config"
)

// TestCheckSessionUserAllowed 验证 root fail-closed 规则：root 且无 shell-user 一律拒绝。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；各 euid/shell-user 组合判定与预期一致时测试通过。
//
// 错误情况：任一分支放行 root 空配置或误拒非 root 时测试失败。
func TestCheckSessionUserAllowed(t *testing.T) {
	t.Setenv("SUDO_USER", "")
	cases := []struct {
		name      string
		euid      int
		shellUser string
		sudoUser  string
		wantErr   bool
	}{
		{"非 root 无配置", 1000, "", "", false},
		{"非 root 有配置", 1000, "tp", "", false},
		{"纯 root 无配置拒绝", 0, "", "", true},
		{"纯 root 空白配置拒绝", 0, "  ", "", true},
		{"root 有配置放行", 0, "tp", "", false},
		{"sudo root 自动识别普通用户放行", 0, "", "tp", false},
		{"Windows 语义（-1）", -1, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSessionUserAllowedWithSudo(tc.euid, tc.shellUser, tc.sudoUser)
			if tc.wantErr && !errors.Is(err, errRootShellUserRequired) {
				t.Fatalf("期望 errRootShellUserRequired，got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望放行，got %v", err)
			}
		})
	}
	if err := checkSessionUserAllowed(0, ""); !errors.Is(err, errRootShellUserRequired) {
		t.Fatalf("清空 SUDO_USER 时 root 空配置应返回 errRootShellUserRequired，got %v", err)
	}
}

// TestSudoInvokerUserFrom 验证 sudo 原始用户识别规则：只接受 root 进程下的合法非 root 账户。
func TestSudoInvokerUserFrom(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不使用 SUDO_USER")
	}
	users := map[string]*user.User{
		"alice": {Username: "alice", Uid: "1000", Gid: "1000"},
		"toor":  {Username: "toor", Uid: "0", Gid: "0"},
	}
	lookup := func(name string) (*user.User, error) {
		if u, ok := users[name]; ok {
			return u, nil
		}
		return nil, user.UnknownUserError(name)
	}
	envOf := func(v string) func(string) string {
		return func(k string) string {
			if k == "SUDO_USER" {
				return v
			}
			return ""
		}
	}
	if got := sudoInvokerUserFrom(0, envOf("alice"), lookup); got != "alice" {
		t.Fatalf("合法 SUDO_USER = %q, want alice", got)
	}
	if got := sudoInvokerUserFrom(1000, envOf("alice"), lookup); got != "" {
		t.Fatalf("非 root 进程不应读取 SUDO_USER，got %q", got)
	}
	if got := sudoInvokerUserFrom(0, envOf("root"), lookup); got != "" {
		t.Fatalf("root 不应被识别为普通 sudo 用户，got %q", got)
	}
	if got := sudoInvokerUserFrom(0, envOf("toor"), lookup); got != "" {
		t.Fatalf("UID 0 账户不应被识别为普通 sudo 用户，got %q", got)
	}
	if got := sudoInvokerUserFrom(0, envOf("missing"), lookup); got != "" {
		t.Fatalf("不存在账户不应被识别，got %q", got)
	}
}

// TestSessionShellActive 验证 shell 入口判定只覆盖真正会创建本机 shell 的配置。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；各配置组合判定与预期一致时测试通过。
//
// 错误情况：模块停用仍判活跃、或开启入口未判活跃时测试失败。
func TestSessionShellActive(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.RemoteConfig
		want bool
	}{
		{"全关", config.RemoteConfig{}, false},
		{"仅服务端无内嵌 SSH", config.RemoteConfig{Enabled: true}, false},
		{"服务端加内嵌 SSH", config.RemoteConfig{Enabled: true, BuiltinSSH: true}, true},
		{"内嵌 SSH 但服务端未开", config.RemoteConfig{BuiltinSSH: true}, false},
		{"仅 Web 终端", config.RemoteConfig{WebTerminal: true}, true},
		{"模块停用优先", config.RemoteConfig{Disabled: true, Enabled: true, BuiltinSSH: true, WebTerminal: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionShellActive(tc.cfg); got != tc.want {
				t.Fatalf("sessionShellActive = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestValidateShellUser 验证 shell-user 设置入口的账户校验。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；root、非法字符、不存在账户被拒绝且当前账户被接受时测试通过。
//
// 错误情况：任一分支与预期不符时测试失败；Windows 跳过存在性用例（平台不支持）。
func TestValidateShellUser(t *testing.T) {
	if err := ValidateShellUser("root"); err == nil {
		t.Fatal("root 必须被拒绝")
	}
	if err := ValidateShellUser("bad name"); err == nil {
		t.Fatal("含空白的账户名必须被拒绝")
	}
	if runtime.GOOS == "windows" {
		if err := ValidateShellUser("someone"); err == nil || !strings.Contains(err.Error(), "Windows") {
			t.Fatalf("Windows 应返回不支持错误，got %v", err)
		}
		return
	}
	if err := ValidateShellUser("proxyd-no-such-user-7f3a9b"); err == nil {
		t.Fatal("不存在的账户必须被拒绝")
	}
	current, err := user.Current()
	if err != nil {
		t.Skip("无法解析当前用户")
	}
	if current.Uid == "0" {
		t.Skip("root 运行的测试环境没有可接受的普通账户")
	}
	if err := ValidateShellUser(current.Username); err != nil {
		t.Fatalf("当前普通账户应被接受，got %v", err)
	}
}

// TestEffectiveSessionUser 验证展示用会话用户：配置优先，缺省回退进程用户。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；两种来源的展示值正确时测试通过。
//
// 错误情况：配置未优先或回退值非进程用户名时测试失败。
func TestEffectiveSessionUser(t *testing.T) {
	if got := effectiveSessionUser("  tp "); got != "tp" {
		t.Fatalf("配置值应去空白优先返回，got %q", got)
	}
	if runtime.GOOS != "windows" {
		alice := &user.User{Username: "alice", Uid: "1000", Gid: "1000"}
		root := &user.User{Username: "root", Uid: "0", Gid: "0"}
		got := effectiveSessionUserFrom(
			0,
			"",
			func(k string) string {
				if k == "SUDO_USER" {
					return "alice"
				}
				return ""
			},
			func() (*user.User, error) { return root, nil },
			func(name string) (*user.User, error) {
				if name == "alice" {
					return alice, nil
				}
				return nil, user.UnknownUserError(name)
			},
		)
		if got != "alice" {
			t.Fatalf("sudo root 未配置 shell-user 时应展示 SUDO_USER，got %q", got)
		}
	}
	current, err := user.Current()
	if err != nil {
		t.Skip("无法解析当前用户")
	}
	if got := effectiveSessionUser(""); got != current.Username {
		t.Fatalf("缺省应回退进程用户 %q，got %q", current.Username, got)
	}
}
