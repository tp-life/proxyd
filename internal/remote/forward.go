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

	mu      sync.Mutex
	lastErr string
}

// newForwardRunner 构造转发runner；dial 为 nil 表示使用真实隧道拨号。
func newForwardRunner(name, listen string, token tailcat.ConnBlob, port uint16, logf func(string, ...any), clientKey key.NodePrivate, dial func(ctx context.Context) (net.Conn, error)) *forwardRunner {
	return &forwardRunner{
		name:      name,
		listen:    listen,
		token:     token,
		port:      port,
		logf:      logf,
		clientKey: clientKey,
		dial:      dial,
		done:      make(chan struct{}),
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
		go r.handle(conn)
	}
}

// handle 处理一条本地连接：拨远端隧道端口并双向拷贝。
func (r *forwardRunner) handle(local net.Conn) {
	r.active.Add(1)
	defer r.active.Add(-1)
	defer local.Close()

	upstream, err := r.dial(context.Background())
	if err != nil {
		r.setLastError(err.Error())
		r.logf("[remote] 转发 %s 拨号失败: %v", r.name, err)
		return
	}
	defer upstream.Close()
	relay(local, upstream)
}

// stop 关闭监听器与隧道客户端；活动连接随监听器关闭后由各自连接关闭收尾。
func (r *forwardRunner) stop() {
	close(r.done)
	if r.ln != nil {
		_ = r.ln.Close()
	}
	if r.client != nil {
		_ = r.client.Close()
	}
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
