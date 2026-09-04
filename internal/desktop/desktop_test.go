package desktop

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeForward 是会话生命周期测试使用的无网络转发器。
type fakeForward struct {
	mu      sync.Mutex
	active  int64
	closed  int
	onClose func()
}

// Address 返回稳定的测试回环地址。
//
// 参数说明：无。
// 返回值说明：string，固定为 127.0.0.1:41000。
// 错误情况：无。
func (f *fakeForward) Address() string { return "127.0.0.1:41000" }

// ActiveConnections 返回测试控制的活动连接数。
//
// 参数说明：无。
// 返回值说明：int64，受互斥锁保护的当前值。
// 错误情况：无。
func (f *fakeForward) ActiveConnections() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

// Close 记录一次底层资源释放动作。
//
// 参数说明：无。
// 返回值说明：error，测试实现始终返回 nil。
// 错误情况：无。
func (f *fakeForward) Close() error {
	f.mu.Lock()
	f.closed++
	onClose := f.onClose
	f.mu.Unlock()
	if onClose != nil {
		onClose()
	}
	return nil
}

// setActive 修改测试转发器的活动连接数。
//
// 参数说明：active 为新的活动连接数。
// 返回值说明：无。
// 错误情况：测试只传非负值，不执行额外校验。
func (f *fakeForward) setActive(active int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = active
}

// closeCount 返回测试转发器累计关闭次数。
//
// 参数说明：无。
// 返回值说明：int，当前累计值。
// 错误情况：无。
func (f *fakeForward) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// testSessionSpec 构造合法的最小 RDP 会话输入。
//
// 参数说明：无。
// 返回值说明：SessionSpec，供多个生命周期用例复用。
// 错误情况：无。
func testSessionSpec() SessionSpec {
	return SessionSpec{ConnectionName: "办公室电脑", Remote: "office", Protocol: ProtocolRDP, RemotePort: 3389}
}

// TestManagerStartReuseAndStop 验证同一连接档案只保留一个会话且显式断开释放资源。
//
// 参数说明：t 为 Go 测试上下文。
// 返回值说明：无；断言全部成立即通过。
// 错误情况：工厂被重复调用、ID 不稳定或转发未关闭时测试失败。
func TestManagerStartReuseAndStop(t *testing.T) {
	forward := &fakeForward{}
	created := 0
	manager := NewManager(func(SessionSpec) (Forward, error) {
		created++
		return forward, nil
	}, Options{SweepInterval: time.Hour})
	t.Cleanup(manager.Close)

	first, err := manager.Start(testSessionSpec())
	if err != nil {
		t.Fatalf("首次启动桌面会话失败: %v", err)
	}
	second, err := manager.Start(testSessionSpec())
	if err != nil {
		t.Fatalf("复用桌面会话失败: %v", err)
	}
	if created != 1 || first.ID != second.ID || first.LocalAddress != "127.0.0.1:41000" {
		t.Fatalf("会话未正确复用: created=%d first=%+v second=%+v", created, first, second)
	}
	if err := manager.Stop(first.ID); err != nil {
		t.Fatalf("显式断开失败: %v", err)
	}
	if forward.closeCount() != 1 {
		t.Fatalf("底层转发关闭次数 = %d，期望 1", forward.closeCount())
	}
	if _, err := manager.Get(first.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("断开后 Get 错误 = %v，期望 ErrSessionNotFound", err)
	}
}

// TestManagerSweepsStartupAndIdleSessions 验证未连接宽限期和连接后空闲期都会自动回收。
//
// 参数说明：t 为 Go 测试上下文。
// 返回值说明：无；两个超时边界均释放对应转发时通过。
// 错误情况：清扫过早、过晚或活动状态未更新时测试失败。
func TestManagerSweepsStartupAndIdleSessions(t *testing.T) {
	forwards := []*fakeForward{{}, {}}
	index := 0
	manager := NewManager(func(SessionSpec) (Forward, error) {
		forward := forwards[index]
		index++
		return forward, nil
	}, Options{StartupGrace: time.Minute, IdleTimeout: 30 * time.Second, MaxLifetime: time.Hour, SweepInterval: time.Hour})
	t.Cleanup(manager.Close)
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return base }

	startup, err := manager.Start(testSessionSpec())
	if err != nil {
		t.Fatalf("启动待连接会话失败: %v", err)
	}
	manager.sweep(base.Add(59 * time.Second))
	if _, err := manager.Get(startup.ID); err != nil {
		t.Fatalf("启动宽限期内会话被过早回收: %v", err)
	}
	manager.sweep(base.Add(time.Minute))
	if forwards[0].closeCount() != 1 {
		t.Fatalf("未连接会话到期关闭次数 = %d，期望 1", forwards[0].closeCount())
	}

	idleSpec := testSessionSpec()
	idleSpec.ConnectionName = "机房电脑"
	idle, err := manager.Start(idleSpec)
	if err != nil {
		t.Fatalf("启动空闲回收会话失败: %v", err)
	}
	forwards[1].setActive(1)
	manager.sweep(base.Add(10 * time.Second))
	forwards[1].setActive(0)
	manager.sweep(base.Add(39 * time.Second))
	if _, err := manager.Get(idle.ID); err != nil {
		t.Fatalf("空闲超时前会话被过早回收: %v", err)
	}
	manager.sweep(base.Add(40 * time.Second))
	if forwards[1].closeCount() != 1 {
		t.Fatalf("空闲会话到期关闭次数 = %d，期望 1", forwards[1].closeCount())
	}
}

// TestManagerRejectsInvalidOrClosedStart 验证无效输入和关闭后的管理器都不会创建转发。
//
// 参数说明：t 为 Go 测试上下文。
// 返回值说明：无；错误类型和工厂调用次数符合预期时通过。
// 错误情况：校验被绕过或关闭后仍接受新会话时测试失败。
func TestManagerRejectsInvalidOrClosedStart(t *testing.T) {
	created := 0
	manager := NewManager(func(SessionSpec) (Forward, error) {
		created++
		return &fakeForward{}, nil
	}, Options{SweepInterval: time.Hour})
	if _, err := manager.Start(SessionSpec{}); err == nil {
		t.Fatal("空会话输入应被拒绝")
	}
	manager.Close()
	if _, err := manager.Start(testSessionSpec()); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("关闭后 Start 错误 = %v，期望 ErrManagerClosed", err)
	}
	if created != 0 {
		t.Fatalf("非法或关闭请求不应调用工厂，got %d", created)
	}
}

// TestManagerCloseDoesNotHoldLockDuringForwardClose 验证关闭外部资源时不持有管理器锁。
//
// 参数说明：t 为 Go 测试上下文。
//
// 返回值说明：无；Forward.Close 能回调只读 List 且管理器及时退出时通过。
//
// 错误情况：若 Manager.Close 在持锁状态调用外部 Close，会发生可复现的自锁并超时；
// 这会模拟真实隧道收口期间回调指标或状态读取的风险。
func TestManagerCloseDoesNotHoldLockDuringForwardClose(t *testing.T) {
	forward := &fakeForward{}
	var manager *Manager
	forward.onClose = func() {
		_ = manager.List()
	}
	manager = NewManager(func(SessionSpec) (Forward, error) {
		return forward, nil
	}, Options{SweepInterval: time.Hour})
	if _, err := manager.Start(testSessionSpec()); err != nil {
		t.Fatalf("启动桌面会话失败: %v", err)
	}
	done := make(chan struct{})
	go func() {
		manager.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Manager.Close 持锁调用 Forward.Close，产生退出死锁")
	}
	if forward.closeCount() != 1 {
		t.Fatalf("退出时底层转发关闭次数 = %d，期望 1", forward.closeCount())
	}
}
