package remote

// 本文件验证 Manager 的多条转发与临时桌面转发共享同一个 tailcat 客户端，
// 避免同一 node key 的多个 Client 互相覆盖握手与 endpoint。

import (
	"testing"

	"proxyd/internal/config"
)

// TestManagerForwardsShareClient 验证同一 token 的多条常驻转发只创建一个客户端。
//
// 参数说明：t 为 *testing.T。
//
// 返回值说明：无。
//
// 错误情况：两条转发各建客户端、删除转发后引用未回收时失败。
func TestManagerForwardsShareClient(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	defer m.Close()

	token := newTestToken(t)
	forwards := []config.RemoteForward{
		{Name: "vnc", Listen: freeTCPAddr(t), Remote: token, RemotePort: 5900},
		{Name: "web", Listen: freeTCPAddr(t), Remote: token, RemotePort: 4321},
	}
	applyForwardsOnly(t, m, forwards)

	m.mu.Lock()
	first, firstOK := m.forwards["vnc"]
	second, secondOK := m.forwards["web"]
	m.mu.Unlock()
	if !firstOK || !secondOK {
		t.Fatalf("forwards not running: vnc=%v web=%v", firstOK, secondOK)
	}
	if first.client != nil || second.client != nil {
		t.Fatal("shared runners must not own a private tailcat client")
	}
	if got := m.clients.size(); got != 1 {
		t.Fatalf("shared client count = %d, want 1", got)
	}

	// 删除两条转发后引用归零，共享客户端应被回收。
	applyForwardsOnly(t, m, nil)
	if got := m.clients.size(); got != 0 {
		t.Fatalf("shared client count after removing forwards = %d, want 0", got)
	}
}

// TestManagerTransientForwardSharesClient 验证临时桌面转发复用常驻转发的客户端。
//
// 参数说明：t 为 *testing.T。
//
// 返回值说明：无。
//
// 错误情况：临时转发另建客户端，或关闭后未归还引用时失败。
func TestManagerTransientForwardSharesClient(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	defer m.Close()

	token := newTestToken(t)
	applyForwardsOnly(t, m, []config.RemoteForward{
		{Name: "vnc", Listen: freeTCPAddr(t), Remote: token, RemotePort: 5900},
	})
	if got := m.clients.size(); got != 1 {
		t.Fatalf("shared client count before transient forward = %d, want 1", got)
	}

	forward, err := m.StartTransientForward(token, 5900)
	if err != nil {
		t.Fatalf("StartTransientForward: %v", err)
	}
	if got := m.clients.size(); got != 1 {
		t.Fatalf("transient forward created a second client: %d", got)
	}
	if err := forward.Close(); err != nil {
		t.Fatalf("close transient forward: %v", err)
	}
	// 常驻转发仍借用客户端，因此关闭临时转发不能把它回收。
	if got := m.clients.size(); got != 1 {
		t.Fatalf("shared client count after transient close = %d, want 1", got)
	}

	applyForwardsOnly(t, m, nil)
	if got := m.clients.size(); got != 0 {
		t.Fatalf("shared client count after removing forward = %d, want 0", got)
	}
}
