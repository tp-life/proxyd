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

	"proxyd/internal/config"
)

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
	keyText := clientKeyForAddr(cfg, remoteAddr)
	decision := connectionDecision{Allowed: true, Key: keyText}
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
		m.mu.Unlock()
		// 上游关闭隧道不保证应用处理器立即结束；世代取消显式关闭流以释放 SSH/转发进程。
		stopCancellation := context.AfterFunc(runtimeCtx, func() { _ = connection.Close() })
		defer stopCancellation()
		startedAt := time.Now().UTC()
		decision := decideConnection(cfg, connection.RemoteAddr(), int(port), startedAt)
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
			m.mu.Unlock()
			defer func() {
				m.mu.Lock()
				m.activeClients[decision.Key]--
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
