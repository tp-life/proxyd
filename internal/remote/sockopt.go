package remote

// 本文件集中调整本地 TCP 转发连接的套接字选项。
//
// tailcat 拨出的连接来自 gVisor netstack，不实现 *net.TCPConn 的 SetNoDelay/
// SetKeepAlive；这里只对本地真实 TCP 端点生效，两类连接可以安全混用。

import (
	"net"
	"sync"
	"time"
)

const (
	// localKeepAlivePeriod 是本地 TCP 转发连接的空闲 keepalive 探测周期。
	localKeepAlivePeriod = 30 * time.Second
	// relayCopyBufferSize 是双向转发每个方向的拷贝缓冲区大小。
	relayCopyBufferSize = 64 << 10
)

// tuneLocalConn 为本地 TCP 连接启用低延迟与 keepalive。
//
// 参数说明：conn 为本地监听 accept 或回环拨号得到的连接；非 *net.TCPConn 时安静返回。
//
// 返回值说明：无。
//
// 错误情况：套接字选项不支持时忽略错误；这些选项只优化交互延迟和死连接回收，
// 不影响转发正确性。
func tuneLocalConn(conn net.Conn) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	// VNC/SSH 这类交互流量对 Nagle 很敏感；本地两段 TCP 都关掉。
	_ = tcp.SetNoDelay(true)
	_ = tcp.SetKeepAlive(true)
	_ = tcp.SetKeepAlivePeriod(localKeepAlivePeriod)
}

// relayCopyBufferPool 复用双向转发的拷贝缓冲区，避免每条连接重复分配 64 KiB。
var relayCopyBufferPool = sync.Pool{
	New: func() any { return make([]byte, relayCopyBufferSize) },
}

// getRelayCopyBuffer 从池中取出一个 64 KiB 拷贝缓冲区。
//
// 参数说明：无。
//
// 返回值说明：[]byte，长度 relayCopyBufferSize；使用后必须归还。
//
// 错误情况：无。
func getRelayCopyBuffer() []byte {
	return relayCopyBufferPool.Get().([]byte)
}

// putRelayCopyBuffer 归还拷贝缓冲区。
//
// 参数说明：buffer 为 getRelayCopyBuffer 返回的切片。
//
// 返回值说明：无。
//
// 错误情况：无；池只保存固定长度切片。
func putRelayCopyBuffer(buffer []byte) {
	relayCopyBufferPool.Put(buffer)
}
