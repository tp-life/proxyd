package remote

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"

	"proxyd/internal/config"
)

// forwardRunner 是一条运行中的本地转发：监听本地端口，把每条连接
// 经隧道拨到远端端口后双向拷贝。
type forwardRunner struct {
	name      string
	listen    string
	token     tailcat.ConnBlob
	port      uint16
	logf      func(format string, args ...any)
	clientKey key.NodePrivate // 本机客户端身份（对端 --allow 白名单用）；零值=临时身份

	// dial 可注入替换（测试）；为 nil 时内部创建复用的 tailcat 客户端。
	dial func(ctx context.Context) (net.Conn, error)

	ln     net.Listener
	client *tailcat.Client
	done   chan struct{}
	active atomic.Int64
	ctx    context.Context
	cancel context.CancelFunc

	stopOnce sync.Once
	connMu   sync.Mutex
	conns    map[net.Conn]struct{}
	stopped  bool

	mu      sync.Mutex
	lastErr string
}

// newForwardRunner 构造一条具有独立生命周期的转发 runner。
//
// 参数说明：
//   - name: string，转发名称，用于日志和错误定位。
//   - listen: string，本地 TCP 监听地址。
//   - token: tailcat.ConnBlob，远端 tailcat 连接凭据。
//   - port: uint16，隧道远端的目标端口。
//   - logf: func，记录运行错误的日志函数。
//   - clientKey: key.NodePrivate，用于对端白名单验证的客户端身份。
//   - dial: func，可选的测试拨号器；nil 表示启动时创建真实 tailcat 客户端。
//
// 返回值说明：*forwardRunner，尚未启动监听的转发器。
//
// 错误情况：本函数不执行 I/O，不返回错误；监听错误由 start 返回。
func newForwardRunner(name, listen string, token tailcat.ConnBlob, port uint16, logf func(string, ...any), clientKey key.NodePrivate, dial func(ctx context.Context) (net.Conn, error)) *forwardRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &forwardRunner{
		name:      name,
		listen:    listen,
		token:     token,
		port:      port,
		logf:      logf,
		clientKey: clientKey,
		dial:      dial,
		done:      make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
		conns:     make(map[net.Conn]struct{}),
	}
}

// specEqual 判断运行中的转发与新配置是否等价（等价则保留，避免打断活动连接）。
func (r *forwardRunner) specEqual(listen string, token tailcat.ConnBlob, port uint16) bool {
	return r.listen == listen && r.token == token && r.port == port
}

// start 开始监听并进入 accept 循环；监听失败时返回错误。
func (r *forwardRunner) start() error {
	ln, err := net.Listen("tcp", r.listen)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", r.listen, err)
	}
	r.ln = ln
	if r.dial == nil {
		cl := tailcat.NewClient(r.token)
		cl.Logf = logger.Logf(r.logf)
		if !r.clientKey.IsZero() {
			cl.Key = r.clientKey
		}
		r.client = cl
		r.dial = func(ctx context.Context) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			return cl.DialTCPPort(ctx, r.port)
		}
	}
	go r.acceptLoop()
	return nil
}

// acceptLoop 持续接受本地连接直到 stop；accept 错误（非关闭导致）记入 lastErr。
func (r *forwardRunner) acceptLoop() {
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			select {
			case <-r.done:
				return
			default:
			}
			r.setLastError(err.Error())
			return
		}
		// Accept 与 stop 可能同时发生。先登记连接，使 stop 能够及时
		// 关闭已接受但尚未进入 handle 的套接字，避免此竞态窗口泄漏连接。
		if !r.trackConnection(conn) {
			_ = conn.Close()
			return
		}
		r.active.Add(1)
		go r.handle(conn)
	}
}

// handle 处理一条本地连接：拨远端隧道端口并双向拷贝。
func (r *forwardRunner) handle(local net.Conn) {
	defer r.active.Add(-1)
	defer r.releaseConnection(local)

	// 拨号继承 runner 的生命周期，这样停止转发时不必等待拨号
	// 超时；真实 tailcat dialer 和测试 dialer 都应响应 context 取消。
	upstream, err := r.dial(r.ctx)
	if err != nil {
		select {
		case <-r.done:
			// 主动停止导致的 context 取消属于正常生命周期，不污染运行错误。
		default:
			r.setLastError(err.Error())
			r.logf("[remote] 转发 %s 拨号失败: %v", r.name, err)
		}
		return
	}
	if !r.trackConnection(upstream) {
		_ = upstream.Close()
		return
	}
	defer r.releaseConnection(upstream)
	relay(local, upstream)
}

// trackConnection 登记一条由 runner 拥有的套接字。
//
// 参数说明：
//   - conn: net.Conn，待纳入生命周期管理的本地或上游连接。
//
// 返回值说明：bool，true 表示登记成功；false 表示 runner 已停止。
//
// 错误情况：无显式错误；返回 false 时调用方必须立即关闭 conn。
func (r *forwardRunner) trackConnection(conn net.Conn) bool {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	if r.stopped {
		return false
	}
	r.conns[conn] = struct{}{}
	return true
}

// releaseConnection 从 runner 中移除并关闭一条套接字。
//
// 参数说明：
//   - conn: net.Conn，已完成转发或需要中止的连接。
//
// 返回值说明：无。
//
// 错误情况：Close 错误无法影响收尾语义，因此按尽力而为处理。
func (r *forwardRunner) releaseConnection(conn net.Conn) {
	r.connMu.Lock()
	delete(r.conns, conn)
	r.connMu.Unlock()
	_ = conn.Close()
}

// stop 幂等地停止转发器并回收所有运行时资源。
//
// 参数说明：无。
//
// 返回值说明：无；关闭操作均按尽力而为执行。
//
// 错误情况：套接字或 tailcat 客户端关闭错误不向上传递；
// 重复调用不会重复关闭 channel。
func (r *forwardRunner) stop() {
	r.stopOnce.Do(func() {
		// 先在锁内标记停止并摘取连接快照，防止 accept/dial 在
		// 关闭过程中又登记新连接。真正 Close 在锁外执行，避免外部
		// net.Conn 实现反向调用 runner 时造成死锁。
		r.connMu.Lock()
		r.stopped = true
		connections := make([]net.Conn, 0, len(r.conns))
		for conn := range r.conns {
			connections = append(connections, conn)
		}
		clear(r.conns)
		r.connMu.Unlock()

		r.cancel()
		close(r.done)
		if r.ln != nil {
			_ = r.ln.Close()
		}
		for _, conn := range connections {
			_ = conn.Close()
		}
		if r.client != nil {
			_ = r.client.Close()
		}
	})
}

// lastError 返回最近一次拨号/监听错误。
func (r *forwardRunner) lastError() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

// setLastError 记录最近一次错误。
func (r *forwardRunner) setLastError(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastErr = msg
}

// reconcileForwardsLocked 按新配置增量调和转发集合：停用/变更的停掉重建，
// 未变的保留（活动连接不断）。调用方需持有 m.mu。
func (m *Manager) reconcileForwardsLocked(cfg config.RemoteConfig) {
	want := map[string]config.RemoteForward{}
	for _, f := range cfg.Forwards {
		if f.IsEnabled() {
			want[f.Name] = f
		}
	}

	// 停掉被删除、被禁用或规格变化的转发。
	for name, r := range m.forwards {
		f, ok := want[name]
		if !ok {
			r.stop()
			delete(m.forwards, name)
			continue
		}
		listen, err := config.NormalizeRemoteListen(f.Listen)
		token, terr := ResolveToken(cfg.Remotes, f.Remote)
		// ln==nil 表示上次启动失败：规格未变也停下重建，让配置修正后能自动恢复。
		if err != nil || terr != nil || r.ln == nil || !r.specEqual(listen, tailcat.ConnBlob(token), uint16(f.RemotePort)) {
			r.stop()
			delete(m.forwards, name)
		}
	}

	// 启动新增的（或被停掉后需要重建的）转发。
	for name, f := range want {
		if _, ok := m.forwards[name]; ok {
			continue
		}
		listen, err := config.NormalizeRemoteListen(f.Listen)
		if err != nil {
			m.logf("[remote] 转发 %s 配置无效: %v", name, err)
			continue
		}
		token, err := ResolveToken(cfg.Remotes, f.Remote)
		if err != nil {
			m.logf("[remote] 转发 %s: %v", name, err)
			continue
		}
		r := newForwardRunner(name, listen, tailcat.ConnBlob(token), uint16(f.RemotePort), m.logf, m.clientKeyLocked(), nil)
		if err := r.start(); err != nil {
			r.setLastError(err.Error())
			m.logf("[remote] 转发 %s 启动失败: %v", name, err)
			// 启动失败的 runner 也放入 map，让 Status 能展示错误；下次配置变更时会重建。
			m.forwards[name] = r
			continue
		}
		m.logf("[remote] 转发 %s 已监听 %s → 远端:%d", name, listen, f.RemotePort)
		m.forwards[name] = r
	}
}
