package config

import "testing"

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
