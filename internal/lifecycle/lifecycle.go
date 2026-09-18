// Package lifecycle 定义模块运行状态，不依赖网络、配置或第三方 SDK。
package lifecycle

import (
	"sync"
	"time"
)

// State 区分期望启用状态与实际运行阶段；失败时保留可恢复入口所需的信息。
type State struct {
	Phase       string     `json:"phase"`
	Running     bool       `json:"running"`
	Error       string     `json:"error,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
	Attempts    uint64     `json:"attempts"`
}

// Tracker 串行保存一个模块的运行快照；世代号防止迟到结果覆盖新操作。
type Tracker struct {
	mu         sync.Mutex
	state      State
	generation uint64
}

// Begin 标记一次应用尝试，保留已有可用服务状态。
// 参数：无；返回 uint64 世代；无错误，状态锁不覆盖实际网络或磁盘操作。
func (t *Tracker) Begin() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.generation++
	t.state.Attempts++
	t.state.Phase = "starting"
	t.state.NextRetryAt = nil
	t.state.UpdatedAt = time.Now().UTC()
	return t.generation
}

// Complete 提交仍属于当前世代的结果。
// 参数：generation 为 Begin 的返回值；phase、running、message 为结果；retryAfter 为零时不安排重试。
// 返回无；过期结果忽略，重试时间返回独立值，避免共享指针竞争。
func (t *Tracker) Complete(generation uint64, phase string, running bool, message string, retryAfter time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if generation != t.generation {
		return
	}
	t.state.Phase = phase
	t.state.Running = running
	t.state.Error = message
	t.state.UpdatedAt = time.Now().UTC()
	t.state.NextRetryAt = nil
	if retryAfter > 0 {
		next := t.state.UpdatedAt.Add(retryAfter)
		t.state.NextRetryAt = &next
	}
}

// CompleteCurrent 以当前世代提交结果，用于流水线收尾阶段覆盖本轮尝试的最终状态。
// 与 Begin+Complete 组合的区别：不新增尝试计数，也不会先把相位闪成 starting。
// 参数：phase、running、message 同 Complete；retryAfter 为零时不安排重试。
// 返回无；调用方需确保没有并发流水线正在运行（本包调用点均持串行锁）。
func (t *Tracker) CompleteCurrent(phase string, running bool, message string, retryAfter time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.Phase = phase
	t.state.Running = running
	t.state.Error = message
	t.state.UpdatedAt = time.Now().UTC()
	t.state.NextRetryAt = nil
	if retryAfter > 0 {
		next := t.state.UpdatedAt.Add(retryAfter)
		t.state.NextRetryAt = &next
	}
}

// Snapshot 返回独立快照；参数无，返回 State；尚无操作时为 idle，无错误。
func (t *Tracker) Snapshot() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.state
	if s.Phase == "" {
		s.Phase = "idle"
	}
	if s.NextRetryAt != nil {
		next := *s.NextRetryAt
		s.NextRetryAt = &next
	}
	return s
}
