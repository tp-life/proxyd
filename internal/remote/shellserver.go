//go:build linux || darwin || windows

package remote

// 本文件实现 Web Terminal 专用的进程内免认证 SSH shell 服务，独立于 tailcat 隧道
// 服务端运行：远程连接只使用客户端功能（不开服务端）时浏览器终端依然可用。
// 会话只经 openAuthenticatedLoopback 的一次性令牌回环暴露，不监听任何持久端口。
//
// 会话与 PTY 处理逻辑移植自 tailcat（BSD-3-Clause）：
// Portions Copyright (c) Tailscale Inc & contributors.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	ssh "github.com/tailscale/gliderssh"
	gossh "golang.org/x/crypto/ssh"
)

// shellHostKeyRelPath 是内嵌 SSH host key 相对于 state-dir 的固定位置。
const shellHostKeyRelPath = "remote/ssh_host_ed25519_key"

// shellHostKeyMu 串行化 SSH host key 的首次生成，避免 Web Terminal 与
// builtin-ssh 同时首次启动时各自写出不同的密钥。
var shellHostKeyMu sync.Mutex

// localShellSSHHandler 构造只服务交互 shell 的免认证 SSH 连接处理器。
//
// 参数说明：
//   - stateDir: string，proxyd 状态目录，host key 保存在其 remote 子目录。
//
// 返回值说明：func(net.Conn) 和 error，处理器接管单条已认证的隧道或回环连接。
//
// 错误情况：状态目录创建、host key 读写或解析失败时返回错误，
// 不会返回半成品处理器。
func localShellSSHHandler(stateDir string) (func(net.Conn), error) {
	signer, err := loadOrCreateShellHostKey(stateDir)
	if err != nil {
		return nil, fmt.Errorf("加载内嵌 SSH host key 失败: %w", err)
	}
	srv := &ssh.Server{
		Handler:             shellSessionHandler,
		NoClientAuthHandler: func(ssh.Context) error { return nil },
		ChannelHandlers:     map[string]ssh.ChannelHandler{"session": ssh.DefaultSessionHandler},
		RequestHandlers:     map[string]ssh.RequestHandler{},
		SubsystemHandlers:   map[string]ssh.SubsystemHandler{},
	}
	srv.AddHostKey(signer)
	return srv.HandleConn, nil
}

// loadOrCreateShellHostKey 读取持久化 ed25519 SSH host key，不存在时原子生成。
//
// 参数说明：
//   - stateDir: string，proxyd 状态目录；不允许空值，避免密钥落到未知相对路径。
//
// 返回值说明：
//   - gossh.Signer：可直接注册到 gliderssh.Server 的签名器。
//   - error：文件系统、随机数、密钥编码或解析失败。
//
// 错误情况：新密钥先以 0600 写入同目录临时文件再 rename；
// 任一步失败都清理临时文件，不会用半截 PEM 覆盖旧密钥。
func loadOrCreateShellHostKey(stateDir string) (gossh.Signer, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, fmt.Errorf("状态目录为空")
	}
	shellHostKeyMu.Lock()
	defer shellHostKeyMu.Unlock()

	path := filepath.Join(stateDir, shellHostKeyRelPath)
	data, err := os.ReadFile(path)
	if err == nil {
		// 托管私钥即使被备份工具或人工操作放宽权限，也在每次加载时
		// 收紧为 0600；权限无法修复时拒绝启动，避免继续使用可被读取的密钥。
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
		return gossh.ParsePrivateKey(data)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	data = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey})
	temporary, err := os.CreateTemp(filepath.Dir(path), ".ssh-host-key-*")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return nil, err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return nil, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return nil, err
	}
	return gossh.NewSignerFromKey(privateKey)
}

// shellSessionHandler 处理单个 SSH 会话（交互 shell 或 exec）。
//
// 参数说明：
//   - sess: ssh.Session，已完成免认证握手的会话。
//
// 返回值说明：无；shell 退出码经 sess.Exit 回传给客户端。
//
// 错误情况：无法解析当前用户时向会话写错误并以退出码 1 结束。
func shellSessionHandler(sess ssh.Session) {
	u, err := user.Current()
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "获取当前用户失败: %v\r\n", err)
		sess.Exit(1)
		return
	}
	cmd := newShellSessionCommand(u, sess.RawCommand())
	for _, env := range sess.Environ() {
		if acceptShellEnvPair(env) {
			cmd.Env = append(cmd.Env, env)
		}
	}
	ptyReq, winCh, isPTY := sess.Pty()
	if isPTY {
		sess.DisablePTYEmulation()
		runShellWithPTY(sess, cmd, ptyReq, winCh)
		return
	}
	runShellWithPipes(sess, cmd)
}

// runShellWithPipes 以管道（无 PTY）方式运行命令并桥接会话流。
//
// 参数说明：
//   - sess: ssh.Session，未申请 PTY 的会话。
//   - cmd: *exec.Cmd，尚未启动的 shell 命令。
//
// 返回值说明：无；命令退出码经 sess.Exit 回传。
//
// 错误情况：管道创建或进程启动失败时向会话写错误并以退出码 1 结束。
func runShellWithPipes(sess ssh.Session, cmd *exec.Cmd) {
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "stdin pipe: %v\r\n", err)
		sess.Exit(1)
		return
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdinPipe.Close()
		fmt.Fprintf(sess.Stderr(), "stdout pipe: %v\r\n", err)
		sess.Exit(1)
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = stdinPipe.Close()
		_ = stdoutPipe.Close()
		fmt.Fprintf(sess.Stderr(), "stderr pipe: %v\r\n", err)
		sess.Exit(1)
		return
	}
	// 无 PTY 命令也可能派生子进程；在启动前设置平台进程属性，
	// 使断连时的终止动作不只清理最外层 shell。
	prepareShellProcess(cmd)
	if err := cmd.Start(); err != nil {
		// StdinPipe/StdoutPipe/StderrPipe 已经创建真实管道 fd；Start 失败时
		// exec.Cmd 不会替调用方关闭它们，必须在错误路径显式回收。
		_ = stdinPipe.Close()
		_ = stdoutPipe.Close()
		_ = stderrPipe.Close()
		fmt.Fprintf(sess.Stderr(), "start: %v\r\n", err)
		sess.Exit(1)
		return
	}
	stopWatching := watchShellSession(sess, cmd, func() {
		_ = stdinPipe.Close()
		_ = stdoutPipe.Close()
		_ = stderrPipe.Close()
	})
	defer stopWatching()

	go func() {
		defer stdinPipe.Close()
		_, _ = io.Copy(stdinPipe, sess)
	}()
	// 先排空 stdout/stderr 再 Wait：Wait 会在进程退出时关闭管道，
	// 与拷贝协程竞争可能丢失快速退出命令的输出。
	outputDone := make(chan struct{})
	var openStreams atomic.Int32
	openStreams.Store(2)
	closeOutput := func() {
		if openStreams.Add(-1) == 0 {
			close(outputDone)
		}
	}
	go func() {
		defer closeOutput()
		_, _ = io.Copy(sess, stdoutPipe)
	}()
	go func() {
		defer closeOutput()
		_, _ = io.Copy(sess.Stderr(), stderrPipe)
	}()
	<-outputDone
	if err := cmd.Wait(); err != nil {
		sess.Exit(shellExitCode(err))
		return
	}
	sess.Exit(0)
}

// watchShellSession 在 SSH 连接断开时终止 shell 进程组并关闭传输资源。
//
// 参数说明：
//   - sess: ssh.Session，Context 在客户端断开或传输失败时取消。
//   - cmd: *exec.Cmd，已经成功启动的 shell 进程。
//   - closeIO: func()，用于关闭 PTY 或进程管道，使阻塞的 io.Copy 立即返回。
//
// 返回值说明：func()，命令正常结束后调用以停止监听协程。
//
// 错误情况：关闭与终止错误在会话收尾阶段无法可靠回传，因此按幂等清理处理；
// 必须同时关闭 I/O 和终止进程组，否则忽略 stdin 的长运行命令会在断连后泄漏。
func watchShellSession(sess ssh.Session, cmd *exec.Cmd, closeIO func()) func() {
	done := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		select {
		case <-sess.Context().Done():
			closeIO()
			terminateShellProcess(cmd)
		case <-done:
		}
	}()
	return func() {
		stopOnce.Do(func() { close(done) })
	}
}

// shellExitCode 从进程错误中提取退出码。
//
// 参数说明：
//   - err: error，cmd.Wait 返回的错误。
//
// 返回值说明：int，非 ExitError 一律按 1 处理。
//
// 错误情况：无。
func shellExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

// acceptShellEnvPair 判定客户端环境变量是否接受（与 OpenSSH AcceptEnv 默认一致）。
//
// 参数说明：
//   - kv: string，key=value 形式的环境变量。
//
// 返回值说明：bool，仅 TERM、LANG 与 LC_* 前缀放行。
//
// 错误情况：无；无等号的条目直接拒绝。
func acceptShellEnvPair(kv string) bool {
	k, _, ok := strings.Cut(kv, "=")
	if !ok {
		return false
	}
	return k == "TERM" || k == "LANG" || strings.HasPrefix(k, "LC_")
}
