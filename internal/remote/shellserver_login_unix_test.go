//go:build linux || darwin

package remote

// 本文件通过真实 SSH 握手和 PTY 验证登录环境，所有启动文件均放在临时 HOME，
// 不读取或修改开发者的个人 shell 配置，也不依赖外部隧道网络。

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ssh "github.com/tailscale/gliderssh"
	gossh "golang.org/x/crypto/ssh"
)

// newLoginTestSession 创建连接隔离用户目录的真实 SSH 客户端会话。
// 参数说明：t 为 *testing.T，负责临时目录、连接清理与失败报告。
// 返回值说明：*gossh.Session，以及复制的 *user.User（仅替换 HOME）。
// 错误情况：用户解析、密钥初始化或 SSH 握手失败时终止测试；连接限时五秒，避免卡住测试进程。
func newLoginTestSession(t *testing.T) (*gossh.Session, *user.User) {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	u := *current
	u.HomeDir = t.TempDir()
	signer, err := loadOrCreateShellHostKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 仅替换账户目录，仍使用生产会话执行器，覆盖环境过滤、shell 选择和真实 PTY。
	server := &ssh.Server{
		Handler:           func(sess ssh.Session) { runShellSession(sess, &sessionUser{user: &u}) },
		SubsystemHandlers: map[string]ssh.SubsystemHandler{"proxyd-diagnostics": func(sess ssh.Session) { runShellDiagnostic(sess, &sessionUser{user: &u}) }},
		ChannelHandlers:   map[string]ssh.ChannelHandler{"session": ssh.DefaultSessionHandler},
	}
	server.AddHostKey(signer)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	conn, err := openAuthenticatedLoopback(ctx, server.HandleConn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	clientConn, channels, requests, err := gossh.NewClientConn(conn, "login-test", &gossh.ClientConfig{
		User: u.Username,
		// 此端点是测试创建的一次性认证回环，接受测试密钥不会连接未知主机。
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	client := gossh.NewClient(clientConn, channels, requests)
	t.Cleanup(func() { _ = client.Close() })
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, &u
}

// TestShellPTYEnvironment 验证显式 TERM 优先级以及 SSH_TTY 与真实终端一致。
// 参数说明：t 为 *testing.T，执行有、无 SetEnv 的真实 PTY 会话。
// 返回值说明：无。
// 错误情况：SSH 请求失败、变量被覆盖、缺少真实终端路径或命令执行超时均失败。
func TestShellPTYEnvironment(t *testing.T) {
	for _, override := range []string{"", "xterm-256color"} {
		t.Run("override="+override, func(t *testing.T) {
			sess, u := newLoginTestSession(t)
			// 客户端不得改写账户目录或真实终端身份；验证兼容修复没有放宽环境白名单。
			for _, key := range []string{"HOME", "PATH", "SSH_TTY", "SSH_CONNECTION", "SSH_CLIENT"} {
				if err := sess.Setenv(key, "/invalid-client-override"); err != nil {
					t.Fatal(err)
				}
			}
			if err := sess.RequestPty("vt100", 24, 80, gossh.TerminalModes{}); err != nil {
				t.Fatal(err)
			}
			if override != "" {
				if err := sess.Setenv("TERM", override); err != nil {
					t.Fatal(err)
				}
			}
			output, err := sess.CombinedOutput(`printf 'TERM=%s\nHOME=%s\nSSH_CONNECTION=%s\nSSH_CLIENT=%s\n' "$TERM" "$HOME" "$SSH_CONNECTION" "$SSH_CLIENT"; test -n "$SSH_TTY" && test "$SSH_TTY" = "$(tty)"`)
			if err != nil {
				t.Errorf("PTY 环境不完整：%v，输出=%q", err, output)
			}
			want := "vt100"
			if override != "" {
				want = override
			}
			if !strings.Contains(string(output), "TERM="+want+"\r\n") {
				t.Errorf("TERM 期望 %q，实际输出=%q", want, output)
			}
			if !strings.Contains(string(output), "HOME="+u.HomeDir+"\r\n") {
				t.Errorf("客户端不应覆盖 HOME，输出=%q", output)
			}
			// 连接元数据必须来自真实 TCP 端点，格式分别为四段和三段，且不得接受客户端注入。
			for _, line := range strings.Split(string(output), "\r\n") {
				key, value, _ := strings.Cut(line, "=")
				if key == "SSH_CONNECTION" || key == "SSH_CLIENT" {
					wantFields := 4
					if key == "SSH_CLIENT" {
						wantFields = 3
					}
					if len(strings.Fields(value)) != wantFields {
						t.Errorf("%s 连接标识格式错误：%q", key, value)
					}
				}
			}
		})
	}
}

// TestShellWithoutPTYEnvironment 验证普通 exec 保留显式 TERM，且不会伪造交互终端。
// 参数说明：t 为 *testing.T，提供真实但不申请 PTY 的 SSH 会话。
// 返回值说明：无。
// 错误情况：TERM 丢失、出现 SSH_TTY 或命令执行失败时测试失败。
func TestShellWithoutPTYEnvironment(t *testing.T) {
	sess, _ := newLoginTestSession(t)
	if err := sess.Setenv("TERM", "xterm-256color"); err != nil {
		t.Fatal(err)
	}
	output, err := sess.CombinedOutput(`printf 'TERM=%s\n' "$TERM"; test -z "$SSH_TTY" && test -n "$SSH_CONNECTION" && test -n "$SSH_CLIENT"`)
	if err != nil || string(output) != "TERM=xterm-256color\n" {
		t.Fatalf("非 PTY 环境异常：%v，输出=%q", err, output)
	}
}

// TestShellLoginLoadsUserCommands 验证依赖 SSH 会话标识的用户启动文件能够加载个人命令。
// 参数说明：t 为 *testing.T，提供临时 HOME 与测试命令。
// 返回值说明：无。
// 错误情况：未加载登录文件、SSH_TTY 缺失或五秒内无法退出时失败；非 bash/zsh 平台跳过。
func TestShellLoginLoadsUserCommands(t *testing.T) {
	sess, u := newLoginTestSession(t)
	shell := filepath.Base(loginShell(u))
	if shell != "zsh" && shell != "bash" {
		t.Skip("此登录文件回归用例需要 bash 或 zsh")
	}
	bin := filepath.Join(u.HomeDir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "proxyd-test-tool"), []byte("#!/bin/sh\n# 输出独立标记，证明个人命令目录已由用户启动文件加入 PATH。\nprintf 'USER_TOOL_READY\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// 用户配置常以 SSH_TTY 区分远程登录与本地终端；只在真实 SSH 环境加载个人工具。
	profile := "# 仅为远程登录加载个人命令，模拟用户启动配置。\nif [ -n \"$SSH_TTY\" ] && [ -n \"$SSH_CONNECTION\" ]; then\n export PATH=\"$HOME/bin:$PATH\"\nfi\n"
	file := ".zprofile"
	if shell == "bash" {
		file = ".bash_profile"
	}
	if err := os.WriteFile(filepath.Join(u.HomeDir, file), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	sess.Stdin = strings.NewReader("proxyd-test-tool; exit\n")
	// 空命令走真实登录 shell；交互输入只执行临时工具，不依赖开发者安装的命令。
	output, err := sess.CombinedOutput("")
	if err != nil || !strings.Contains(string(output), "USER_TOOL_READY") {
		t.Fatalf("登录后个人环境未加载：%v，输出=%q", err, output)
	}
}

// TestSSHDiagnosticLoadsInteractiveProfile 验证诊断子系统确实加载交互用户启动文件。
// 参数说明：t 为 *testing.T，提供隔离 HOME 与真实 SSH/PTy；返回值说明：无。
// 错误情况：绕过交互/登录配置、TERM 覆盖失效或输出缺少结束标记时失败。
func TestSSHDiagnosticLoadsInteractiveProfile(t *testing.T) {
	sess, u := newLoginTestSession(t)
	shell := filepath.Base(loginShell(u))
	file := ".zshrc"
	profile := "# 诊断回归：必须同时加载交互与登录环境。\nif [ -n \"$PS1\" ]; then\n export PATH=\"$HOME/diagnostic-bin:$PATH\"\nfi\n"
	if shell == "bash" {
		file = ".bash_profile"
	} else if shell == "fish" {
		file = ".config/fish/config.fish"
		profile = "# 诊断回归：fish 必须保持交互登录语义，不依赖终端模拟器回应查询。\nif status is-interactive; and status is-login\n set -gx PATH \"$HOME/diagnostic-bin\" $PATH\nend\n"
	} else if shell != "zsh" {
		t.Skip("需要 bash/zsh/fish 启动文件")
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(u.HomeDir, file)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(u.HomeDir, file), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm-256color", 24, 160, gossh.TerminalModes{gossh.ECHO: 0}); err != nil {
		t.Fatal(err)
	}
	reader, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestSubsystem("proxyd-diagnostics"); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	if !strings.Contains(text, "PATH="+u.HomeDir+"/diagnostic-bin:") || !strings.Contains(text, "TERM=xterm-256color") || !strings.Contains(text, "PROXYD_DIAG_DONE") {
		t.Fatalf("诊断没有加载真实用户环境: %q", text)
	}
}

// TestSSHDiagnosticWithOpenSSH 验证原生 OpenSSH 的 PTY 子系统调用可以完成真实 shell 检查。
// 参数说明：t 为 *testing.T，所有启动文件使用临时 HOME；返回值说明：无。
// 错误情况：缺少系统 ssh 时跳过；协议调用、脚本注入或关闭流程失败时测试失败。
func TestSSHDiagnosticWithOpenSSH(t *testing.T) {
	executable, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("系统未安装 OpenSSH")
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	u := *current
	u.HomeDir = t.TempDir()
	host, err := loadOrCreateShellHostKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := &ssh.Server{SubsystemHandlers: map[string]ssh.SubsystemHandler{"proxyd-diagnostics": func(sess ssh.Session) { runShellDiagnostic(sess, &sessionUser{user: &u}) }}}
	server.AddHostKey(host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close(); server.Close() })
	go func() { _ = server.Serve(listener) }()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-F", os.DevNull, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile="+os.DevNull, "-o", "LogLevel=ERROR", "-o", "ControlPath=none", "-p", port, "-tt", "-s", "127.0.0.1", "proxyd-diagnostics")
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "PROXYD_DIAG_DONE") {
		t.Fatalf("原生 OpenSSH 子系统失败: %v %q", err, output)
	}
}
