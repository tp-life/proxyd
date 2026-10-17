package remote

// 本文件实现进程内共享的 tailcat 客户端池。
//
// tailcat/WireGuard 的 peer 按 node 公钥唯一：同一 node key 并发创建多个
// Client 会互相覆盖 endpoint 与握手状态，表现为反复握手、会话过期和路径抖动。
// Manager 的所有常驻转发、临时桌面会话和探测统一从池中借用客户端，进程内每个
// (token, 客户端身份) 只保留一个 Client。
import (
	"sync"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// clientPoolKey 是共享客户端的唯一键：远端 token + 客户端 node 公钥。
//
// NodePrivate 本身不可比较，因此用公钥文本；零值身份表示 tailcat 临时身份，
// 同一 token 下也复用同一个临时客户端，避免同 key 多实例。
type clientPoolKey struct {
	token    string
	identity string
}

// pooledClient 记录一个共享客户端及其当前借用者数量。
type pooledClient struct {
	client *tailcat.Client
	refs   int
}

// clientPool 是 Manager 私有的 tailcat 客户端池，零值不可用。
type clientPool struct {
	mu      sync.Mutex
	logf    func(format string, args ...any)
	clients map[clientPoolKey]*pooledClient
}

// newClientPool 创建空客户端池；logf 为 nil 时丢弃隧道内部日志。
func newClientPool(logf func(format string, args ...any)) *clientPool {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &clientPool{
		logf:    logf,
		clients: map[clientPoolKey]*pooledClient{},
	}
}

// acquire 借用共享客户端并返回幂等的释放函数；同一 token/身份只创建一个 Client。
//
// 参数说明：
//   - token: string，远端完整 tailcat 连接 token。
//   - private: key.NodePrivate，客户端 node 私钥；零值表示使用 tailcat 临时身份。
//
// 返回值说明：*tailcat.Client 和 func()；调用方必须调用释放函数归还引用。
//
// 错误情况：构造阶段不执行网络 I/O，不返回错误；首次拨号错误由 Client 返回。
func (p *clientPool) acquire(token string, private key.NodePrivate) (*tailcat.Client, func()) {
	poolKey := clientPoolKey{token: token}
	if !private.IsZero() {
		poolKey.identity = private.Public().String()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.clients[poolKey]; ok {
		existing.refs++
		return existing.client, p.releaseFunc(poolKey, existing)
	}
	client := tailcat.NewClient(tailcat.ConnBlob(token))
	client.Logf = logger.Logf(p.logf)
	if !private.IsZero() {
		client.Key = private
	}
	pooled := &pooledClient{client: client, refs: 1}
	p.clients[poolKey] = pooled
	return client, p.releaseFunc(poolKey, pooled)
}

// releaseFunc 返回幂等的释放闭包；最后一个引用释放时关闭并移除客户端。
//
// 参数说明：
//   - poolKey: acquire 使用的池键。
//   - target: 本次 acquire 对应的池项；用指针校验，避免 closeAll 后同一键的
//     新客户端被旧释放闭包误减引用。
//
// 返回值说明：func()，重复调用只生效一次。
//
// 错误情况：Close 错误按尽力而为处理；池项已被 closeAll 移除或替换时直接返回。
func (p *clientPool) releaseFunc(poolKey clientPoolKey, target *pooledClient) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			existing, ok := p.clients[poolKey]
			if !ok || existing != target {
				p.mu.Unlock()
				return
			}
			existing.refs--
			if existing.refs > 0 {
				p.mu.Unlock()
				return
			}
			delete(p.clients, poolKey)
			p.mu.Unlock()
			_ = existing.client.Close()
		})
	}
}

// closeAll 关闭池中全部客户端，供 Manager.Close 在停止转发后兜底回收。
//
// 参数说明：无。
//
// 返回值说明：无；客户端关闭错误被忽略。
//
// 错误情况：调用后仍在途的释放闭包找不到池项，会安静返回。
func (p *clientPool) closeAll() {
	p.mu.Lock()
	clients := make([]*tailcat.Client, 0, len(p.clients))
	for _, existing := range p.clients {
		clients = append(clients, existing.client)
	}
	clear(p.clients)
	p.mu.Unlock()
	for _, client := range clients {
		_ = client.Close()
	}
}

// size 返回当前池中客户端数量，供测试断言共享行为。
func (p *clientPool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.clients)
}
