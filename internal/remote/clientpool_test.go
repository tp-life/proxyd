package remote

// 本文件验证共享客户端池按 token/身份复用，并在最后一个引用释放时关闭。

import (
	"testing"

	"tailscale.com/types/key"
)

// TestClientPoolSharesByTokenAndIdentity 验证同 token/同身份只创建一个客户端。
//
// 参数说明：t 为 *testing.T。
//
// 返回值说明：无。
//
// 错误情况：同一键返回不同客户端、引用释放后未回收，或不同键被错误合并时失败。
func TestClientPoolSharesByTokenAndIdentity(t *testing.T) {
	pool := newClientPool(nil)
	identity := key.NewNode()

	first, releaseFirst := pool.acquire("tcunused", identity)
	second, releaseSecond := pool.acquire("tcunused", identity)
	if first != second {
		t.Fatal("same token/identity should share one tailcat client")
	}
	if got := pool.size(); got != 1 {
		t.Fatalf("pool size = %d, want 1", got)
	}

	releaseFirst()
	releaseFirst() // 幂等：重复释放不能把引用减到负数或提前关闭
	if got := pool.size(); got != 1 {
		t.Fatalf("pool size after first release = %d, want 1", got)
	}
	releaseSecond()
	if got := pool.size(); got != 0 {
		t.Fatalf("pool size after final release = %d, want 0", got)
	}

	// 不同身份和不同 token 必须是独立客户端。
	otherIdentity, releaseOtherIdentity := pool.acquire("tcunused", key.NewNode())
	otherToken, releaseOtherToken := pool.acquire("tcother", identity)
	if otherIdentity == first || otherToken == first || otherIdentity == otherToken {
		t.Fatal("different token/identity must not share a client")
	}
	if got := pool.size(); got != 2 {
		t.Fatalf("pool size with two distinct clients = %d, want 2", got)
	}
	releaseOtherIdentity()
	releaseOtherToken()
	if got := pool.size(); got != 0 {
		t.Fatalf("pool size after cleanup = %d, want 0", got)
	}
}

// TestClientPoolCloseAll 验证 closeAll 清空池，且随后释放闭包不再影响新池项。
//
// 参数说明：t 为 *testing.T。
//
// 返回值说明：无。
//
// 错误情况：closeAll 后池未清空或旧释放闭包误删新客户端时失败。
func TestClientPoolCloseAll(t *testing.T) {
	pool := newClientPool(nil)
	identity := key.NewNode()
	_, staleRelease := pool.acquire("tcunused", identity)
	pool.closeAll()
	if got := pool.size(); got != 0 {
		t.Fatalf("pool size after closeAll = %d, want 0", got)
	}

	// closeAll 后同一键重新借用：旧释放闭包必须失效，不能误减新池项的引用。
	client, releaseNew := pool.acquire("tcunused", identity)
	if client == nil {
		t.Fatal("acquire after closeAll returned nil client")
	}
	staleRelease()
	if got := pool.size(); got != 1 {
		t.Fatalf("stale release removed the new client: size = %d, want 1", got)
	}
	releaseNew()
	if got := pool.size(); got != 0 {
		t.Fatalf("pool size after releasing new client = %d, want 0", got)
	}
}
