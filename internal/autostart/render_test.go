package autostart

import (
	"encoding/xml"
	"strings"
	"testing"
)

// TestRenderPlist 验证系统 LaunchDaemon 的必要字段与 XML 结构。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；所有启动、降权和日志字段存在且 XML 合法时测试通过。
//
// 错误情况：字段缺失或 XML 无法解析时测试失败。
func TestRenderPlist(t *testing.T) {
	p := RenderPlist("/usr/local/bin/proxyd", "/Users/x/.config/proxyd/config.yaml", "/Users/x/.local/state/proxyd/proxyd.log", "x", "/Users/x")
	for _, want := range []string{
		"<string>com.proxyd</string>",
		"<string>/usr/local/bin/proxyd</string>",
		"<string>serve</string>",
		"<string>-c</string>",
		"<string>/Users/x/.config/proxyd/config.yaml</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<true/>",
		"<key>UserName</key>\n\t<string>x</string>",
		"<key>HOME</key>\n\t\t<string>/Users/x</string>",
		"<string>/Users/x/.local/state/proxyd/proxyd.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist 缺少 %q:\n%s", want, p)
		}
	}
	var document struct {
		XMLName xml.Name `xml:"plist"`
	}
	if err := xml.Unmarshal([]byte(p), &document); err != nil {
		t.Fatalf("LaunchDaemon plist 不是合法 XML: %v", err)
	}
}

// TestRenderPlistEscapesXML 验证所有外部文本都经过 XML 转义。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；路径和用户名中的保留字符不破坏 plist 时测试通过。
//
// 错误情况：原始保留字符泄露或转义结果缺失时测试失败。
func TestRenderPlistEscapesXML(t *testing.T) {
	p := RenderPlist("/opt/a&b/proxyd", "/cfg<c>.yaml", "/log>d.log", "a&b", "/Users/<x>")
	if strings.Contains(p, "a&b") || strings.Contains(p, "<c>") || strings.Contains(p, "d>d") {
		t.Errorf("XML 特殊字符未转义:\n%s", p)
	}
	for _, want := range []string{"a&amp;b", "&lt;c&gt;", "log&gt;d", "/Users/&lt;x&gt;"} {
		if !strings.Contains(p, want) {
			t.Errorf("plist 缺少转义结果 %q", want)
		}
	}
}

// TestRenderUnit 验证 Linux systemd user unit 的关键服务语义。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；执行命令、重启策略与安装目标完整时测试通过。
//
// 错误情况：任一关键字段缺失时测试失败。
func TestRenderUnit(t *testing.T) {
	u := RenderUnit("/usr/local/bin/proxyd", "/home/x/.config/proxyd/config.yaml")
	for _, want := range []string{
		"[Service]",
		"ExecStart=/usr/local/bin/proxyd serve -c /home/x/.config/proxyd/config.yaml",
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit 缺少 %q:\n%s", want, u)
		}
	}
}

// TestRenderRunCommand 验证 Windows 登录启动命令保留路径引用。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；渲染结果与预期命令完全一致时测试通过。
//
// 错误情况：可执行文件或配置路径引用错误时测试失败。
func TestRenderRunCommand(t *testing.T) {
	c := RenderRunCommand(`C:\Tools\proxyd.exe`, `C:\Users\x\proxyd.yaml`)
	want := `"C:\Tools\proxyd.exe" start -c "C:\Users\x\proxyd.yaml"`
	if c != want {
		t.Errorf("Run 命令 = %q, want %q", c, want)
	}
}
