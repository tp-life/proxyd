package core

// mihomo Tailscale 交互注册桥接。
//
// 该文件只观察 mihomo 已公开的日志事件并主动触发指定出站，不调用 Tailscale SDK，
// 也不读取 tsnet 私有状态。注册链接仅驻留 Runner 内存，避免一次性认证信息写入
// 配置文件或普通运行日志 API 的持久化路径。

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/observable"
	mihomolog "github.com/metacubex/mihomo/log"
)

const (
	// TailscaleEnrollmentStarting 表示配置已加载，正在等待 tsnet 返回认证状态。
	TailscaleEnrollmentStarting = "starting"
	// TailscaleEnrollmentWaiting 表示已获得注册链接，正在等待用户或管理员批准。
	TailscaleEnrollmentWaiting = "waiting_approval"
	// TailscaleEnrollmentConnected 表示 tsnet AuthLoop 已进入 Running。
	TailscaleEnrollmentConnected = "connected"
	// TailscaleEnrollmentError 表示 Auth Key 自动注册未能完成。
	TailscaleEnrollmentError = "error"
	// tailscaleLoginMessagePrefix 是 tsnet 通过 UserLogf 输出交互注册链接时使用的
	// 稳定提示前缀。必须先匹配该语义前缀，不能把 Tailscale DEBUG 日志中的控制面
	// API 地址误当成用户需要打开的注册链接。
	tailscaleLoginMessagePrefix = "To start this tsnet server, restart with TS_AUTHKEY set, or go to: "
	// tailscaleRunningMessage 是 tsnet 完成交互认证并进入 Running 状态时输出的提示。
	tailscaleRunningMessage = "AuthLoop: state is Running; done"
)

// TailscaleEnrollmentSnapshot 是一条可安全返回管理面的内存注册状态。
type TailscaleEnrollmentSnapshot struct {
	Name            string    `json:"name"`
	AuthMode        string    `json:"auth_mode"`
	State           string    `json:"state"`
	RegistrationURL string    `json:"registration_url,omitempty"`
	AuthID          string    `json:"auth_id,omitempty"`
	Message         string    `json:"message,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// tailscaleEnrollmentTracker 订阅 mihomo 全局日志，并只消费已登记节点的 Tailscale
// UserLogf。订阅按需启动，避免没有使用接入向导的 Runner 占用日志订阅缓冲。
type tailscaleEnrollmentTracker struct {
	mu      sync.RWMutex
	startMu sync.Mutex
	items   map[string]TailscaleEnrollmentSnapshot
	sub     observable.Subscription[mihomolog.Event]
	closed  bool
}

// tailscaleEnrollmentTrigger 保存单个 Tailscale 出站当前主动探测的取消函数。
// 使用指针实例作为代际标识，避免旧探测结束时误删同名的新探测。
type tailscaleEnrollmentTrigger struct {
	cancel context.CancelFunc
}

// Begin 登记或重置一个 Tailscale 注册会话，并在首次调用时启动日志观察器。
//
// 参数说明：name 是 mihomo 出站名称；authMode 是 approval 或 auth-key。
//
// 返回值说明：无。
//
// 错误情况：无；Runner 已关闭时忽略新会话，因为核心不再能触发出站。
func (t *tailscaleEnrollmentTracker) Begin(name, authMode string) {
	t.ensureStarted()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if t.items == nil {
		t.items = make(map[string]TailscaleEnrollmentSnapshot)
	}
	t.items[name] = TailscaleEnrollmentSnapshot{
		Name:      name,
		AuthMode:  authMode,
		State:     TailscaleEnrollmentStarting,
		Message:   "正在启动 mihomo/tsnet 注册流程",
		UpdatedAt: time.Now(),
	}
}

// Remove 删除一次尚未提交或已经回滚的注册会话。
//
// 参数说明：name 是需要清理的 mihomo 出站名称。
//
// 返回值说明：无。
//
// 错误情况：无；不存在的名称按幂等删除处理。
func (t *tailscaleEnrollmentTracker) Remove(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.items, name)
}

// Snapshot 返回指定节点当前的注册状态副本。
//
// 参数说明：name 是 mihomo 出站名称。
//
// 返回值说明：状态副本与是否存在；调用方不能修改 tracker 内部状态。
//
// 错误情况：无；未知节点返回零值和 false。
func (t *tailscaleEnrollmentTracker) Snapshot(name string) (TailscaleEnrollmentSnapshot, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	item, ok := t.items[name]
	return item, ok
}

// List 返回全部内存注册状态，并按节点名称排序以稳定 API 输出。
//
// 参数说明：无。
//
// 返回值说明：独立切片；注册链接等字段只存在于进程内存。
//
// 错误情况：无。
func (t *tailscaleEnrollmentTracker) List() []TailscaleEnrollmentSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]TailscaleEnrollmentSnapshot, 0, len(t.items))
	for _, item := range t.items {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TriggerFailed 记录主动启动请求的失败结果。
//
// 参数说明：name 是出站名称；err 是 URLTest 返回的有界错误。
//
// 返回值说明：无。
//
// 错误情况：管理员审批模式下的 URLTest 超时通常表示正在等待批准，因此只更新提示；
// Auth Key 模式没有交互等待阶段，触发失败会标记为 error 供用户排障。
func (t *tailscaleEnrollmentTracker) TriggerFailed(name string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	item, ok := t.items[name]
	if !ok || item.State == TailscaleEnrollmentConnected || item.RegistrationURL != "" {
		return
	}
	item.Message = fmt.Sprintf("启动探测尚未完成：%v", err)
	if item.AuthMode == "auth-key" {
		item.State = TailscaleEnrollmentError
	}
	item.UpdatedAt = time.Now()
	t.items[name] = item
}

// ensureStarted 按需订阅 mihomo 日志并启动唯一消费协程。
//
// 参数说明：无。
//
// 返回值说明：无。
//
// 错误情况：mihomo 的 Subscribe 当前不会失败；Runner 已关闭或已经订阅时直接返回。
func (t *tailscaleEnrollmentTracker) ensureStarted() {
	t.startMu.Lock()
	defer t.startMu.Unlock()
	if t.sub != nil {
		return
	}
	t.mu.RLock()
	closed := t.closed
	t.mu.RUnlock()
	if closed {
		return
	}
	t.sub = mihomolog.Subscribe()
	go t.consume(t.sub)
}

// consume 持续消费 mihomo 日志事件，直到订阅被 Close 关闭。
//
// 参数说明：sub 是本 tracker 独占的 observable 订阅通道。
//
// 返回值说明：无。
//
// 错误情况：通道关闭时自然退出；无法识别或不属于已登记节点的日志被忽略。
func (t *tailscaleEnrollmentTracker) consume(sub observable.Subscription[mihomolog.Event]) {
	for event := range sub {
		t.consumeEvent(event)
	}
}

// consumeEvent 过滤 mihomo 日志等级后再处理 Tailscale 用户可见事件。
//
// 参数说明：event 是 mihomo observable 发布的日志事件，包含等级与未格式化时间戳的
// payload。
//
// 返回值说明：无；只有 INFO 事件会进入状态解析。
//
// 错误情况：无；DEBUG 事件会在 mihomo 控制台等级过滤前进入 observable，其中可能
// 包含 /machine/register 等内部 API 地址，因此必须在这里主动丢弃。
func (t *tailscaleEnrollmentTracker) consumeEvent(event mihomolog.Event) {
	if event.LogLevel != mihomolog.INFO {
		return
	}
	t.consumePayload(event.Payload)
}

// consumePayload 从单条 mihomo Tailscale UserLogf 中提取注册链接或 Running 状态。
//
// 参数说明：payload 是 mihomo log.Event.Payload，不包含日志等级和时间戳。
//
// 返回值说明：无。
//
// 错误情况：节点名可能包含空格或标点，因此不使用宽泛正则拆名称，而是与当前登记
// 名称的完整前缀逐一匹配，避免把其它 Tailscale 出站的认证信息串到当前会话。
func (t *tailscaleEnrollmentTracker) consumePayload(payload string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for name, item := range t.items {
		prefix := fmt.Sprintf("[Tailscale](%s) ", name)
		if !strings.HasPrefix(payload, prefix) {
			continue
		}
		message := strings.TrimPrefix(payload, prefix)
		switch {
		case strings.Contains(message, tailscaleRunningMessage):
			item.State = TailscaleEnrollmentConnected
			item.Message = "已加入 Tailnet，mihomo Tailscale 出站正在运行"
		case strings.HasPrefix(message, tailscaleLoginMessagePrefix):
			loginURL, authID, ok := parseTailscaleRegistrationURL(message)
			if !ok {
				continue
			}
			item.RegistrationURL = loginURL
			item.AuthID = authID
			item.State = TailscaleEnrollmentWaiting
			if authID != "" {
				item.Message = "请复制 Auth ID 或注册链接给 Headscale 管理员批准"
			} else {
				item.Message = "请复制注册链接并按控制面页面提示完成批准"
			}
		default:
			continue
		}
		item.UpdatedAt = time.Now()
		t.items[name] = item
		return
	}
}

// parseTailscaleRegistrationURL 从 tsnet 用户提示中提取可打开的认证 URL，并在
// Headscale 标准 `/register/<Auth ID>` 路径存在时同时拆出 Auth ID。
//
// 参数说明：message 是已经去掉 `[Tailscale](节点名)` 前缀的 INFO 日志正文。
//
// 返回值说明：registrationURL 是经过语法校验的绝对 HTTP(S) URL；authID 是可选的
// Headscale 审批标识；ok 表示该日志是否为有效的 tsnet 登录提示。
//
// 错误情况：提示前缀不匹配、URL 为空、URL 不是 HTTP(S) 绝对地址或解析失败时返回
// 空值与 false。Auth ID 路径不符合 Headscale 形式时仍返回有效 URL，但 authID 为空，
// 以兼容 Tailscale 官方控制面或其它实现不同的认证 URL。
func parseTailscaleRegistrationURL(message string) (registrationURL, authID string, ok bool) {
	if !strings.HasPrefix(message, tailscaleLoginMessagePrefix) {
		return "", "", false
	}
	rawURL := strings.TrimSpace(strings.TrimPrefix(message, tailscaleLoginMessagePrefix))
	rawURL = strings.TrimRight(rawURL, `.,;\"'`)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", "", false
	}

	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) >= 2 && segments[len(segments)-2] == "register" {
		authID = strings.TrimSpace(segments[len(segments)-1])
	}
	return parsed.String(), authID, true
}

// Close 取消 mihomo 日志订阅并结束消费协程。
//
// 参数说明：无。
//
// 返回值说明：无。
//
// 错误情况：无；重复关闭是幂等操作，未启动订阅时只记录 closed 状态。
func (t *tailscaleEnrollmentTracker) Close() {
	t.startMu.Lock()
	defer t.startMu.Unlock()
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	sub := t.sub
	t.sub = nil
	t.mu.Unlock()
	if sub != nil {
		mihomolog.UnSubscribe(sub)
	}
}

// BeginTailscaleEnrollment 创建一个由管理面观察的 Tailscale 注册会话。
//
// 参数说明：name 是 mihomo 出站名；authMode 是 approval 或 auth-key。
//
// 返回值说明：无。
//
// 错误情况：无；生命周期由 Runner.Shutdown 统一清理。
func (r *Runner) BeginTailscaleEnrollment(name, authMode string) {
	r.tailscaleEnrollment.Begin(name, authMode)
}

// RemoveTailscaleEnrollment 清理配置事务回滚产生的临时注册状态。
//
// 参数说明：name 是已回滚的 mihomo 出站名。
//
// 返回值说明：无。
//
// 错误情况：无；不存在时幂等返回。
func (r *Runner) RemoveTailscaleEnrollment(name string) {
	r.CancelTailscaleEnrollmentTrigger(name)
	r.tailscaleEnrollment.Remove(name)
}

// CancelTailscaleEnrollmentTrigger 取消指定 Tailscale 出站尚在执行的主动 URLTest，
// 但保留管理面注册快照，供终止事务失败时继续展示诊断状态。
//
// 参数说明：name 是 mihomo 出站名称。
//
// 返回值说明：无；存在探测时触发其 context 取消，不等待网络栈退出。
//
// 错误情况：无；名称不存在或探测已经结束时幂等返回。注册触发使用独立于 Reload
// 锁的专用探测路径；取消用于尽快结束旧网络请求并阻止其结果污染同名新会话。
func (r *Runner) CancelTailscaleEnrollmentTrigger(name string) {
	r.tailscaleTriggerMu.Lock()
	trigger := r.tailscaleTriggers[name]
	delete(r.tailscaleTriggers, name)
	r.tailscaleTriggerMu.Unlock()
	if trigger != nil {
		trigger.cancel()
	}
}

// registerTailscaleEnrollmentTrigger 登记同名出站的新主动探测，并取消此前尚未结束的
// 旧探测，保证一个出站最多只有一个主动注册 URLTest。
//
// 参数说明：name 是 mihomo 出站名称；cancel 是新探测的取消函数。
//
// 返回值说明：*tailscaleEnrollmentTrigger，作为本次探测的代际标识。
//
// 错误情况：无；map 按需初始化，旧探测取消属于正常替换流程。
func (r *Runner) registerTailscaleEnrollmentTrigger(name string, cancel context.CancelFunc) *tailscaleEnrollmentTrigger {
	trigger := &tailscaleEnrollmentTrigger{cancel: cancel}
	r.tailscaleTriggerMu.Lock()
	if r.tailscaleTriggers == nil {
		r.tailscaleTriggers = make(map[string]*tailscaleEnrollmentTrigger)
	}
	previous := r.tailscaleTriggers[name]
	r.tailscaleTriggers[name] = trigger
	r.tailscaleTriggerMu.Unlock()
	if previous != nil {
		previous.cancel()
	}
	return trigger
}

// unregisterTailscaleEnrollmentTrigger 在主动探测结束后移除仍属于本代的登记项。
//
// 参数说明：name 是 mihomo 出站名称；trigger 是 register 返回的代际标识。
//
// 返回值说明：无。
//
// 错误情况：无；若同名新探测已经替换当前项，则保留新登记，避免旧协程误取消它。
func (r *Runner) unregisterTailscaleEnrollmentTrigger(name string, trigger *tailscaleEnrollmentTrigger) {
	r.tailscaleTriggerMu.Lock()
	defer r.tailscaleTriggerMu.Unlock()
	if r.tailscaleTriggers[name] == trigger {
		delete(r.tailscaleTriggers, name)
	}
}

// isCurrentTailscaleEnrollmentTrigger 判断一个刚结束的探测是否仍属于该出站的当前代。
//
// 参数说明：name 是 mihomo 出站名称；trigger 是当前协程持有的代际标识。
//
// 返回值说明：登记表仍指向同一实例时返回 true；已终止或已被新探测替换时返回 false。
//
// 错误情况：无。终止后旧 tsnet 请求可能稍晚才返回，必须用该检查阻止它把
// context canceled 等结果写入用户刚刚同名重建的新注册会话。
func (r *Runner) isCurrentTailscaleEnrollmentTrigger(name string, trigger *tailscaleEnrollmentTrigger) bool {
	r.tailscaleTriggerMu.Lock()
	defer r.tailscaleTriggerMu.Unlock()
	return r.tailscaleTriggers[name] == trigger
}

// TailscaleEnrollment 返回指定节点的内存注册状态。
//
// 参数说明：name 是 mihomo 出站名。
//
// 返回值说明：状态副本和存在标志。
//
// 错误情况：无。
func (r *Runner) TailscaleEnrollment(name string) (TailscaleEnrollmentSnapshot, bool) {
	return r.tailscaleEnrollment.Snapshot(name)
}

// TailscaleEnrollments 返回当前进程内的全部 Tailscale 注册状态。
//
// 参数说明：无。
//
// 返回值说明：按名称排序的状态副本。
//
// 错误情况：无。
func (r *Runner) TailscaleEnrollments() []TailscaleEnrollmentSnapshot {
	return r.tailscaleEnrollment.List()
}

// TriggerTailscaleEnrollment 通过一次有界 URLTest 主动启动 mihomo 的懒加载
// Tailscale 出站，使 tsnet 产生注册链接或完成 Auth Key 登录。
//
// 参数说明：ctx 控制取消；name 是已加载到 mihomo 代理表的出站名称。
//
// 返回值说明：无；最终状态由日志观察器异步更新。
//
// 错误情况：未配置 Exit Node 时公网测试预期会失败，但启动动作已经发生；审批模式
// 保持等待状态，Auth Key 模式记录 error，避免把探测失败误当成未执行。
func (r *Runner) TriggerTailscaleEnrollment(ctx context.Context, name string) {
	triggerCtx, cancel := context.WithCancel(ctx)
	trigger := r.registerTailscaleEnrollmentTrigger(name, cancel)
	defer func() {
		cancel()
		r.unregisterTailscaleEnrollmentTrigger(name, trigger)
	}()
	// Runner.Shutdown 必须能立即终止等待管理员审批的 URLTest；否则关闭进程会被单次
	// 15 秒探测拖住。独立协程只等待两个取消信号，Trigger 返回时由 defer 负责回收。
	go func() {
		select {
		case <-r.shutdown:
			cancel()
		case <-triggerCtx.Done():
		}
	}()
	err := r.triggerTailscaleURLTest(triggerCtx, name, "http://100.100.100.100", 15*time.Second)
	if err != nil && r.isCurrentTailscaleEnrollmentTrigger(name, trigger) {
		r.tailscaleEnrollment.TriggerFailed(name, err)
	}
}
