//go:build darwin

package autostart

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderPlistPassesPlutil 使用 macOS 原生解析器验证最终 LaunchDaemon 文件。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；plutil 接受生成的 plist 时测试通过。
//
// 错误情况：临时文件写入失败、plutil 不可执行或系统拒绝 plist 结构时测试失败。
func TestRenderPlistPassesPlutil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "com.proxyd.plist")
	content := RenderPlist("/usr/local/bin/proxyd", "/Users/x/.config/proxyd/config.yaml", "/Users/x/.local/state/proxyd/proxyd.log", "x", "/Users/x")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入 plist 测试文件失败: %v", err)
	}
	if output, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil 拒绝 LaunchDaemon plist: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

// TestRenderShellCommandQuotesArguments 验证管理员授权脚本不会把路径内容解释为 shell 操作符。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；命令与参数均正确引用时测试通过。
//
// 错误情况：空格、单引号或分号未被安全引用时测试失败。
func TestRenderShellCommandQuotesArguments(t *testing.T) {
	command := privilegedCommand{
		Name: "/usr/bin/install",
		Args: []string{"/tmp/proxy d'aemon;touch bad", daemonPlistPath},
	}
	got := renderShellCommand(command)
	for _, want := range []string{
		"'/usr/bin/install'",
		"'/tmp/proxy d'\\''aemon;touch bad'",
		"'/Library/LaunchDaemons/com.proxyd.plist'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("管理员命令缺少安全引用 %q: %s", want, got)
		}
	}
}

// TestEscapeAppleScript 验证 shell 文本嵌入 AppleScript 时正确转义。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；反斜杠与双引号均只作为字符串内容时测试通过。
//
// 错误情况：任一字符可能提前结束 AppleScript 字面量时测试失败。
func TestEscapeAppleScript(t *testing.T) {
	got := escapeAppleScript(`a\b"c`)
	if got != `a\\b\"c` {
		t.Fatalf("AppleScript 转义结果 = %q", got)
	}
}
