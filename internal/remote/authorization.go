package remote

// 本文件集中实现远程入站连接的身份归因、TTL/端口授权与审计包装。
// 授权在每条 TCP 连接建立时重新读取配置，因此过期边界无需等待一分钟清扫即可立即生效。

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"

	"proxyd/internal/config"
)

// isClientAllowed 判定指定客户端公钥当前是否获准建立隧道会话。
//
// 参数说明：
//   - cfg: config.RemoteConfig，当前配置的独立快照。
//   - keyText: string，客户端 nodekey:... 公钥文本。
//   - now: time.Time，判定 TTL 过期边界的统一时钟。
//
// 返回值说明：bool，允许接入返回 true；模块禁用、受限空列表、未登记或已过期均返回 false。
//
// 错误情况：无；供 tailcat v0.8.0 Server.AllowClient 动态钩子与撤销清扫共用。
func isClientAllowed(cfg config.RemoteConfig, keyText string, now time.Time) bool {
	if cfg.Disabled {
		return false
	}
	if !cfg.ClientWhitelistEnabled() {
		return true
	}
	keyText = strings.TrimSpace(keyText)
	if keyText == "" {
		return false
	}
	if keyText == strings.TrimSpace(cfg.TempKey) {
		return true
	}
	for _, entry := range cfg.Allow {
		if strings.TrimSpace(entry.Key) != keyText {
			continue
		}
		return !entry.IsExpired(now)
	}
	return false
}

// allowClient 是交给 tailcat v0.8.0 Server.AllowClient 的动态接入钩子。
// 每次新客户端握手时按最新配置实时判定，支持白名单、临时身份与 TTL 边界热生效，
// 不再需要为了变更白名单而重建整个隧道服务端。
func (m *Manager) allowClient(pub key.NodePublic) bool {
	m.mu.Lock()
	cfg := m.cfg.Clone()
	m.mu.Unlock()
	return isClientAllowed(cfg, pub.String(), time.Now())
}

// clientDisplayName 按配置查找客户端公钥对应的可读别名。
func clientDisplayName(cfg config.RemoteConfig, keyText string) string {
	keyText = strings.TrimSpace(keyText)
	if keyText == "" {
		return ""
	}
	if keyText == strings.TrimSpace(cfg.TempKey) {
		return "临时身份"
	}
	for _, entry := range cfg.Allow {
		if strings.TrimSpace(entry.Key) == keyText {
			return strings.TrimSpace(entry.Name)
		}
	}
	return ""
}

// resolvePeerKey 优先通过 tailcat v0.8.0 Server.PeerKey 获取已认证的客户端公钥；
// 仅在无运行中 Server 的离线单元测试场景回退到本地地址反查。
func resolvePeerKey(srv *tailcat.Server, cfg config.RemoteConfig, remoteAddr net.Addr) string {
	if srv != nil {
		if pub, ok := srv.PeerKey(remoteAddr); ok && !pub.IsZero() {
			return pub.String()
		}
	}
	return clientKeyForAddr(cfg, remoteAddr)
}

// connectionDecision 是一次入站授权判定的内部值对象。
type connectionDecision struct {
	Allowed bool
	Key     string
	Name    string
	Reason  string
}

// decideConnection 根据配置、隧道源地址和目标端口执行完整授权判定。
//
// 参数说明：
//   - cfg: config.RemoteConfig，当前配置的独立快照。
//   - remoteAddr: net.Addr，tailcat netstack 为入站连接提供的远端隧道地址。
//   - targetPort: int，Server.OnTCP 已确认的目标端口。
//   - now: time.Time，执行 TTL 边界判断的统一时钟。
//
// 返回值说明：connectionDecision，包含是否允许、已归因身份和可审计拒绝原因。
//
// 错误情况：无；无法解析地址或归因身份时在白名单模式下保守拒绝，开放模式下继续放行。
func decideConnection(cfg config.RemoteConfig, remoteAddr net.Addr, targetPort int, now time.Time) connectionDecision {
	return decideConnectionForKey(cfg, clientKeyForAddr(cfg, remoteAddr), targetPort, now)
}

// decideConnectionForKey 根据已归因客户端公钥、配置和目标端口执行完整授权判定。
func decideConnectionForKey(cfg config.RemoteConfig, keyText string, targetPort int, now time.Time) connectionDecision {
	keyText = strings.TrimSpace(keyText)
	decision := connectionDecision{Allowed: true, Key: keyText, Name: clientDisplayName(cfg, keyText)}
	// 模块停用优先于白名单规则，防止排队的连接在停用后继续进入 shell。
	if cfg.Disabled {
		decision.Allowed = false
		decision.Reason = "远程访问模块已禁用"
		return decision
	}
	if !cfg.ClientWhitelistEnabled() {
		return decision
	}
	if keyText == "" {
		decision.Allowed = false
		decision.Reason = "白名单模式下无法识别客户端身份"
		return decision
	}
	if keyText == strings.TrimSpace(cfg.TempKey) {
		decision.Name = "临时身份"
		return decision
	}
	for _, entry := range cfg.Allow {
		if strings.TrimSpace(entry.Key) != keyText {
			continue
		}
		decision.Name = strings.TrimSpace(entry.Name)
		if entry.IsExpired(now) {
			decision.Allowed = false
			decision.Reason = "客户端授权已过期"
			return decision
		}
		if !entry.AllowsPort(targetPort) {
			decision.Allowed = false
			decision.Reason = fmt.Sprintf("客户端未获授权访问端口 %d", targetPort)
			return decision
		}
		return decision
	}
	decision.Allowed = false
	decision.Reason = "客户端不在当前白名单中"
	return decision
}

// clientKeyForAddr 把 tailcat 隧道源地址映射回已配置的客户端公钥。
//
// 参数说明：
//   - cfg: config.RemoteConfig，包含白名单和临时身份。
//   - remoteAddr: net.Addr，连接远端地址。
//
// 返回值说明：string，匹配到的 nodekey；开放模式陌生客户端或非法地址返回空串。
//
// 错误情况：无；地址转换失败时不会猜测身份。
func clientKeyForAddr(cfg config.RemoteConfig, remoteAddr net.Addr) string {
	tcpAddr, ok := remoteAddr.(*net.TCPAddr)
	if !ok {
		return ""
	}
	address, ok := netip.AddrFromSlice(tcpAddr.IP)
	if !ok {
		return ""
	}
	return knownClientAddrs(allowKeys(cfg.Allow), cfg.TempKey)[address.Unmap()]
}

// guardConnection 为真实连接处理器添加授权、活动计数、流量计量与审计生命周期。
//
// 参数说明：
//   - port: uint16，连接要访问的服务端目标端口。
//   - handler: func(net.Conn)，builtin-ssh 或本机端口转发处理器。
//
// 返回值说明：func(net.Conn)，可直接交给 tailcat Server.OnTCP。
//
// 错误情况：拒绝连接会立即关闭并写审计，不调用下游；下游网络错误由原处理器自行记录。
func (m *Manager) guardConnection(port uint16, handler func(net.Conn)) func(net.Conn) {
	return func(connection net.Conn) {
		m.mu.Lock()
		cfg := m.cfg.Clone()
		runtimeCtx := m.runtimeCtx
		srv := m.srv
		m.mu.Unlock()
		// 上游关闭隧道不保证应用处理器立即结束；世代取消显式关闭流以释放 SSH/转发进程。
		stopCancellation := context.AfterFunc(runtimeCtx, func() { _ = connection.Close() })
		defer stopCancellation()
		startedAt := time.Now().UTC()
		peerKey := resolvePeerKey(srv, cfg, connection.RemoteAddr())
		decision := decideConnectionForKey(cfg, peerKey, int(port), startedAt)
		if !decision.Allowed {
			m.audit.Append(AuditEntry{
				Time:       startedAt,
				ClientKey:  decision.Key,
				ClientName: decision.Name,
				TargetPort: int(port),
				Action:     AuditActionRejected,
				Reason:     decision.Reason,
			})
			m.logf("[remote] 拒绝客户端 %s 访问端口 %d: %s", displayClient(decision), port, decision.Reason)
			_ = connection.Close()
			return
		}

		if decision.Key != "" {
			m.mu.Lock()
			m.activeClients[decision.Key]++
			if m.clientConns == nil {
				m.clientConns = map[string]map[net.Conn]struct{}{}
			}
			if m.clientConns[decision.Key] == nil {
				m.clientConns[decision.Key] = map[net.Conn]struct{}{}
			}
			m.clientConns[decision.Key][connection] = struct{}{}
			m.mu.Unlock()
			defer func() {
				m.mu.Lock()
				m.activeClients[decision.Key]--
				if conns := m.clientConns[decision.Key]; conns != nil {
					delete(conns, connection)
					if len(conns) == 0 {
						delete(m.clientConns, decision.Key)
					}
				}
				m.mu.Unlock()
			}()
		}
		metered := &meteredConn{Conn: connection}
		m.audit.Append(AuditEntry{
			Time:       startedAt,
			ClientKey:  decision.Key,
			ClientName: decision.Name,
			TargetPort: int(port),
			Action:     AuditActionConnected,
		})
		defer func() {
			m.audit.Append(AuditEntry{
				Time:       time.Now().UTC(),
				ClientKey:  decision.Key,
				ClientName: decision.Name,
				TargetPort: int(port),
				Action:     AuditActionDisconnected,
				DurationMS: time.Since(startedAt).Milliseconds(),
				RxBytes:    metered.rx.Load(),
				TxBytes:    metered.tx.Load(),
			})
		}()
		handler(metered)
	}
}

// guardUDPFlow 为入站 UDP 流（如 RDP 3389 的 MS-RDPEUDP）提供与 TCP 一致的授权、
// 运行世代取消与主动断开追踪。
func (m *Manager) guardUDPFlow(port uint16, handler func(tailcat.ConnPacketConn)) func(tailcat.ConnPacketConn) {
	return func(flow tailcat.ConnPacketConn) {
		m.mu.Lock()
		cfg := m.cfg.Clone()
		runtimeCtx := m.runtimeCtx
		srv := m.srv
		m.mu.Unlock()
		stopCancellation := context.AfterFunc(runtimeCtx, func() { _ = flow.Close() })
		defer stopCancellation()
		startedAt := time.Now().UTC()
		peerKey := resolvePeerKey(srv, cfg, flow.RemoteAddr())
		decision := decideConnectionForKey(cfg, peerKey, int(port), startedAt)
		if !decision.Allowed {
			_ = flow.Close()
			return
		}
		if decision.Key != "" {
			m.mu.Lock()
			if m.clientConns == nil {
				m.clientConns = map[string]map[net.Conn]struct{}{}
			}
			if m.clientConns[decision.Key] == nil {
				m.clientConns[decision.Key] = map[net.Conn]struct{}{}
			}
			m.clientConns[decision.Key][flow] = struct{}{}
			m.mu.Unlock()
			defer func() {
				m.mu.Lock()
				if conns := m.clientConns[decision.Key]; conns != nil {
					delete(conns, flow)
					if len(conns) == 0 {
						delete(m.clientConns, decision.Key)
					}
				}
				m.mu.Unlock()
			}()
		}
		handler(flow)
	}
}

// DisconnectClient 主动断开指定隧道客户端：先从 tailcat 服务端移除对端会话，
// 同时关闭其在本机已建立的活动 TCP 连接，避免连接停滞等待超时。
//
// 参数说明：
//   - clientKey: string，客户端 nodekey:... 公钥。
//
// 返回值说明：
//   - bool：tailcat 服务端是否存在并移除了该对端。
//   - int：同步关闭的活动 TCP 连接数量。
//   - error：公钥格式非法时返回错误。
//
// 错误情况：仅在公钥格式无效时返回错误；此操作不修改白名单配置，若需禁止重连请先移除授权。
func (m *Manager) DisconnectClient(clientKey string) (bool, int, error) {
	pub, err := ValidateClientKey(clientKey)
	if err != nil {
		return false, 0, err
	}
	keyText := pub.String()

	m.mu.Lock()
	srv := m.srv
	name := clientDisplayName(m.cfg, keyText)
	var conns []net.Conn
	for conn := range m.clientConns[keyText] {
		conns = append(conns, conn)
	}
	m.mu.Unlock()

	peerDisconnected := false
	if srv != nil {
		peerDisconnected = srv.DisconnectClient(pub)
	}
	for _, conn := range conns {
		_ = conn.Close()
	}
	m.audit.Append(AuditEntry{
		ClientKey:  keyText,
		ClientName: name,
		Action:     AuditActionClientDisconnect,
		Reason:     fmt.Sprintf("管理员请求断开隧道客户端（关闭 %d 条活动连接）", len(conns)),
	})
	m.logf("[remote] 已请求断开隧道客户端 %s（服务端移除=%v，关闭活动连接=%d）", displayClient(connectionDecision{Key: keyText, Name: name}), peerDisconnected, len(conns))
	return peerDisconnected, len(conns), nil
}

// disconnectRevokedClients 在热更新白名单/临时身份后，断开已不再被允许的在线客户端及其活动连接。
// 本函数必须在释放 m.mu 之后调用，避免与 tailcat Server 内部锁交叉。
func (m *Manager) disconnectRevokedClients(srv *tailcat.Server, cfg config.RemoteConfig) {
	now := time.Now()
	var revokedPeers []key.NodePublic
	if srv != nil {
		if status := srv.Status(); status != nil {
			for pub := range status.Peer {
				if !isClientAllowed(cfg, pub.String(), now) {
					revokedPeers = append(revokedPeers, pub)
				}
			}
		}
	}

	m.mu.Lock()
	var revokedConns []net.Conn
	for keyText, conns := range m.clientConns {
		if !isClientAllowed(cfg, keyText, now) {
			for conn := range conns {
				revokedConns = append(revokedConns, conn)
			}
		}
	}
	m.mu.Unlock()

	for _, pub := range revokedPeers {
		srv.DisconnectClient(pub)
	}
	for _, conn := range revokedConns {
		_ = conn.Close()
	}
}

// displayClient 为运行日志生成不泄露额外信息的客户端标识。
//
// 参数说明：
//   - decision: connectionDecision，当前授权判定结果。
//
// 返回值说明：string，优先别名，其次公钥，均缺失时返回“未知客户端”。
//
// 错误情况：无。
func displayClient(decision connectionDecision) string {
	if decision.Name != "" {
		return decision.Name
	}
	if decision.Key != "" {
		return decision.Key
	}
	return "未知客户端"
}
