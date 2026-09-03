package remote

// 本文件实现 remote 专用连接审计环与连接字节计量。
// 它不复用全局 logbuf，避免高频代理日志挤掉安全审计记录。

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// remoteAuditCapacity 是内存中保留的最大连接事件数。
	remoteAuditCapacity = 500
	// AuditActionConnected 表示一个连接已经通过身份与端口授权。
	AuditActionConnected = "connected"
	// AuditActionRejected 表示连接被 TTL、端口或身份规则拒绝。
	AuditActionRejected = "rejected"
	// AuditActionDisconnected 表示一个已建立连接已经结束。
	AuditActionDisconnected = "disconnected"
)

// AuditEntry 描述一条可追溯的远程连接安全事件。
type AuditEntry struct {
	Time       time.Time `json:"time"`
	ClientKey  string    `json:"client_key,omitempty"`
	ClientName string    `json:"client_name,omitempty"`
	TargetPort int       `json:"target_port"`
	Action     string    `json:"action"`
	Reason     string    `json:"reason,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	RxBytes    int64     `json:"rx_bytes,omitempty"`
	TxBytes    int64     `json:"tx_bytes,omitempty"`
}

// auditLog 是固定容量、并发安全的 FIFO 环形缓冲。
type auditLog struct {
	mu       sync.RWMutex
	entries  []AuditEntry
	next     int
	full     bool
	capacity int
}

// newAuditLog 创建指定容量的空审计环。
//
// 参数说明：
//   - capacity: int，最多保留的事件数。
//
// 返回值说明：*auditLog，可安全供多个连接协程并发写入。
//
// 错误情况：无；非正容量会提升为 1，避免取模除零。
func newAuditLog(capacity int) *auditLog {
	capacity = max(1, capacity)
	return &auditLog{entries: make([]AuditEntry, capacity), capacity: capacity}
}

// Append 把事件追加到环形缓冲，容量满时覆盖最旧记录。
//
// 参数说明：
//   - entry: AuditEntry，调用方已填充的连接事件。
//
// 返回值说明：无。
//
// 错误情况：无；零值时间会在此补成当前 UTC 时间，保证每条记录可排序。
func (l *auditLog) Append(entry AuditEntry) {
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[l.next] = entry
	l.next = (l.next + 1) % l.capacity
	if l.next == 0 {
		l.full = true
	}
}

// Tail 返回按时间从旧到新排列的最近事件副本。
//
// 参数说明：
//   - limit: int，请求的最大条数；大于容量会自动夹紧。
//
// 返回值说明：[]AuditEntry，与内部数组不共享可变存储；无事件时返回空切片。
//
// 错误情况：无；limit 小于等于零时返回空切片。
func (l *auditLog) Tail(limit int) []AuditEntry {
	if limit <= 0 {
		return []AuditEntry{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	count := l.next
	start := 0
	if l.full {
		count = l.capacity
		start = l.next
	}
	if limit < count {
		start = (start + count - limit) % l.capacity
		count = limit
	}
	result := make([]AuditEntry, 0, count)
	for offset := 0; offset < count; offset++ {
		result = append(result, l.entries[(start+offset)%l.capacity])
	}
	return result
}

// Audit 返回 remote 专用审计环中的最近记录。
//
// 参数说明：
//   - tail: int，最多返回条数，API 层负责进一步限制用户输入。
//
// 返回值说明：[]AuditEntry，按发生时间从旧到新排列的安全副本。
//
// 错误情况：无；Manager 始终由 NewManager 初始化审计环，防御性处理 nil 仍返回空集合。
func (m *Manager) Audit(tail int) []AuditEntry {
	if m.audit == nil {
		return []AuditEntry{}
	}
	return m.audit.Tail(tail)
}

// meteredConn 在不改变 net.Conn 行为的前提下统计隧道方向的累计字节。
type meteredConn struct {
	net.Conn
	rx atomic.Int64
	tx atomic.Int64
}

// Read 代理底层读取并累计客户端发往服务端的字节数。
//
// 参数说明：
//   - buffer: []byte，接收底层连接数据的缓冲区。
//
// 返回值说明：int 与 error，完全保持底层 net.Conn.Read 语义。
//
// 错误情况：底层超时、关闭或网络错误原样返回；已成功读取的字节仍会计入审计。
func (c *meteredConn) Read(buffer []byte) (int, error) {
	read, err := c.Conn.Read(buffer)
	c.rx.Add(int64(read))
	return read, err
}

// Write 代理底层写入并累计服务端发往客户端的字节数。
//
// 参数说明：
//   - buffer: []byte，待写入底层连接的数据。
//
// 返回值说明：int 与 error，完全保持底层 net.Conn.Write 语义。
//
// 错误情况：底层超时、关闭或网络错误原样返回；已成功写入的字节仍会计入审计。
func (c *meteredConn) Write(buffer []byte) (int, error) {
	written, err := c.Conn.Write(buffer)
	c.tx.Add(int64(written))
	return written, err
}
