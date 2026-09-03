package remote

// 本文件实现浏览器终端到进程内 builtin-ssh 的会话适配。
// WebSocket、HTTP 与页面状态不进入本层；本层只负责建立一次性 loopback SSH、申请 PTY、
// 转发字节流和同步窗口大小，从而把 tailcat/SSH 的不稳定实现细节隔离在 remote 模块内。

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"golang.org/x/crypto/ssh"
)

const (
	// terminalHandshakeTimeout 限制回环 SSH 握手与 host key 初始化耗时，避免异常磁盘环境
	// 让 HTTP handler 永久占用一个请求协程。
	terminalHandshakeTimeout = 10 * time.Second
	// 终端尺寸上限既覆盖常见超宽屏，也防止恶意 WebSocket 消息把异常尺寸传到 PTY ioctl。
	maxTerminalColumns = 1000
	maxTerminalRows    = 500
)

var (
	// ErrWebTerminalDisabled 表示高权限浏览器终端入口尚未显式开启。
	ErrWebTerminalDisabled = errors.New("Web 终端未开启")
	// ErrBuiltinSSHRequired 表示浏览器终端依赖的进程内 SSH 服务尚未开启。
	ErrBuiltinSSHRequired = errors.New("Web 终端需要先开启内嵌免密 SSH")
	// ErrRemoteServerNotRunning 表示没有可供内存 loopback 复用的运行中 tailcat 服务端。
	ErrRemoteServerNotRunning = errors.New("远程连接服务端未运行")
)

// TerminalSize 是浏览器与 PTY 之间传递的字符网格尺寸值对象。
type TerminalSize struct {
	Columns int `json:"cols"`
	Rows    int `json:"rows"`
}

// NormalizeTerminalSize 把浏览器提交的终端尺寸夹紧到安全、可用范围。
//
// 参数说明：
//   - size: TerminalSize，xterm 当前列数与行数；零值常见于 Dialog 尚未完成布局。
//
// 返回值说明：TerminalSize，列数范围 2..1000、行数范围 1..500；零值采用 80x24。
//
// 错误情况：无；异常负数或超大值不会继续传到 SSH window-change/PTY ioctl。
func NormalizeTerminalSize(size TerminalSize) TerminalSize {
	if size.Columns <= 0 {
		size.Columns = 80
	}
	if size.Rows <= 0 {
		size.Rows = 24
	}
	size.Columns = min(max(2, size.Columns), maxTerminalColumns)
	size.Rows = min(max(1, size.Rows), maxTerminalRows)
	return size
}

// TerminalSession 是一个已经进入交互 shell 的进程内 SSH 会话。
// 它以 io.ReadWriteCloser 形态向 API 层暴露纯终端字节流，并保留独立 Resize 操作。
type TerminalSession struct {
	input        *io.PipeWriter
	output       *io.PipeReader
	outputWriter *io.PipeWriter
	sshSession   *ssh.Session
	sshClient    *ssh.Client
	transport    net.Conn
	closeOnce    sync.Once
	done         chan struct{}
	outputReady  chan struct{}
	resizeReady  chan struct{}
	resizeMu     sync.Mutex
	pendingSize  TerminalSize
	hasPending   bool
}

// terminalReadyWriter 在第一段 shell 输出进入本地输出管道前发布就绪信号。
// 信号先于可能阻塞的 io.Pipe 写入发出，保证尚未启动 API 输出读取时也能开放 resize。
type terminalReadyWriter struct {
	writer io.Writer
	ready  chan struct{}
	once   sync.Once
}

// Write 转发 SSH stdout/stderr，并以首个非空写入作为 session handler 已读取 PTY 的同步点。
//
// 参数说明：
//   - data: []byte，SSH 客户端收到的终端输出。
//
// 返回值说明：int 和 error，保持底层 io.Writer 语义。
//
// 错误情况：底层输出管道关闭时返回写错误；就绪信号只关闭一次，stdout/stderr 并发安全。
func (w *terminalReadyWriter) Write(data []byte) (int, error) {
	if len(data) > 0 {
		w.once.Do(func() { close(w.ready) })
	}
	return w.writer.Write(data)
}

// OpenWebTerminal 经内存 loopback 连接当前运行中的 tailcat builtin-ssh，并申请交互 PTY。
//
// 参数说明：
//   - ctx: context.Context，浏览器请求生命周期；取消后会主动关闭 shell 与 SSH 传输。
//   - size: TerminalSize，首次打开时的字符列数和行数。
//
// 返回值说明：*TerminalSession 和 error；成功会话已完成 SSH 握手、PTY 请求与 Shell 启动。
//
// 错误情况：Web 终端关闭、builtin-ssh 关闭、remote 服务端未运行、当前平台不支持 SSH、
// host key 初始化失败、握手/PTY/Shell 启动失败或请求取消时返回错误，所有半建连接都会关闭。
func (m *Manager) OpenWebTerminal(ctx context.Context, size TerminalSize) (*TerminalSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	cfg := m.cfg.Clone()
	server := m.srv
	m.mu.Unlock()
	if !cfg.WebTerminal {
		return nil, ErrWebTerminalDisabled
	}
	if !cfg.BuiltinSSH {
		return nil, ErrBuiltinSSHRequired
	}
	if server == nil {
		return nil, ErrRemoteServerNotRunning
	}
	if !tailcat.SupportsSSHServer() {
		return nil, fmt.Errorf("当前平台不支持 Web 终端依赖的内嵌 SSH")
	}

	handler := server.SSHConnHandler(tailcat.SSHOptions{Shell: true})
	clientSide, err := openAuthenticatedLoopback(ctx, handler)
	if err != nil {
		return nil, err
	}

	// NewClientConn 不直接接收 context；同时设置最晚 deadline 并监听请求取消，确保
	// host key 生成或协议握手异常时能可靠解阻塞。握手完成后清除 deadline，交互 shell
	// 的寿命只由 WebSocket/request context 控制。
	deadline := time.Now().Add(terminalHandshakeTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = clientSide.SetDeadline(deadline)
	handshakeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = clientSide.Close()
		case <-handshakeDone:
		}
	}()
	sshConnection, channels, requests, err := ssh.NewClientConn(clientSide, "proxyd-web-terminal", &ssh.ClientConfig{
		User: "proxyd-web-terminal",
		// 传输端点是本进程刚创建的 net.Pipe，不经过内核网络或可被劫持的 DNS/TCP；
		// 接受该进程内 builtin-ssh 的 host key 不会扩大信任边界，也避免依赖 known_hosts。
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	close(handshakeDone)
	if err != nil {
		_ = clientSide.Close()
		return nil, fmt.Errorf("建立进程内 SSH 会话失败: %w", err)
	}
	_ = clientSide.SetDeadline(time.Time{})
	client := ssh.NewClient(sshConnection, channels, requests)
	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("创建进程内 SSH session 失败: %w", err)
	}

	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	outputReady := make(chan struct{})
	readyWriter := &terminalReadyWriter{writer: outputWriter, ready: outputReady}
	session.Stdin = inputReader
	// PTY 模式下正常 stdout/stderr 已由 tailcat 合流；初始化失败信息仍可能走 stderr。
	// io.Pipe 支持并发写，把两者统一送到浏览器可避免遗漏明确的 shell 启动错误。
	session.Stdout = readyWriter
	session.Stderr = readyWriter
	normalized := NormalizeTerminalSize(size)
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", normalized.Rows, normalized.Columns, modes); err != nil {
		_ = inputReader.Close()
		_ = inputWriter.Close()
		_ = outputReader.Close()
		_ = outputWriter.Close()
		_ = session.Close()
		_ = client.Close()
		return nil, fmt.Errorf("申请进程内 SSH PTY 失败: %w", err)
	}
	if err := session.Shell(); err != nil {
		_ = inputReader.Close()
		_ = inputWriter.Close()
		_ = outputReader.Close()
		_ = outputWriter.Close()
		_ = session.Close()
		_ = client.Close()
		return nil, fmt.Errorf("启动进程内 SSH shell 失败: %w", err)
	}

	terminal := &TerminalSession{
		input:        inputWriter,
		output:       outputReader,
		outputWriter: outputWriter,
		sshSession:   session,
		sshClient:    client,
		transport:    clientSide,
		done:         make(chan struct{}),
		outputReady:  outputReady,
		resizeReady:  make(chan struct{}),
	}
	go terminal.wait()
	go terminal.unlockResizeWhenReady()
	go func() {
		select {
		case <-ctx.Done():
			_ = terminal.Close()
		case <-terminal.done:
		}
	}()
	return terminal, nil
}

// openAuthenticatedLoopback 建立仅接受一次连接的 127.0.0.1 传输，并在进入 SSH 前校验
// 随机令牌。
//
// 参数说明：
//   - ctx: context.Context，用于限制监听与拨号阶段。
//   - handler: func(net.Conn)，通过令牌校验后处理服务端 SSH 协议的函数。
//
// 返回值说明：net.Conn 和 error；成功返回已经完成前置令牌写入的客户端连接。
//
// 错误情况：随机数生成、回环监听、拨号或令牌写入失败时返回错误并关闭所有资源。
// 监听器在接受第一条连接后立即关闭；随机令牌用于防止其他本机进程在极短监听窗口中
// 抢先连接并直接进入免认证 SSH。net.Pipe 不适用这里，因为 SSH 两端会同时写版本头，
// 无缓冲管道会形成协议级互锁。
func openAuthenticatedLoopback(ctx context.Context, handler func(net.Conn)) (net.Conn, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, fmt.Errorf("生成 Web 终端回环令牌失败: %w", err)
	}

	listenConfig := net.ListenConfig{}
	listener, err := listenConfig.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("创建 Web 终端回环监听失败: %w", err)
	}
	go func() {
		serverSide, acceptErr := listener.Accept()
		_ = listener.Close()
		if acceptErr != nil {
			return
		}
		defer serverSide.Close()

		// 前置校验设置独立超时：即使抢占连接的本机进程不发送数据，也不会长期占用
		// handler。比较使用常量时间实现，令牌通过后才清除 deadline 并进入 SSH。
		_ = serverSide.SetDeadline(time.Now().Add(terminalHandshakeTimeout))
		var received [32]byte
		if _, readErr := io.ReadFull(serverSide, received[:]); readErr != nil {
			return
		}
		if subtle.ConstantTimeCompare(received[:], token[:]) != 1 {
			return
		}
		_ = serverSide.SetDeadline(time.Time{})
		handler(serverSide)
	}()

	clientSide, err := (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("连接 Web 终端回环监听失败: %w", err)
	}
	if written, writeErr := clientSide.Write(token[:]); writeErr != nil || written != len(token) {
		_ = clientSide.Close()
		_ = listener.Close()
		if writeErr != nil {
			return nil, fmt.Errorf("写入 Web 终端回环令牌失败: %w", writeErr)
		}
		return nil, fmt.Errorf("写入 Web 终端回环令牌不完整: %d/%d", written, len(token))
	}
	return clientSide, nil
}

// Read 读取 shell 输出，供 WebSocket 服务端封装为二进制消息。
//
// 参数说明：
//   - buffer: []byte，接收终端原始字节的调用方缓冲区。
//
// 返回值说明：int 和 error，完全保持 io.Reader 语义；shell 退出且输出排空后返回 io.EOF。
//
// 错误情况：会话被取消、SSH 连接异常或调用 Close 时可能返回 pipe/closed 错误。
func (s *TerminalSession) Read(buffer []byte) (int, error) {
	return s.output.Read(buffer)
}

// Write 把浏览器键盘/粘贴产生的原始字节写入 shell stdin。
//
// 参数说明：
//   - data: []byte，UTF-8 文本或终端控制序列；不做内容解释和命令过滤。
//
// 返回值说明：int 和 error，完全保持 io.Writer 语义。
//
// 错误情况：shell 已退出、会话已关闭或 SSH 传输断开时返回 pipe 错误。
func (s *TerminalSession) Write(data []byte) (int, error) {
	return s.input.Write(data)
}

// Resize 把 xterm 的字符网格变化同步成 SSH window-change 请求。
//
// 参数说明：
//   - size: TerminalSize，浏览器当前列数与行数，会先经过安全范围夹紧。
//
// 返回值说明：error，服务端接受窗口变更时为 nil。
//
// 错误情况：SSH session 已结束或传输断开时返回错误；异常尺寸不会直接传给 PTY。
func (s *TerminalSession) Resize(size TerminalSize) error {
	normalized := NormalizeTerminalSize(size)
	s.resizeMu.Lock()
	select {
	case <-s.done:
		s.resizeMu.Unlock()
		return net.ErrClosed
	default:
	}
	select {
	case <-s.resizeReady:
		err := s.sshSession.WindowChange(normalized.Rows, normalized.Columns)
		s.resizeMu.Unlock()
		return err
	default:
		// 上游 handler 尚未通过首段 shell 输出确认 PTY 读取完成时只覆盖最新尺寸，
		// 不阻塞 WebSocket 输入方向；否则一个无提示符 shell 会让后续键盘输入无法抵达。
		s.pendingSize = normalized
		s.hasPending = true
	}
	outputStarted := false
	select {
	case <-s.outputReady:
		outputStarted = true
	default:
	}
	s.resizeMu.Unlock()
	if outputStarted {
		// 首段输出已经到达但解锁协程尚未取得 resizeMu 时，等待它先应用刚保存的尺寸。
		// resizeReady 在 WindowChange 返回后才关闭，因此返回 nil 表示该尺寸确已提交。
		select {
		case <-s.resizeReady:
			return nil
		case <-s.done:
			return net.ErrClosed
		}
	}
	return nil
}

// unlockResizeWhenReady 在上游 SSH session handler 完成首次 PTY 读取后开放窗口更新。
//
// 参数说明：无。
//
// 返回值说明：无；收到首段 shell 输出后先应用等待中的最新尺寸，再关闭 resizeReady。
//
// 错误情况：会话在等待期内关闭时提前退出；等待中的 WindowChange 失败会关闭整个会话，
// 后续 Resize 从 done 分支收到 net.ErrClosed。无提示符 shell 不会阻塞键盘输入；首段命令输出
// 到达后再应用最后一次尺寸。
func (s *TerminalSession) unlockResizeWhenReady() {
	select {
	case <-s.outputReady:
	case <-s.done:
		return
	}

	s.resizeMu.Lock()
	var err error
	if s.hasPending {
		err = s.sshSession.WindowChange(s.pendingSize.Rows, s.pendingSize.Columns)
		s.hasPending = false
	}
	close(s.resizeReady)
	s.resizeMu.Unlock()
	if err != nil {
		_ = s.Close()
	}
}

// Close 幂等关闭 shell、管道和 SSH transport，保证请求取消或浏览器断开时不遗留子进程。
//
// 参数说明：无。
//
// 返回值说明：error，当前实现始终返回 nil；重复调用安全。
//
// 错误情况：底层 close 错误不再上抛，因为关闭可能由 shell 正常退出与网络断开并发触发。
func (s *TerminalSession) Close() error {
	s.closeOnce.Do(func() {
		_ = s.input.Close()
		_ = s.sshSession.Close()
		_ = s.sshClient.Close()
		_ = s.transport.Close()
		_ = s.outputWriter.Close()
		close(s.done)
	})
	return nil
}

// wait 等待远端 shell 结束，先让 x/crypto/ssh 排空 stdout/stderr，再统一关闭会话资源。
//
// 参数说明：无。
//
// 返回值说明：无；仅由 OpenWebTerminal 启动的后台协程调用。
//
// 错误情况：非零退出码不额外注入终端输出，shell 自身错误已经在 PTY 字节流中呈现；
// 无论 Wait 原因如何都执行幂等 Close。
func (s *TerminalSession) wait() {
	_ = s.sshSession.Wait()
	_ = s.Close()
}
