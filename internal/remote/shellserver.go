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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	ssh "github.com/tailscale/gliderssh"
	gossh "golang.org/x/crypto/ssh"

	"proxyd/internal/config"
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
//   - shellUser: string，remote.shell-user 配置原值；root 运行时为空会被拒绝。
//
// 返回值说明：func(net.Conn) 和 error，处理器接管单条已认证的隧道或回环连接。
//
// 错误情况：状态目录创建、host key 读写或解析失败时返回错误，
// 不会返回半成品处理器。
func localShellSSHHandler(stateDir, shellUser string) (func(net.Conn), error) {
	// Web 终端依赖管理 API 与一次性回环令牌认证，不受远程 SSH 公钥开关影响。
	return configuredShellSSHHandler(stateDir, false, nil, shellUser)
}

// configuredShellSSHHandler 创建保留本项目 PTY、环境与断连清理逻辑的 SSH 服务。
// 参数说明：stateDir 为 string，host key 状态目录；required 为 bool，是否要求公钥；
// entries 为 []config.RemoteSSHKey，已授权的客户端公钥，绝不包含客户端私钥；
// shellUser 为 string，会话降权运行的本机账户（空=进程用户，root 下拒绝）。
// 返回值说明：func(net.Conn) 和 error，处理器接管已由隧道认证的单条连接。
// 错误情况：公钥无效或 host key 不可用时拒绝构造；required=true 且列表为空时
// 保持拒绝全部，绝不因删除最后一把公钥而降级为免认证。
func configuredShellSSHHandler(stateDir string, required bool, entries []config.RemoteSSHKey, shellUser string) (func(net.Conn), error) {
	keys, err := NormalizeSSHKeys(entries)
	if err != nil {
		return nil, err
	}
	policy := newSSHAccess(newAuditLog(remoteAuditCapacity))
	policy.update(required, keys)
	return managedShellSSHHandler(stateDir, policy, shellUser)
}

// managedShellSSHHandler 构造共享动态授权策略的 SSH 入口。
// 参数说明：stateDir 为 string，host key 保存目录；policy 为 *sshAccess，管理器共享策略；
// shellUser 为 string，remote.shell-user 配置原值，逐会话解析为降权身份。
// 返回值说明：func(net.Conn) 与 error；每条连接持有独立 SSH 协议状态。
// 错误情况：host key 加载失败返回错误；握手超过 30 秒会关闭连接。
// 公钥探测与签名完成分别检查策略，防止探测缓存跨越禁用或到期边界。
func managedShellSSHHandler(stateDir string, policy *sshAccess, shellUser string) (func(net.Conn), error) {
	signer, err := loadOrCreateShellHostKey(stateDir)
	if err != nil {
		return nil, fmt.Errorf("加载内嵌 SSH host key 失败: %w", err)
	}
	return func(conn net.Conn) {
		attempted := ""
		// callbacks 与 HandleConn 在同一握手协程执行；会话索引的并发读写由 policy 锁保护。
		defer func() { policy.finished(conn, attempted) }()
		srv := &ssh.Server{
			Handler:          func(sess ssh.Session) { shellSessionHandler(sess, shellUser) },
			HandshakeTimeout: 30 * time.Second,
			ChannelHandlers:  map[string]ssh.ChannelHandler{"session": ssh.DefaultSessionHandler},
			RequestHandlers:  map[string]ssh.RequestHandler{},
			SubsystemHandlers: map[string]ssh.SubsystemHandler{
				"proxyd-diagnostics": func(sess ssh.Session) { shellDiagnosticHandler(sess, shellUser) },
				"sftp":               func(sess ssh.Session) { shellSFTPHandler(sess, shellUser) },
			},
		}
		srv.PublicKeyHandler = func(_ ssh.Context, public ssh.PublicKey) error {
			attempted = gossh.FingerprintSHA256(public)
			return policy.check(attempted)
		}
		// 两种协议认证入口始终存在，none 是否放行也读取实时策略，因此无需重建隧道。
		srv.NoClientAuthHandler = func(_ ssh.Context) error { return policy.authenticated(conn, "") }
		srv.ServerConfigCallback = func(_ ssh.Context) *gossh.ServerConfig {
			return &gossh.ServerConfig{NoClientAuth: true,
				VerifiedPublicKeyCallback: func(_ gossh.ConnMetadata, public gossh.PublicKey, permissions *gossh.Permissions, _ string) (*gossh.Permissions, error) {
					err := policy.authenticated(conn, gossh.FingerprintSHA256(public))
					return permissions, err
				},
			}
		}
		srv.AddHostKey(signer)
		srv.HandleConn(conn)
	}, nil
}

// shellSFTPHandler 以与交互 shell 相同的本机用户权限处理标准 SFTP 子系统。
//
// 参数说明：
//   - sess: ssh.Session，已经通过隧道身份及可选 SSH 公钥认证的会话。
//   - shellUser: string，remote.shell-user 配置原值；客户端声明的 SSH 用户名不参与授权。
//
// 返回值说明：无；成功或失败均通过 SSH session 退出码结束。
//
// 错误情况：用户解析、SFTP 初始化或协议处理失败时写入 stderr 并返回退出码 1。
// 当会话无需降权时可在当前进程安全运行；需要从 root 切换到 shell-user 时必须启动
// 独立子进程，避免并发 goroutine 中修改整个 Go 进程的有效 UID。
func shellSFTPHandler(sess ssh.Session, shellUser string) {
	su, err := resolveSessionUser(shellUser)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "%v\r\n", err)
		_ = sess.Exit(1)
		return
	}
	if needsSFTPSubprocess(su) {
		runShellSessionCommand(sess, newSFTPServerCommand(su))
		return
	}
	server, err := sftp.NewServer(sess, sftp.WithServerWorkingDirectory(su.user.HomeDir))
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "sftp init: %v\r\n", err)
		_ = sess.Exit(1)
		return
	}
	defer server.Close()
	if err := server.Serve(); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(sess.Stderr(), "sftp serve: %v\r\n", err)
		_ = sess.Exit(1)
		return
	}
	_ = sess.Exit(0)
}

// ServeSFTP 在当前进程的标准流上运行 SFTP 协议服务。
//
// 功能说明：该函数仅供隐藏的 __remote-sftp 子命令调用。父进程在启动前设置工作目录、
// 最小环境和 Unix 降权凭据，因此子进程的所有文件系统访问都受 shell-user 权限约束。
//
// 参数说明：
//   - input: io.Reader，接收 SFTP 二进制请求，通常为 os.Stdin。
//   - output: io.WriteCloser，写出 SFTP 二进制响应，通常为 os.Stdout。
//
// 返回值说明：error；客户端正常关闭输入时返回 nil，其余初始化或协议错误原样返回。
//
// 错误情况：协议初始化失败或底层标准流异常时返回错误；调用方只可把错误写到 stderr，
// 绝不能混入 stdout 的 SFTP 数据流。
func ServeSFTP(input io.Reader, output io.WriteCloser) error {
	stream := &sftpHelperStream{Reader: input, output: output}
	server, err := sftp.NewServer(stream)
	if err != nil {
		return fmt.Errorf("初始化 SFTP 服务失败: %w", err)
	}
	defer server.Close()
	if err := server.Serve(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("运行 SFTP 服务失败: %w", err)
	}
	return nil
}

// sftpHelperStream 把子进程分离的 stdin/stdout 适配为 pkg/sftp 所需的全双工流。
// Reader 只读取 stdin，写入和关闭只作用于 stdout；进程退出会由操作系统回收 stdin。
type sftpHelperStream struct {
	io.Reader
	output io.WriteCloser
}

// Write 把 SFTP 响应写入子进程 stdout。
// 参数说明：data 为待写出的协议帧字节。
// 返回值说明：写入字节数和底层错误。
// 错误情况：stdout 已关闭或系统写入失败时返回错误。
func (s *sftpHelperStream) Write(data []byte) (int, error) { return s.output.Write(data) }

// Close 关闭 SFTP 响应输出流。
// 参数说明：无。
// 返回值说明：stdout 的关闭错误。
// 错误情况：重复关闭或系统关闭失败时由底层返回错误。
func (s *sftpHelperStream) Close() error { return s.output.Close() }

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
//   - shellUser: string，remote.shell-user 配置原值；会话用户只由它或进程用户决定。
//
// 返回值说明：无；shell 退出码经 sess.Exit 回传给客户端。
//
// 错误情况：无法解析会话用户（含 root 未配置 shell-user）时向会话写错误并以退出码 1 结束。
func shellSessionHandler(sess ssh.Session, shellUser string) {
	su, err := resolveSessionUser(shellUser)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "%v\r\n", err)
		sess.Exit(1)
		return
	}
	runShellSession(sess, su)
}

// runShellSession 为已解析的会话用户启动 SSH 会话，统一内嵌 SSH 与 Web 终端的执行路径。
//
// 参数说明：sess 为 ssh.Session，承载命令、环境和终端请求；su 为 *sessionUser，
// 必须是调用方已确认的降权身份，不能直接使用客户端声明的 SSH 用户名。
// 返回值说明：无，退出状态通过 sess.Exit 回传。
// 错误情况：命令或终端启动失败时由对应执行器写入错误并结束会话。
func runShellSession(sess ssh.Session, su *sessionUser) {
	cmd := newShellSessionCommand(su, sess.RawCommand())
	runShellSessionCommand(sess, cmd)
}

// runShellSessionCommand 将已构造的命令接入统一 SSH 环境、PTY 与断连清理流程。
// 参数说明：sess 为 ssh.Session，提供经过过滤的客户端环境；cmd 为 *exec.Cmd，
// 必须由服务端构造且尚未启动，诊断命令和普通会话共用此执行边界。
// 返回值说明：无，命令退出状态通过 sess.Exit 回传。
// 错误情况：PTY 或进程启动失败由执行器报告；所有会话继续使用相同的环境白名单。
func runShellSessionCommand(sess ssh.Session, cmd *exec.Cmd) {
	// 包括无 PTY exec 在内的 SSH 会话都应携带连接标识，否则用户启动脚本可能
	// 误入本地终端分支（例如等待本地交互）。这里记录 SSH 传输端点；经隧道或
	// 回环接入时它们是隧道/回环地址，不冒充客户端公网地址。非 TCP 地址则不伪造。
	clientIP, clientPort, clientErr := net.SplitHostPort(sess.RemoteAddr().String())
	serverIP, serverPort, serverErr := net.SplitHostPort(sess.LocalAddr().String())
	if clientErr == nil && serverErr == nil {
		cmd.Env = append(cmd.Env,
			"SSH_CLIENT="+strings.Join([]string{clientIP, clientPort, serverPort}, " "),
			"SSH_CONNECTION="+strings.Join([]string{clientIP, clientPort, serverIP, serverPort}, " "),
		)
	}
	ptyReq, winCh, isPTY := sess.Pty()
	// PTY 提供默认终端类型，随后应用客户端显式 SetEnv；若顺序相反，
	// 用户为远端缺失 terminfo 设置的兼容 TERM 会在进程启动前被悄悄覆盖。
	if isPTY && ptyReq.Term != "" {
		cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
	}
	for _, env := range sess.Environ() {
		if acceptShellEnvPair(env) {
			cmd.Env = append(cmd.Env, env)
		}
	}
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
