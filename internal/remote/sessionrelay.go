package remote

// 会话中转：守护进程为 Web 终端会话签发一次性凭据，会话内的 `proxyd ssh <远端名>`
// 经管理 API 请求守护进程代拨已保存的远端。
//
// 背景：root 运行时 Web 终端降权为 shell-user，既读不到 root 配置里保存的远端 token，
// 也拿不到守护进程的客户端身份（对端白名单登记的公钥）。中转让 token 与私钥始终留在
// 守护进程内：凭据只允许按名称拨号已保存远端，连接复用 Manager.clients 共享客户端，
// 会话结束即撤销并断开经它建立的全部连接。仅 Web 终端（已过管理面认证）获得凭据，
// 隧道入站的内嵌 SSH 会话不获得，避免远端用户借本机身份横向跳转。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"proxyd/internal/config"
)

const (
	// SessionEnvSecret 是注入 Web 终端会话的中转凭据环境变量名。
	SessionEnvSecret = "PROXYD_SESSION"
	// SessionEnvAPI 是注入 Web 终端会话的管理 API 地址环境变量名（host:port）。
	SessionEnvAPI = "PROXYD_API"
	// SessionPipePath 是会话中转的 HTTP 升级端点路径。
	SessionPipePath = "/api/remote/session/pipe"
	// SessionHeader 是携带中转凭据的请求头。
	SessionHeader = "X-Proxyd-Session"
	// SessionUpgrade 是中转端点使用的 HTTP Upgrade 协议名。
	SessionUpgrade = "proxyd-pipe"
)

// ErrSessionGrantInvalid 表示中转凭据缺失、伪造或所属会话已结束。
var ErrSessionGrantInvalid = errors.New("会话中转凭据无效或会话已结束")

// sessionGrant 是一个 Web 终端会话的中转授权，记录经它建立的连接以便撤销时断开。
type sessionGrant struct {
	conns map[net.Conn]struct{}
}

// sessionRelay 保存中转地址与有效授权；授权按凭据 SHA-256 索引，内存里不留明文。
type sessionRelay struct {
	mu     sync.Mutex
	addr   string
	grants map[[sha256.Size]byte]*sessionGrant
}

// SetSessionRelayAddr 设置会话中转使用的管理 API 地址；空串表示不签发中转凭据。
//
// 参数说明：
//   - addr: string，管理 API 实际监听地址；通配地址（0.0.0.0/::）改写为回环地址，
//     因为会话总在本机拨号。
//
// 返回值说明：无。
//
// 错误情况：无；地址格式非法时视为不可用并清空。
func (m *Manager) SetSessionRelayAddr(addr string) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		addr = ""
	} else {
		if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
			host = "127.0.0.1"
		}
		addr = net.JoinHostPort(host, port)
	}
	m.relay.mu.Lock()
	m.relay.addr = addr
	m.relay.mu.Unlock()
}

// issueSessionGrant 为一个 Web 终端会话签发中转凭据。
//
// 返回值说明：env 为应注入会话的环境变量（未配置中转地址时为 nil）；revoke 幂等撤销
// 授权并关闭经它建立的全部中转连接，调用方必须在会话结束时调用。
//
// 错误情况：随机数生成失败时返回错误。
func (m *Manager) issueSessionGrant() (env []string, revoke func(), err error) {
	m.relay.mu.Lock()
	addr := m.relay.addr
	m.relay.mu.Unlock()
	if addr == "" {
		return nil, func() {}, nil
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, nil, fmt.Errorf("生成会话中转凭据失败: %w", err)
	}
	secret := hex.EncodeToString(raw[:])
	id := sha256.Sum256([]byte(secret))
	m.relay.mu.Lock()
	if m.relay.grants == nil {
		m.relay.grants = map[[sha256.Size]byte]*sessionGrant{}
	}
	m.relay.grants[id] = &sessionGrant{conns: map[net.Conn]struct{}{}}
	m.relay.mu.Unlock()

	var once sync.Once
	revoke = func() {
		once.Do(func() {
			m.relay.mu.Lock()
			grant := m.relay.grants[id]
			delete(m.relay.grants, id)
			m.relay.mu.Unlock()
			if grant != nil {
				for conn := range grant.conns {
					_ = conn.Close()
				}
			}
		})
	}
	return []string{SessionEnvSecret + "=" + secret, SessionEnvAPI + "=" + addr}, revoke, nil
}

// relayConn 是经会话中转建立的连接；关闭时从所属授权中摘除。
type relayConn struct {
	net.Conn
	once   sync.Once
	detach func()
}

// Close 关闭底层连接并幂等摘除授权登记。
func (c *relayConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.detach)
	return err
}

// DialSessionRemote 凭会话中转凭据，以守护进程身份拨号一个已保存远端的 TCP 端口。
//
// 参数说明：
//   - ctx: context.Context，控制建连；无 deadline 时套用 dialTimeout。
//   - secret: string，会话环境变量中的中转凭据。
//   - name: string，remote.remotes 中保存的远端名称；刻意不接受裸 token，凭据只授予
//     「使用已保存远端」这一项能力。
//   - port: int，远端 TCP 端口。
//
// 返回值说明：net.Conn，调用方必须 Close；会话撤销时会被强制关闭。
//
// 错误情况：凭据无效返回 ErrSessionGrantInvalid；名称未保存、模块禁用或拨号失败返回对应错误。
func (m *Manager) DialSessionRemote(ctx context.Context, secret, name string, port int) (net.Conn, error) {
	id := sha256.Sum256([]byte(secret))
	m.relay.mu.Lock()
	_, ok := m.relay.grants[id]
	m.relay.mu.Unlock()
	if secret == "" || !ok {
		return nil, ErrSessionGrantInvalid
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.HasPrefix(name, "tc") {
		return nil, fmt.Errorf("会话中转只接受已保存的远端名称")
	}
	m.mu.Lock()
	remotes := append([]config.RemotePeer(nil), m.cfg.Remotes...)
	m.mu.Unlock()
	token, err := ResolveToken(remotes, name)
	if err != nil {
		return nil, err
	}
	conn, err := m.dialShared(ctx, token, port)
	if err != nil {
		return nil, err
	}
	// 拨号期间会话可能已结束：重新确认授权仍在，再登记连接。
	m.relay.mu.Lock()
	grant, ok := m.relay.grants[id]
	if !ok {
		m.relay.mu.Unlock()
		_ = conn.Close()
		return nil, ErrSessionGrantInvalid
	}
	wrapped := &relayConn{Conn: conn}
	wrapped.detach = func() {
		m.relay.mu.Lock()
		delete(grant.conns, wrapped)
		m.relay.mu.Unlock()
	}
	grant.conns[wrapped] = struct{}{}
	m.relay.mu.Unlock()
	return wrapped, nil
}
