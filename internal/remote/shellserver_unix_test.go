//go:build linux || darwin

package remote

// 本文件验证 Unix Web Terminal 在客户端断开后会终止并回收
// 登录 shell，避免长期运行的 proxyd 积累 PTY、子进程和处理协程。

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestShellHostKeyPersistsSecurely 验证 Web Terminal 与 builtin-ssh 共用的 host key
// 会稳定保存在 state-dir，且私钥文件不会向同组或其他用户开放。
//
// 参数说明：
//   - t: *testing.T，提供隔离状态目录和文件权限断言。
//
// 返回值说明：无。
//
// 错误情况：首次生成、再次加载、密钥身份稳定性或 0600 权限任一不满足时测试失败。
func TestShellHostKeyPersistsSecurely(t *testing.T) {
	stateDir := t.TempDir()
	first, err := loadOrCreateShellHostKey(stateDir)
	if err != nil {
		t.Fatalf("首次生成 SSH host key 失败: %v", err)
	}
	keyPath := filepath.Join(stateDir, shellHostKeyRelPath)
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("准备权限修复场景失败: %v", err)
	}
	second, err := loadOrCreateShellHostKey(stateDir)
	if err != nil {
		t.Fatalf("再次加载 SSH host key 失败: %v", err)
	}
	if !bytes.Equal(first.PublicKey().Marshal(), second.PublicKey().Marshal()) {
		t.Fatal("同一 state-dir 再次加载得到了不同 SSH host key")
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("读取 SSH host key 文件状态失败: %v", err)
	}
	if permission := info.Mode().Perm(); permission != 0o600 {
		t.Fatalf("SSH host key 权限=%o，期望 600", permission)
	}
}

// TestWebTerminalDisconnectReapsShell 验证交互 shell 未主动 exit 时，
// 关闭客户端会话仍会终止对应的 Unix 进程组并完成 Wait 回收。
//
// 参数说明：
//   - t: *testing.T，负责隔离状态目录、超时控制与断言报告。
//
// 返回值说明：无。
//
// 错误情况：SSH/PTY 建立失败、shell PID 未写入、断连后进程仍存活，
// 或读取协程无法随会话结束时测试失败。
func TestWebTerminalDisconnectReapsShell(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(stateDir, nil)
	manager.cfg.WebTerminal = true

	session, err := manager.OpenWebTerminal(t.Context(), TerminalSize{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("打开 Web Terminal 失败: %v", err)
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		_, _ = io.Copy(io.Discard, session)
	}()

	pidPath := stateDir + "/shell.pid"
	if _, err := fmt.Fprintf(session, "printf '%%d\\n' $$ > %q\n", pidPath); err != nil {
		_ = session.Close()
		t.Fatalf("请求 shell 写入 PID 失败: %v", err)
	}

	var pid int
	pidDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(pidDeadline) {
		data, readErr := os.ReadFile(pidPath)
		if readErr == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid <= 0 {
		_ = session.Close()
		t.Fatalf("未在限期内取得 shell PID：%v", err)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("关闭 Web Terminal 失败: %v", err)
	}
	reapDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(reapDeadline) {
		if killErr := syscall.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
			select {
			case <-readDone:
				return
			case <-time.After(time.Second):
				t.Fatal("shell 已回收，但终端输出读取协程未退出")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Web Terminal 断开后 shell 进程 %d 仍存活", pid)
}
