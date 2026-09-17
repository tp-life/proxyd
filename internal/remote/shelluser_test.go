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
	cases := []struct {
		name      string
		euid      int
		shellUser string
		wantErr   bool
	}{
		{"非 root 无配置", 1000, "", false},
		{"非 root 有配置", 1000, "tp", false},
		{"root 无配置拒绝", 0, "", true},
		{"root 空白配置拒绝", 0, "  ", true},
		{"root 有配置放行", 0, "tp", false},
		{"Windows 语义（-1）", -1, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSessionUserAllowed(tc.euid, tc.shellUser)
			if tc.wantErr && !errors.Is(err, errRootShellUserRequired) {
				t.Fatalf("期望 errRootShellUserRequired，got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望放行，got %v", err)
			}
		})
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
	current, err := user.Current()
	if err != nil {
		t.Skip("无法解析当前用户")
	}
	if got := effectiveSessionUser(""); got != current.Username {
		t.Fatalf("缺省应回退进程用户 %q，got %q", current.Username, got)
	}
}
