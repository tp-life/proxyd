//go:build linux || darwin

package remote

// 本文件验证 Unix Web Terminal 在客户端断开后会终止并回收
// 登录 shell，避免长期运行的 proxyd 积累 PTY、子进程和处理协程。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
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
	client := startTerminalTestClient(t, session)

	pidPath := stateDir + "/shell.pid"
	// 使用受控 POSIX 子命令读取父进程 PID，避免 fish 将 $$ 解析为语法错误；
	// $PPID 指向实际交互 shell，仍然验证断连时该 shell 是否被回收。
	if _, err := fmt.Fprintf(session, "/bin/sh -c 'printf \"%%s\\n\" \"$PPID\"' > %q\r", pidPath); err != nil {
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
	// 先确认记录的是存活进程，避免错误 PID 让断连后的 ESRCH 检查出现假阳性。
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("断连前 shell 进程 %d 不可用: %v", pid, err)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("关闭 Web Terminal 失败: %v", err)
	}
	reapDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(reapDeadline) {
		if killErr := syscall.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
			select {
			case <-client.done:
				return
			case <-time.After(time.Second):
				t.Fatal("shell 已回收，但终端输出读取协程未退出")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Web Terminal 断开后 shell 进程 %d 仍存活", pid)
}

// TestBuiltinSSHLocalForwardingAndPeerKey 验证内嵌 SSH 支持到本机回环 IP 的
// 本地端口转发（ssh -L / VS Code Remote-SSH），同时拒绝主机名和非回环地址，
// 并将隧道节点公钥注入 TAILCAT_PEER_KEY；Web Terminal 默认不开放端口转发。
func TestBuiltinSSHLocalForwardingAndPeerKey(t *testing.T) {
	echoLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听回环回显服务失败: %v", err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	echoAddr := echoLn.Addr().String()
	echoPort := echoLn.Addr().(*net.TCPAddr).Port

	stateDir := t.TempDir()
	const wantPeerKey = "nodekey:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	handler, err := managedShellSSHHandlerWithOptions(stateDir, newSSHAccess(newAuditLog(16)), "", shellSSHOptions{
		allowLocalForwarding: true,
		peerKeyLookup:        func(net.Addr) string { return wantPeerKey },
	})
	if err != nil {
		t.Fatalf("构造内嵌 SSH 处理器失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	clientSide, err := openAuthenticatedLoopback(ctx, handler)
	if err != nil {
		t.Fatalf("建立回环测试传输失败: %v", err)
	}
	defer clientSide.Close()

	cc, chans, reqs, err := gossh.NewClientConn(clientSide, "tailcat", &gossh.ClientConfig{
		User:            "proxyd",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatalf("SSH 握手失败: %v", err)
	}
	client := gossh.NewClient(cc, chans, reqs)
	defer client.Close()

	// 1. 回环 IP 字面量转发应放行。
	fwdConn, err := client.Dial("tcp", echoAddr)
	if err != nil {
		t.Fatalf("回环 IP 本地转发应成功: %v", err)
	}
	if _, err := fwdConn.Write([]byte("vsc")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(fwdConn, buf); err != nil || string(buf) != "vsc" {
		t.Fatalf("本地转发回显错误: %q err=%v", buf, err)
	}
	_ = fwdConn.Close()

	// 2. 主机名（含 localhost）与非回环 IP 必须被拒绝。
	if denied, err := client.Dial("tcp", fmt.Sprintf("localhost:%d", echoPort)); err == nil {
		_ = denied.Close()
		t.Fatal("不应允许通过主机名 localhost 进行本地转发")
	}
	if denied, err := client.Dial("tcp", "192.0.2.1:80"); err == nil {
		_ = denied.Close()
		t.Fatal("不应允许转发到非回环 IP")
	}

	// 3. 验证 TAILCAT_PEER_KEY 已注入到会话环境。
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("创建 SSH 会话失败: %v", err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput("printf '%s' \"$TAILCAT_PEER_KEY\"")
	if err != nil {
		t.Fatalf("执行环境检查命令失败: %v (%s)", err, string(out))
	}
	if strings.TrimSpace(string(out)) != wantPeerKey {
		t.Fatalf("TAILCAT_PEER_KEY = %q, want %q", strings.TrimSpace(string(out)), wantPeerKey)
	}

	// 4. Web Terminal 使用的 localShellSSHHandler 不应开启 direct-tcpip。
	localHandler, err := localShellSSHHandler(stateDir, "")
	if err != nil {
		t.Fatalf("构造 Web Terminal 处理器失败: %v", err)
	}
	c2, err := openAuthenticatedLoopback(ctx, localHandler)
	if err != nil {
		t.Fatalf("建立 Web Terminal 测试传输失败: %v", err)
	}
	defer c2.Close()
	cc2, chans2, reqs2, err := gossh.NewClientConn(c2, "local", &gossh.ClientConfig{
		User:            "proxyd",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatalf("local SSH 握手失败: %v", err)
	}
	localClient := gossh.NewClient(cc2, chans2, reqs2)
	defer localClient.Close()
	if denied, err := localClient.Dial("tcp", echoAddr); err == nil {
		_ = denied.Close()
		t.Fatal("Web Terminal SSH 不应开启本地端口转发")
	}
}
