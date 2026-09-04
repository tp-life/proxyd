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
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"os/user"
	"strings"
	"sync"
	"sync/atomic"

	ssh "github.com/tailscale/gliderssh"
	gossh "golang.org/x/crypto/ssh"
)

// localShellHostKey 返回进程内一次性的 ed25519 host key。
// 客户端固定走 InsecureIgnoreHostKey 的令牌回环，持久化 host key 没有意义。
var localShellHostKey = sync.OnceValues(func() (gossh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return gossh.NewSignerFromKey(priv)
})

// localShellSSHHandler 构造只服务交互 shell 的免认证 SSH 连接处理器。
//
// 参数说明：无。
//
// 返回值说明：func(net.Conn) 和 error，处理器语义与 tailcat Server.SSHConnHandler 相同。
//
// 错误情况：host key 生成失败时返回错误，不会返回半成品处理器。
func localShellSSHHandler() (func(net.Conn), error) {
	signer, err := localShellHostKey()
	if err != nil {
		return nil, fmt.Errorf("生成 Web 终端 SSH host key 失败: %w", err)
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
		fmt.Fprintf(sess.Stderr(), "stdout pipe: %v\r\n", err)
		sess.Exit(1)
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "stderr pipe: %v\r\n", err)
		sess.Exit(1)
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(sess.Stderr(), "start: %v\r\n", err)
		sess.Exit(1)
		return
	}

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
