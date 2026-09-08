package remote

// 本文件验证 Web 终端的尺寸边界、安全门与进程内真实 PTY 会话；测试只使用一次性
// 127.0.0.1 回环连接，不访问 DERP 网络。

import (
	"context"
	"errors"
	"testing"
	"time"
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

// TestOpenWebTerminalGates 验证会话创建只由 Web Terminal 总开关控制，
// 不再要求远程服务端运行或开启 builtin-ssh。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：关闭状态未阻止创建、或开启后仍因服务端/内嵌 SSH 门槛失败时测试失败；
// 平台不支持进程内 shell 服务时跳过。
func TestOpenWebTerminalGates(t *testing.T) {
	manager := NewManager(t.TempDir(), nil)
	if _, err := manager.OpenWebTerminal(t.Context(), TerminalSize{}); !errors.Is(err, ErrWebTerminalDisabled) {
		t.Fatalf("默认关闭应返回 ErrWebTerminalDisabled，got %v", err)
	}
	if _, err := localShellSSHHandler(t.TempDir()); err != nil {
		t.Skip("当前平台不支持进程内 shell 服务")
	}
	manager.cfg.WebTerminal = true
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := manager.OpenWebTerminal(ctx, TerminalSize{})
	if err != nil {
		t.Fatalf("开启后不应再要求服务端运行或 builtin-ssh: %v", err)
	}
	_ = session.Close()
}
