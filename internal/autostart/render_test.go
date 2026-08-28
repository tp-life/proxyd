package autostart

import (
	"strings"
	"testing"
)

func TestRenderPlist(t *testing.T) {
	p := RenderPlist("/usr/local/bin/proxyd", "/Users/x/.config/proxyd/config.yaml", "/Users/x/.local/state/proxyd/proxyd.log")
	for _, want := range []string{
		"<string>com.proxyd</string>",
		"<string>/usr/local/bin/proxyd</string>",
		"<string>serve</string>",
		"<string>-c</string>",
		"<string>/Users/x/.config/proxyd/config.yaml</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<true/>",
		"<string>/Users/x/.local/state/proxyd/proxyd.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist 缺少 %q:\n%s", want, p)
		}
	}
}

func TestRenderPlistEscapesXML(t *testing.T) {
	p := RenderPlist("/opt/a&b/proxyd", "/cfg<c>.yaml", "/log>d.log")
	if strings.Contains(p, "a&b") || strings.Contains(p, "<c>") || strings.Contains(p, "d>d") {
		t.Errorf("XML 特殊字符未转义:\n%s", p)
	}
	for _, want := range []string{"a&amp;b", "&lt;c&gt;", "log&gt;d"} {
		if !strings.Contains(p, want) {
			t.Errorf("plist 缺少转义结果 %q", want)
		}
	}
}

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

func TestRenderRunCommand(t *testing.T) {
	c := RenderRunCommand(`C:\Tools\proxyd.exe`, `C:\Users\x\proxyd.yaml`)
	want := `"C:\Tools\proxyd.exe" start -c "C:\Users\x\proxyd.yaml"`
	if c != want {
		t.Errorf("Run 命令 = %q, want %q", c, want)
	}
}
