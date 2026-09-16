package app

// 代理域应用层：编排订阅刷新草稿的生成、并发基线校验、确认应用与丢弃。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/pool"
	"proxyd/internal/proxy/subscribe"
)

// subscriptionDraftTTL 限制原始订阅正文和完整节点映射在内存中的最长待确认时间。
const subscriptionDraftTTL = 30 * time.Minute

var (
	// ErrSubscriptionDraftNotFound 表示订阅当前没有待确认草稿或 ID 不匹配。
	ErrSubscriptionDraftNotFound = errors.New("订阅刷新草稿不存在")
	// ErrSubscriptionDraftExpired 表示草稿超过有效期，候选数据不应继续应用。
	ErrSubscriptionDraftExpired = errors.New("订阅刷新草稿已过期")
	// ErrSubscriptionDraftStale 表示预览后运行节点或策略组已经变化，必须重新生成。
	ErrSubscriptionDraftStale = errors.New("订阅刷新草稿基线已变化")
)

// SubscriptionDraftView 是可返回给 API/UI 的订阅刷新草稿视图。
//
// 视图只包含不可逆的随机 ID、时间、缓存降级提示和领域差异；节点稳定身份、原始
// Mapping、订阅正文均保留在 subscriptionDraftState 私有状态中，避免凭据泄露。
type SubscriptionDraftView struct {
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	CreatedAt time.Time           `json:"created_at"`
	ExpiresAt time.Time           `json:"expires_at"`
	Warning   string              `json:"warning,omitempty"`
	Diff      subscribe.DraftDiff `json:"diff"`
}

// subscriptionDraftState 是一次待确认刷新在进程内的应用层实体。
type subscriptionDraftState struct {
	view     SubscriptionDraftView
	target   config.Subscription
	baseline string
	nodes    []*node.Node
	fetched  *subscribe.FetchedSubscription
	stateDir string
}

// CreateSubscriptionDraft 拉取并检测单个订阅，生成不影响运行态和缓存的刷新草稿。
//
// 参数：
//   - ctx: context.Context，控制远端拉取和健康检测的超时/取消。
//   - name: string，目标订阅名称。
//
// 返回值：SubscriptionDraftView，只含安全差异；error 表示订阅不存在/停用、拉取、
// 解析、随机 ID 生成失败，或慢速阶段结束前订阅已变化。
//
// 错误情况：同一订阅的刷新/测速/草稿操作由 subOps 串行；慢速网络阶段不持有
// refreshing。提交草稿前才短暂获取全局锁并重新读取其它来源，避免草稿覆盖并发刷新。
func (a *App) CreateSubscriptionDraft(ctx context.Context, name string) (SubscriptionDraftView, error) {
	unlock := a.lockSubOp(name)
	defer unlock()

	a.mu.RLock()
	var target *config.Subscription
	for i := range a.cfg.Subscriptions {
		if a.cfg.Subscriptions[i].Name == name {
			copied := a.cfg.Subscriptions[i]
			target = &copied
			break
		}
	}
	stateDir := a.cfg.StateDir
	a.mu.RUnlock()
	if target == nil {
		return SubscriptionDraftView{}, fmt.Errorf("订阅 %q 不存在", name)
	}
	if !target.IsEnabled() {
		return SubscriptionDraftView{}, fmt.Errorf("订阅 %q 已禁用，请先启用后再同步", name)
	}

	fetched, fetchErr := subscribe.FetchPreviewWithInfoOptions(ctx, *target, stateDir, a.subscriptionFetchOptions())
	warning := ""
	if fetchErr != nil {
		var fallback *subscribe.FetchWarning
		if !errors.As(fetchErr, &fallback) {
			return SubscriptionDraftView{}, fetchErr
		}
		// 原始网络错误可能回显带 token 的 URL，只写服务器日志；UI 使用固定提示，
		// 既说明数据来源，又不把凭据型订阅地址扩散到响应。
		log.Printf("[subscribe] 创建草稿时 %v", fetchErr)
		warning = "远端拉取失败，本草稿来自最后一份已确认缓存"
	}
	fresh := fetched.Nodes()
	if len(fresh) == 0 {
		return SubscriptionDraftView{}, fmt.Errorf("订阅 %q 没有可预览的节点", name)
	}
	a.checkNodes(ctx, fresh, a.dialerTargets()...)

	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	if err := a.subscriptionUnchangedLocked(name, *target); err != nil {
		return SubscriptionDraftView{}, err
	}

	// MergeFiltered 会原地同步唯一展示名与 Mapping.name。其它来源必须先克隆，
	// 否则仅仅生成草稿就会修改 a.nodes 指向的共享实体，违反“预览不影响运行态”。
	current := a.Nodes()
	groups := map[string][]*node.Node{name: fresh}
	for _, existing := range current {
		if existing != nil && existing.Subscription != name {
			cloned := cloneSubscriptionDraftNode(existing)
			groups[cloned.Subscription] = append(groups[cloned.Subscription], cloned)
		}
	}
	candidates := subscribe.MergeFiltered(groups, a.includeRe, a.excludeRe)
	if len(candidates) == 0 {
		return SubscriptionDraftView{}, fmt.Errorf("刷新草稿没有任何可应用节点")
	}

	beforeTarget := filterSubscriptionDraftNodes(current, name)
	afterTarget := filterSubscriptionDraftNodes(candidates, name)
	id, err := newSubscriptionDraftID()
	if err != nil {
		return SubscriptionDraftView{}, err
	}
	now := time.Now().UTC()
	view := SubscriptionDraftView{
		ID: id, Name: name, CreatedAt: now, ExpiresAt: now.Add(subscriptionDraftTTL),
		Warning: warning, Diff: subscribe.CompareDraftNodes(beforeTarget, afterTarget),
	}
	state := &subscriptionDraftState{
		view: view, target: *target, nodes: candidates, fetched: fetched, stateDir: stateDir,
		baseline: a.subscriptionDraftBaseline(),
	}
	a.mu.Lock()
	a.subscriptionDrafts[name] = state
	a.mu.Unlock()
	// 定时器只捕获订阅名和随机 ID，不捕获含正文的 state；草稿被替换或提前处理时，
	// 旧定时器的 ID 校验会使它安全空转，绝不会删除后来创建的新草稿。
	time.AfterFunc(subscriptionDraftTTL, func() {
		a.discardSubscriptionDraftState(name, id)
	})
	return view, nil
}

// SubscriptionDraft 返回指定订阅当前仍有效的草稿视图。
//
// 参数：name 为订阅名称。
// 返回值：SubscriptionDraftView 为安全视图；error 表示不存在或已过期。
// 错误情况：过期草稿会在读取时从内存删除，避免原始订阅正文长期驻留。
func (a *App) SubscriptionDraft(name string) (SubscriptionDraftView, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.subscriptionDrafts[name]
	if state == nil {
		return SubscriptionDraftView{}, ErrSubscriptionDraftNotFound
	}
	if !time.Now().Before(state.view.ExpiresAt) {
		delete(a.subscriptionDrafts, name)
		return SubscriptionDraftView{}, ErrSubscriptionDraftExpired
	}
	return state.view, nil
}

// ApplySubscriptionDraft 校验草稿 ID、有效期和运行基线后，将候选节点热更新为运行态。
//
// 参数：
//   - ctx: context.Context，控制 mihomo 链式节点验证等确认阶段操作。
//   - name: string，订阅名称。
//   - id: string，CreateSubscriptionDraft 返回的随机草稿 ID。
//
// 返回值：error；nil 表示运行态、节点快照、用量信息和已确认缓存均已收敛。
//
// 错误情况：草稿不存在/过期/基线变化时拒绝应用；热更新失败时恢复旧节点、端口分配
// 与 mihomo 配置。缓存提交失败仅记录告警，因为运行态已经成功，不能把成功报告成可重试失败。
func (a *App) ApplySubscriptionDraft(ctx context.Context, name, id string) error {
	unlock := a.lockSubOp(name)
	defer unlock()
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	a.mu.Lock()
	state := a.subscriptionDrafts[name]
	if state == nil || state.view.ID != id {
		a.mu.Unlock()
		return ErrSubscriptionDraftNotFound
	}
	if !time.Now().Before(state.view.ExpiresAt) {
		delete(a.subscriptionDrafts, name)
		a.mu.Unlock()
		return ErrSubscriptionDraftExpired
	}
	a.mu.Unlock()
	if err := a.subscriptionUnchangedLocked(name, state.target); err != nil {
		a.discardSubscriptionDraftState(name, id)
		return fmt.Errorf("%w: %v", ErrSubscriptionDraftStale, err)
	}
	if a.subscriptionDraftBaseline() != state.baseline {
		a.discardSubscriptionDraftState(name, id)
		return fmt.Errorf("%w，请重新同步后确认", ErrSubscriptionDraftStale)
	}

	a.mu.RLock()
	oldSubscriptions := append([]config.Subscription(nil), a.cfg.Subscriptions...)
	oldGroups := cloneNodeGroups(a.cfg.Groups)
	oldNodes := append([]*node.Node(nil), a.nodes...)
	oldAssignments := append([]pool.Assignment(nil), a.assigns...)
	oldInfos := cloneSubscriptionInfos(a.subInfos)
	a.mu.RUnlock()
	// 使用深到 Mapping 顶层的克隆应用，避免 Runner 的链式验证修改草稿实体；若热更新
	// 失败，用户仍可查看同一草稿，同时运行态通过旧快照回滚。
	candidates := cloneSubscriptionDraftNodes(state.nodes)
	if err := a.applyNodes(ctx, candidates); err != nil {
		return a.rollbackSubscriptionLocked(
			oldSubscriptions, oldGroups, oldNodes, oldAssignments, oldInfos, err, false,
		)
	}

	info := state.fetched.Info()
	a.mu.Lock()
	if !info.IsZero() {
		a.subInfos[name] = info
	}
	delete(a.subscriptionDrafts, name)
	a.mu.Unlock()
	if err := state.fetched.CommitCache(state.stateDir, name); err != nil {
		log.Printf("[subscribe] 草稿已应用，但提交订阅 %s 缓存失败: %v", name, err)
	}
	return nil
}

// DiscardSubscriptionDraft 丢弃指定 ID 的待确认草稿。
//
// 参数：name 为订阅名称，id 为要丢弃的草稿 ID。
// 返回值：error；匹配并删除返回 nil，不存在或 ID 已被新草稿替换时返回 not found。
// 错误情况：ID 不匹配时绝不删除较新的草稿，避免旧页面关闭动作误伤新预览。
func (a *App) DiscardSubscriptionDraft(name, id string) error {
	// 与创建/应用共用订阅级锁，使“取消预览”在线性时序上要么发生在应用前、
	// 要么等待应用完成后返回不存在，不会出现已经丢弃的草稿仍被并发确认。
	unlock := a.lockSubOp(name)
	defer unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.subscriptionDrafts[name]
	if state == nil || state.view.ID != id {
		return ErrSubscriptionDraftNotFound
	}
	delete(a.subscriptionDrafts, name)
	return nil
}

// discardSubscriptionDraftState 在调用方不持有 mu 时按 ID 尽力清理草稿。
//
// 参数：name 为订阅名，id 为待清理 ID。
// 返回值：无。
// 错误情况：草稿已被并发替换时保持新草稿不变。
func (a *App) discardSubscriptionDraftState(name, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if state := a.subscriptionDrafts[name]; state != nil && state.view.ID == id {
		delete(a.subscriptionDrafts, name)
	}
}

// subscriptionDraftBaseline 计算当前节点与策略组的不可逆并发基线。
//
// 参数：无；函数自行在 mu 读锁中读取运行快照。
// 返回值：string，SHA-256 十六进制摘要，仅用于相等比较，不进入持久化。
// 错误情况：JSON 序列化 NodeGroup 理论上不会失败；若未来加入不可序列化字段，使用
// 固定错误标记参与摘要，使草稿保守失效而不是绕过并发检查。
func (a *App) subscriptionDraftBaseline() string {
	a.mu.RLock()
	nodes := append([]*node.Node(nil), a.nodes...)
	groups := cloneNodeGroups(a.cfg.Groups)
	a.mu.RUnlock()

	rows := make([]string, 0, len(nodes))
	for _, candidate := range nodes {
		if candidate == nil {
			continue
		}
		// Key 可能含凭据，但只进入单向摘要且不被记录或返回；健康字段也参与基线，
		// 防止预览期间的独立测速结果被候选集合中的旧状态覆盖。
		rows = append(rows, fmt.Sprintf("%s\x00%s\x00%s\x00%t\x00%d\x00%s",
			candidate.Key(), candidate.Name, candidate.Subscription, candidate.Alive, candidate.Delay, candidate.FailReason))
	}
	sort.Strings(rows)
	hasher := sha256.New()
	for _, row := range rows {
		_, _ = hasher.Write([]byte(row))
		_, _ = hasher.Write([]byte{0xff})
	}
	groupJSON, err := json.Marshal(groups)
	if err != nil {
		groupJSON = []byte("group-serialization-error")
	}
	_, _ = hasher.Write(groupJSON)
	return hex.EncodeToString(hasher.Sum(nil))
}

// newSubscriptionDraftID 生成不可预测的草稿确认令牌。
//
// 参数：无。
// 返回值：string 为 128 位随机数的十六进制表示；error 为系统随机源失败。
// 错误情况：随机源不可用时拒绝创建草稿，避免退化为可猜测确认 ID。
func newSubscriptionDraftID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("生成订阅草稿 ID 失败: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// cloneSubscriptionDraftNode 克隆节点及 Mapping 顶层键值。
//
// 参数：source 为源节点，可为 nil。
// 返回值：*node.Node，nil 输入返回 nil；Mapping 的嵌套值保持只读共享。
// 错误情况：无。草稿合并只会修改 Mapping 顶层 name/dialer-proxy，故无需递归复制
// 不参与变更的嵌套协议选项，避免无意义的反射复制。
func cloneSubscriptionDraftNode(source *node.Node) *node.Node {
	if source == nil {
		return nil
	}
	cloned := *source
	if source.Mapping != nil {
		cloned.Mapping = make(map[string]any, len(source.Mapping))
		for key, value := range source.Mapping {
			cloned.Mapping[key] = value
		}
	}
	return &cloned
}

// cloneSubscriptionDraftNodes 批量克隆草稿候选节点。
//
// 参数：sources 为源节点集合。
// 返回值：[]*node.Node，保持顺序和 nil 位置。
// 错误情况：无；空输入返回非 nil 空切片，便于后续安全追加。
func cloneSubscriptionDraftNodes(sources []*node.Node) []*node.Node {
	out := make([]*node.Node, len(sources))
	for i, source := range sources {
		out[i] = cloneSubscriptionDraftNode(source)
	}
	return out
}

// filterSubscriptionDraftNodes 提取指定订阅的节点引用用于领域差异计算。
//
// 参数：nodes 为完整节点集，name 为订阅名。
// 返回值：[]*node.Node，只包含来源匹配的非 nil 节点，保持原顺序。
// 错误情况：无；空输入或没有匹配项返回空切片。
func filterSubscriptionDraftNodes(nodes []*node.Node, name string) []*node.Node {
	out := make([]*node.Node, 0)
	for _, candidate := range nodes {
		if candidate != nil && candidate.Subscription == name {
			out = append(out, candidate)
		}
	}
	return out
}
