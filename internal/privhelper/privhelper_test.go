package privhelper

// 统一助手 plist 渲染与属主 UID 解析的跨平台单测（无平台代码，任意平台可跑）。

import (
	"strings"
	"testing"
)

// TestRenderPlist 验证统一助手 plist：Label、helper 子命令、属主环境变量与常驻策略。
// 参数：t 为 *testing.T。返回：无。错误：关键字段缺失或未做 XML 转义时失败。
func TestRenderPlist(t *testing.T) {
	plist := RenderPlist("/usr/local/bin/proxyd", 501)
	for _, want := range []string{
		"<string>com.proxyd.helper</string>",
		"<string>/usr/local/bin/proxyd</string>",
		"<string>helper</string>",
		"<key>PROXYD_HELPER_OWNER_UID</key>",
		"<string>501</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"/var/log/com.proxyd.helper.log",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist 缺 %q:\n%s", want, plist)
		}
	}
	// XML 转义。
	escaped := RenderPlist("/opt/a&b/proxyd", 501)
	if !strings.Contains(escaped, "/opt/a&amp;b/proxyd") {
		t.Error("路径未做 XML 转义")
	}
}

// TestResolveOwnerUID 验证属主 UID 解析：统一变量优先、旧版变量回退、
// 缺失报错与非法值拒绝。
// 参数：t 为 *testing.T。返回：无。错误：优先级、回退或校验错误时失败。
func TestResolveOwnerUID(t *testing.T) {
	getenv := func(env map[string]string) func(string) string {
		return func(key string) string { return env[key] }
	}
	// 统一变量优先于旧版变量。
	uid, err := ResolveOwnerUID(getenv(map[string]string{
		OwnerUIDEnv: "501", "PROXYD_TUN_OWNER_UID": "502",
	}), "PROXYD_TUN_OWNER_UID", "proxyd helper install")
	if err != nil || uid != 501 {
		t.Errorf("统一变量应优先: uid=%d err=%v", uid, err)
	}
	// 旧 plist 过渡期：统一变量缺失时回退旧版变量。
	uid, err = ResolveOwnerUID(getenv(map[string]string{
		"PROXYD_GATEWAY_OWNER_UID": "503",
	}), "PROXYD_GATEWAY_OWNER_UID", "proxyd helper install")
	if err != nil || uid != 503 {
		t.Errorf("旧版变量回退失败: uid=%d err=%v", uid, err)
	}
	// 两者皆缺：报错且含安装指引。
	if _, err = ResolveOwnerUID(getenv(nil), "PROXYD_TUN_OWNER_UID", "proxyd helper install"); err == nil ||
		!strings.Contains(err.Error(), OwnerUIDEnv) || !strings.Contains(err.Error(), "proxyd helper install") {
		t.Errorf("属主缺失应报错并含指引: %v", err)
	}
	// 非法值拒绝。
	if _, err = ResolveOwnerUID(getenv(map[string]string{OwnerUIDEnv: "abc"}), "PROXYD_TUN_OWNER_UID", "x"); err == nil {
		t.Error("非法 UID 应拒绝")
	}
	if _, err = ResolveOwnerUID(getenv(map[string]string{OwnerUIDEnv: "-1"}), "PROXYD_TUN_OWNER_UID", "x"); err == nil {
		t.Error("负数 UID 应拒绝")
	}
}
