package gateway

// helper 看门狗：主进程失联超时自动清除 pf 规则并恢复转发，
// 防止主进程崩溃后下游设备断网（docs/adr/0003）。
// 本文件不含平台代码，时钟与执行动作均可注入，便于跨平台单测。

import (
	"context"
	"sync"
	"time"
)

const (
	// helperWatchdogTimeout 是主进程失联判定阈值；心跳间隔（30s，见 Manager）给它三倍余量。
	helperWatchdogTimeout = 90 * time.Second
	// helperWatchdogInterval 是看门狗周期检查间隔。
	helperWatchdogInterval = 15 * time.Second
)

// helperWatchdog 记录最近一次已授权请求时间；只在规则已应用时才需要触发清理。
type helperWatchdog struct {
	now     func() time.Time
	timeout time.Duration

	mu   sync.Mutex
	last time.Time
}

// newHelperWatchdog 创建看门狗；last 初始化为创建时刻，给 helper 启动留出完整超时窗口。
//
// 参数：
//   - now: func() time.Time，时钟（测试注入假时钟）。
//   - timeout: time.Duration，失联判定阈值。
//
// 返回值：*helperWatchdog。
//
// 错误情况：无。
func newHelperWatchdog(now func() time.Time, timeout time.Duration) *helperWatchdog {
	return &helperWatchdog{now: now, timeout: timeout, last: now()}
}

// beat 记录一次主进程存活信号（任何成功执行的指令均算）。
func (w *helperWatchdog) beat() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = w.now()
}

// expired 报告当前是否已失联超时。
func (w *helperWatchdog) expired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now().Sub(w.last) > w.timeout
}

// runHelperWatchdog 周期检查看门狗：失联且规则仍已应用时执行 onExpire 一次
// （清理后规则不再应用，不会重复触发，直到下一次 pf.apply 后再次失联）。
//
// 参数：
//   - ctx: context.Context，helper 生命周期；取消后退出。
//   - w: *helperWatchdog，心跳记录。
//   - interval: time.Duration，检查间隔。
//   - rulesApplied: func() bool，平台层报告规则当前是否已应用。
//   - onExpire: func()，超时清理动作（pf.clear + 恢复转发原值）。
//
// 返回值：无；ctx 取消即返回。
//
// 错误情况：onExpire 的内部失败由平台层自行记录，看门狗不重试（下一周期规则已清，
// 条件不再满足；若清理失败导致规则仍在，则下一周期会再次尝试）。
func runHelperWatchdog(ctx context.Context, w *helperWatchdog, interval time.Duration, rulesApplied func() bool, onExpire func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.expired() && rulesApplied() {
				onExpire()
			}
		}
	}
}
