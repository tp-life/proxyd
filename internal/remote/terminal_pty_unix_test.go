//go:build linux || darwin

package remote

// 本文件使用真实终端协议驱动 Unix PTY 测试，避免 fish 能力查询被误判为 shell 启动失败。
// 命令只在提示符就绪后发送；浏览器生产路径继续由 xterm 回应查询，不注入模拟终端数据。

import (
	"bytes"
	"context"
	"io"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// terminalTestClient 消费终端输出并回应最小查询协议；缓冲区由锁保护，done 表示读取已结束。
type terminalTestClient struct {
	mu     sync.Mutex
	output bytes.Buffer
	done   chan struct{}
}

// startTerminalTestClient 启动输出消费并等待可执行命令的提示符。
// 参数说明：t 为 *testing.T，负责限时与清理；session 为 *TerminalSession，真实 SSH/PTY 会话。
// 返回值说明：*terminalTestClient，可读取输出快照或等待 done。
// 错误情况：用户查询失败、shell 提前退出或五秒内没有提示符时终止测试并关闭会话。
// fish 的首段输出只是能力查询，不能当作提示符就绪；先回应 DA/光标位置，再等 OSC 133 B。
// 其他 shell 沿用首段提示符输出作为同步点；不通过延长超时或修改用户 shell 来规避失败。
func startTerminalTestClient(t *testing.T, session *TerminalSession) *terminalTestClient {
	t.Helper()
	t.Cleanup(func() { _ = session.Close() })
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	fish := filepath.Base(loginShell(u)) == "fish"
	client := &terminalTestClient{done: make(chan struct{})}
	ready := make(chan struct{})
	go func() {
		defer close(client.done)
		pending := ""
		signaled := false
		buffer := make([]byte, 4096)
		for {
			n, err := session.Read(buffer)
			if n > 0 {
				client.mu.Lock()
				_, _ = client.output.Write(buffer[:n])
				client.mu.Unlock()
				pending += string(buffer[:n])
				// 查询可能被 SSH 分片，保留未完成的短尾部；已回应的序列立即移除以免重复发送。
				for _, pair := range [][2]string{{"\x1b[0c", "\x1b[?1;2c"}, {"\x1b[c", "\x1b[?1;2c"}, {"\x1b[6n", "\x1b[1;1R"}} {
					for strings.Contains(pending, pair[0]) {
						if _, writeErr := io.WriteString(session, pair[1]); writeErr != nil {
							return
						}
						pending = strings.Replace(pending, pair[0], "", 1)
					}
				}
				if !signaled && (!fish || strings.Contains(pending, "\x1b]133;B")) {
					signaled = true
					close(ready)
				}
				if len(pending) > 128 {
					pending = pending[len(pending)-128:]
				}
			}
			if err != nil {
				return
			}
		}
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ready:
		return client
	case <-client.done:
		t.Fatalf("shell 在提示符就绪前退出，输出=%q", client.String())
	case <-timer.C:
		t.Fatalf("等待终端握手与提示符超时，输出=%q", client.String())
	}
	return nil
}

// String 返回并发安全的输出快照；无参数，返回 string；不关闭会话，无错误。
func (c *terminalTestClient) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.output.String()
}

// TestOpenWebTerminalPTY 验证 Web Terminal 会真正进入进程内 SSH 服务的 PTY shell，
// 并把 TERM 与浏览器窗口尺寸传到子进程。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：平台不支持进程内 shell 服务时跳过；SSH 握手、PTY、输入输出、TERM 或窗口
// 缩放任一环节失败时测试失败。超时会由 context 主动关闭会话，避免遗留登录 shell。
func TestOpenWebTerminalPTY(t *testing.T) {
	if _, err := localShellSSHHandler(t.TempDir()); err != nil {
		t.Skip("当前平台不支持进程内 shell 服务")
	}
	manager := NewManager(t.TempDir(), nil)
	manager.cfg.WebTerminal = true

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := manager.OpenWebTerminal(ctx, TerminalSize{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("打开进程内 PTY 失败: %v", err)
	}
	defer session.Close()
	client := startTerminalTestClient(t, session)

	if err := session.Resize(TerminalSize{Columns: 120, Rows: 40}); err != nil {
		t.Fatalf("同步窗口尺寸失败: %v", err)
	}
	if _, err := session.Write([]byte("printf 'PROXYD_TERM:%s\\n' \"$TERM\"; stty size; exit\n")); err != nil {
		t.Fatalf("写入 shell 命令失败: %v", err)
	}

	select {
	case <-client.done:
		text := client.String()
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
