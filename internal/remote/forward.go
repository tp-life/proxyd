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
	ctx    context.Context
	cancel context.CancelFunc
	active atomic.Int64

	stopOnce sync.Once
	connMu   sync.Mutex
	conns    map[net.Conn]struct{}
	stopped  bool

	mu      sync.Mutex
	lastErr string
}

// newForwardRunner 构造一条尚未监听、具有独立生命周期的本地转发。
//
// 参数说明：
//   - name: string，配置中的转发名称，用于状态与日志定位。
//   - listen: string，本地 TCP 监听地址。
//   - token: tailcat.ConnBlob，远端身份与 DERP 信息。
//   - port: uint16，远端目标端口。
//   - logf: func(string, ...any)，日志回调；nil 时使用静默回调。
//   - clientKey: key.NodePrivate，本机客户端身份；零值使用临时身份。
//   - dial: func(context.Context) (net.Conn, error)，测试注入拨号器；nil 使用 tailcat。
//
// 返回值说明：*forwardRunner，调用方还需调用 start。
//
// 错误情况：构造阶段不执行 I/O，不返回错误；监听或拨号错误由 start/handle 处理。
func newForwardRunner(name, listen string, token tailcat.ConnBlob, port uint16, logf func(string, ...any), clientKey key.NodePrivate, dial func(ctx context.Context) (net.Conn, error)) *forwardRunner {
	ctx, cancel := context.WithCancel(context.Background())
	if logf == nil {
		logf = func(string, ...any) {}
	}
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
		r.cancel()
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
		// Accept 与 stop 可能同时完成。只有成功登记的连接才允许启动
		// handler；停止标记已设置时立即关闭，避免 stop 快照之后漏入连接。
		if !r.trackConnection(conn) {
			_ = conn.Close()
			return
		}
		r.active.Add(1)
		go r.handle(conn)
	}
}

// handle 处理一条已登记的本地连接：拨远端隧道端口并双向拷贝。
//
// 参数说明：
//   - local: net.Conn，acceptLoop 已加入活动连接集合的本地连接。
//
// 返回值说明：无；连接结束后减少 active 并移除两端连接。
//
// 错误情况：拨号错误在正常运行期间写入状态；stop 引发的 context.Canceled
// 是预期生命周期事件，不污染 lastErr，也不输出误导日志。
func (r *forwardRunner) handle(local net.Conn) {
	defer r.active.Add(-1)
	defer r.releaseConnection(local)

	// 拨号继承 runner 的生命周期，这样停止转发时不必等待拨号
	// 超时；真实 tailcat dialer 和测试 dialer 都应响应 context 取消。
	upstream, err := r.dial(r.ctx)
	if err != nil {
		if r.ctx.Err() == nil {
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
