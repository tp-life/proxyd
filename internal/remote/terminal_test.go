package remote

// 本文件验证 Web 终端的尺寸边界、安全门与进程内真实 PTY 会话；测试只使用一次性
// 127.0.0.1 回环连接，不访问 DERP 网络。

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
)

// TestNormalizeTerminalSize 验证默认尺寸和恶意极值都会收敛到 PTY 安全范围。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：零值未默认化或极值未夹紧时测试失败，表示 resize 消息可能把异常值传给 ioctl。
func TestNormalizeTerminalSize(t *testing.T) {
	if got := NormalizeTerminalSize(TerminalSize{}); got != (TerminalSize{Columns: 80, Rows: 24}) {
		t.Fatalf("零值尺寸应使用 80x24，got %+v", got)
	}
	if got := NormalizeTerminalSize(TerminalSize{Columns: -1, Rows: 999999}); got != (TerminalSize{Columns: 80, Rows: maxTerminalRows}) {
		t.Fatalf("异常尺寸夹紧错误，got %+v", got)
	}
	if got := NormalizeTerminalSize(TerminalSize{Columns: 1, Rows: 1}); got != (TerminalSize{Columns: 2, Rows: 1}) {
		t.Fatalf("最小尺寸夹紧错误，got %+v", got)
	}
}

// TestOpenWebTerminalGates 验证会话创建严格依次检查总开关、builtin-ssh 与运行中服务端。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：任一前置条件未阻止创建时测试失败；测试不会越过服务端运行门，因此不生成 shell。
func TestOpenWebTerminalGates(t *testing.T) {
	manager := NewManager(t.TempDir(), nil)
	if _, err := manager.OpenWebTerminal(t.Context(), TerminalSize{}); !errors.Is(err, ErrWebTerminalDisabled) {
		t.Fatalf("默认关闭应返回 ErrWebTerminalDisabled，got %v", err)
	}
	manager.cfg.WebTerminal = true
	if _, err := manager.OpenWebTerminal(t.Context(), TerminalSize{}); !errors.Is(err, ErrBuiltinSSHRequired) {
		t.Fatalf("未开启 builtin-ssh 应返回 ErrBuiltinSSHRequired，got %v", err)
	}
	manager.cfg.BuiltinSSH = true
	if _, err := manager.OpenWebTerminal(t.Context(), TerminalSize{}); !errors.Is(err, ErrRemoteServerNotRunning) {
		t.Fatalf("服务端未运行应返回 ErrRemoteServerNotRunning，got %v", err)
	}
}

// TestOpenWebTerminalPTY 验证 Web Terminal 会真正进入 tailcat builtin-ssh 的 PTY shell，
// 并把 TERM 与浏览器窗口尺寸传到子进程。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：平台不支持内嵌 SSH 时跳过；SSH 握手、PTY、输入输出、TERM 或窗口缩放任一
// 环节失败时测试失败。超时会由 context 主动关闭会话，避免遗留登录 shell。
func TestOpenWebTerminalPTY(t *testing.T) {
	if !tailcat.SupportsSSHServer() {
		t.Skip("当前平台未编译 tailcat SSH 服务")
	}

	// tailcat 首次握手会在用户配置目录生成 SSH host key。测试改用临时 HOME，既验证
	// 首次生成路径，又不读取或修改开发机上的真实 host key。
	t.Setenv("HOME", t.TempDir())
	manager := NewManager(t.TempDir(), nil)
	manager.cfg.WebTerminal = true
	manager.cfg.BuiltinSSH = true
	manager.srv = &tailcat.Server{}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := manager.OpenWebTerminal(ctx, TerminalSize{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("打开进程内 PTY 失败: %v", err)
	}
	defer session.Close()

	if err := session.Resize(TerminalSize{Columns: 120, Rows: 40}); err != nil {
		t.Fatalf("同步窗口尺寸失败: %v", err)
	}
	if _, err := session.Write([]byte("printf 'PROXYD_TERM:%s\\n' \"$TERM\"; stty size; exit\n")); err != nil {
		t.Fatalf("写入 shell 命令失败: %v", err)
	}

	outputDone := make(chan []byte, 1)
	go func() {
		output, _ := io.ReadAll(session)
		outputDone <- output
	}()
	select {
	case output := <-outputDone:
		text := string(output)
		if !strings.Contains(text, "PROXYD_TERM:xterm-256color") {
			t.Fatalf("PTY 未继承 xterm-256color，输出: %q", text)
		}
		// PTY 的 stty 输出可能带前导空格或 CRLF，按空白字段规范化后再验证行列。
		if !strings.Contains(strings.Join(strings.Fields(text), " "), "40 120") {
			t.Fatalf("PTY 窗口尺寸未同步为 40x120，输出: %q", text)
		}
	case <-ctx.Done():
		t.Fatalf("等待 PTY 输出超时: %v", ctx.Err())
	}
}
