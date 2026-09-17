package config

import "testing"

// TestRemoteShellUserValidation 验证 shell-user 的结构校验与配置段集成。
// 参数：t 为 *testing.T；返回无；非法值被拒或合法值解析丢失时失败。
func TestRemoteShellUserValidation(t *testing.T) {
	cfg, err := Parse([]byte("proxy-disabled: true\nremote:\n  disabled: true\n  shell-user: tp\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remote.ShellUser != "tp" {
		t.Fatalf("shell-user 解析丢失，got %q", cfg.Remote.ShellUser)
	}
	for _, bad := range []string{"root", "bad name", "a/b", "-x", "a:b"} {
		if err := ValidateRemoteShellUser(bad); err == nil {
			t.Errorf("shell-user %q 应被拒绝", bad)
		}
	}
	if err := ValidateRemoteShellUser("tp"); err != nil {
		t.Errorf("合法账户名被拒: %v", err)
	}
	if _, err := Parse([]byte("proxy-disabled: true\nremote:\n  disabled: true\n  shell-user: root\n")); err == nil {
		t.Fatal("配置段应拒绝 shell-user: root")
	}
}

// TestRemoteOnlyConfiguration 验证禁用代理后允许无订阅启动，已有配置的默认行为不变。
// 参数：t 为 *testing.T；返回无；不能加载纯远程配置或默认误停用时失败。
func TestRemoteOnlyConfiguration(t *testing.T) {
	cfg, err := Parse([]byte("proxy-disabled: true\nremote:\n  disabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ProxyDisabled || !cfg.Remote.Disabled {
		t.Fatal("模块配置丢失")
	}
	old, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if old.ProxyDisabled || old.Remote.Disabled {
		t.Fatal("旧配置默认能力被关闭")
	}
}
