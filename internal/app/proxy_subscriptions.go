package app

// 代理域：订阅与手动节点的查询、增删改用例。

import (
	"context"
	"errors"
	"fmt"
	"log"
	urlpkg "net/url"
	"strings"
	"sync"

	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/pool"
	"proxyd/internal/proxy/subscribe"
)

// Nodes 返回当前节点列表快照（含健康状态）。
func (a *App) Nodes() []*node.Node {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*node.Node, len(a.nodes))
	copy(out, a.nodes)
	return out
}

// Subscriptions 返回配置中的订阅列表（供 API 展示）。
func (a *App) Subscriptions() []config.Subscription {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]config.Subscription, len(a.cfg.Subscriptions))
	copy(out, a.cfg.Subscriptions)
	return out
}

// SubscriptionUserInfos 返回订阅流量/到期信息快照。
//
// 参数：无。
//
// 返回值：
//   - map[string]subscribe.UserInfo: 订阅名到用量信息的映射；调用方可自由修改返回 map。
//
// 错误情况：无；没有用量信息的订阅不会出现在返回 map 中。
func (a *App) SubscriptionUserInfos() map[string]subscribe.UserInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]subscribe.UserInfo, len(a.subInfos))
	for name, info := range a.subInfos {
		out[name] = info
	}
	return out
}

// AddSubscription 添加默认启用、自动识别格式的订阅，并持久化到配置文件。
// 该兼容入口供现有 CLI 使用；需要指定类型或初始禁用时应调用 AddSubscriptionEntry。
//
// 参数：
//   - name: string，订阅名；空值由 URL 主机名自动生成。
//   - url: string，HTTP(S) 订阅地址。
//
// 返回值：
//   - config.Subscription，规范化并已提交的订阅。
//   - error，字段非法、名称/URL 重复或配置持久化失败时返回。
//
// 错误情况：持久化失败会撤销内存追加，避免 API 报错后 overview 却出现未落盘订阅。
func (a *App) AddSubscription(name, url string) (config.Subscription, error) {
	enabled := true
	return a.AddSubscriptionEntry(config.Subscription{Name: name, URL: url, Type: "auto", Enabled: &enabled})
}

// AddSubscriptionEntry 添加带类型和启用状态的完整订阅值对象。
//
// 参数：
//   - sub: config.Subscription，包含可选名称、HTTP(S) URL、auto|clash|share 类型和启用状态。
//
// 返回值：
//   - config.Subscription，补齐名称、类型和 enabled 后的已提交值。
//   - error，字段校验、唯一性校验或持久化失败时返回。
//
// 错误情况：该方法只提交配置，新增且启用也不会自动拉取；用户必须明确点击同步后，
// 节点才会进入运行态。持久化失败会删除刚追加的内存项，保持内存与磁盘一致。
func (a *App) AddSubscriptionEntry(sub config.Subscription) (config.Subscription, error) {
	sub.Name = strings.TrimSpace(sub.Name)
	sub.URL = strings.TrimSpace(sub.URL)
	sub.Type = strings.ToLower(strings.TrimSpace(sub.Type))
	if sub.Type == "" {
		sub.Type = "auto"
	}
	if sub.Enabled == nil {
		sub.Enabled = new(bool)
		*sub.Enabled = true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if sub.Name == "" {
		sub.Name = autoSubName(sub.URL, a.cfg.Subscriptions)
	}
	if err := validateSubscriptionFields(sub); err != nil {
		return config.Subscription{}, err
	}
	for _, s := range a.cfg.Subscriptions {
		if s.URL == sub.URL {
			return s, fmt.Errorf("订阅地址已存在（%s）", s.Name)
		}
	}
	for _, s := range a.cfg.Subscriptions {
		if s.Name == sub.Name {
			return config.Subscription{}, fmt.Errorf("订阅名 %q 已存在", sub.Name)
		}
	}
	a.cfg.Subscriptions = append(a.cfg.Subscriptions, sub)
	if err := a.persistLocked(); err != nil {
		a.cfg.Subscriptions = a.cfg.Subscriptions[:len(a.cfg.Subscriptions)-1]
		return config.Subscription{}, err
	}
	return sub, nil
}

// UpdateSubscription 编辑订阅名称、URL、类型和启用状态，并把策略组引用、节点运行态
// 与配置文件作为一次事务提交。该设置用例不访问订阅 URL：来源 URL/类型变化时移除旧来源
// 节点，重新启用但内存中没有旧节点时保持为空，直到用户明确执行手动同步。
//
// 参数：
//   - ctx: context.Context，保留用于兼容调用方；编辑过程不执行网络 I/O。
//   - currentName: string，要编辑的现有订阅名。
//   - next: config.Subscription，目标订阅值；Enabled 为 nil 时沿用旧状态。
//
// 返回值：
//   - config.Subscription，规范化并成功提交的目标值。
//   - error，订阅不存在、字段/唯一性非法、核心热更新、配置持久化或回滚失败时返回。
//
// 错误情况：方法持有 refreshing 锁串行化整个事务。任何提交失败都会恢复旧订阅、
// 策略组引用、节点、端口 assignments、用量信息和 mihomo 配置；组合失败不会被吞掉。
func (a *App) UpdateSubscription(_ context.Context, currentName string, next config.Subscription) (config.Subscription, error) {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	currentName = strings.TrimSpace(currentName)
	a.mu.RLock()
	index := -1
	var current config.Subscription
	for i, candidate := range a.cfg.Subscriptions {
		if candidate.Name == currentName {
			index = i
			current = candidate
			break
		}
	}
	oldSubscriptions := append([]config.Subscription(nil), a.cfg.Subscriptions...)
	oldGroups := cloneNodeGroups(a.cfg.Groups)
	oldNodes := append([]*node.Node(nil), a.nodes...)
	oldAssignments := append([]pool.Assignment(nil), a.assigns...)
	oldInfos := cloneSubscriptionInfos(a.subInfos)
	stateDir := a.cfg.StateDir
	a.mu.RUnlock()
	if index < 0 {
		return config.Subscription{}, fmt.Errorf("订阅 %q 不存在", currentName)
	}

	next.Name = strings.TrimSpace(next.Name)
	next.URL = strings.TrimSpace(next.URL)
	next.Type = strings.ToLower(strings.TrimSpace(next.Type))
	if next.Type == "" {
		next.Type = "auto"
	}
	if next.Enabled == nil {
		next.Enabled = current.Enabled
	}
	if next.PortMapping == nil {
		next.PortMapping = current.PortMapping
	}
	if err := validateSubscriptionFields(next); err != nil {
		return config.Subscription{}, err
	}
	for i, candidate := range oldSubscriptions {
		if i == index {
			continue
		}
		if candidate.Name == next.Name {
			return config.Subscription{}, fmt.Errorf("订阅名 %q 已存在", next.Name)
		}
		if candidate.URL == next.URL {
			return config.Subscription{}, fmt.Errorf("订阅地址已存在（%s）", candidate.Name)
		}
	}

	// 编辑订阅永远不隐式下载。来源未变化且原本已启用时复用现有节点及健康状态；
	// URL/类型变化后旧节点已经不能代表新来源，必须移除并等待用户手动同步。重新启用
	// 同样只复用仍在内存中的同源节点，不从磁盘缓存或网络悄悄恢复。
	// 旧配置可能缺省 type（等价 auto），比较前先归一化，避免误判来源变化。
	currentType := current.Type
	if currentType == "" {
		currentType = "auto"
	}
	sourceChanged := current.URL != next.URL || currentType != next.Type

	var freshNodes []*node.Node
	var freshInfo subscribe.UserInfo
	if next.IsEnabled() && current.IsEnabled() && !sourceChanged {
		for _, existing := range oldNodes {
			if existing == nil || existing.Subscription != current.Name {
				continue
			}
			cloned := *existing
			cloned.Subscription = next.Name
			freshNodes = append(freshNodes, &cloned)
		}
		// 用量缓存只随同一来源改名迁移；URL/类型变化后旧用量已不再对应新订阅。
		if info, ok := oldInfos[current.Name]; ok {
			freshInfo = info
		}
	}

	nextSubscriptions := append([]config.Subscription(nil), oldSubscriptions...)
	nextSubscriptions[index] = next
	nextGroups := cloneNodeGroups(oldGroups)
	if current.Name != next.Name {
		for i := range nextGroups {
			if nextGroups[i].Subscription == current.Name {
				nextGroups[i].Subscription = next.Name
			}
		}
	}
	enabledSources := map[string]bool{subscribe.ManualSubscription: true}
	for _, candidate := range nextSubscriptions {
		enabledSources[candidate.Name] = candidate.IsEnabled()
	}
	nodesBySource := make(map[string][]*node.Node, len(nextSubscriptions)+1)
	for _, existing := range oldNodes {
		if existing == nil || existing.Subscription == current.Name || !enabledSources[existing.Subscription] {
			continue
		}
		nodesBySource[existing.Subscription] = append(nodesBySource[existing.Subscription], existing)
	}
	if next.IsEnabled() {
		nodesBySource[next.Name] = freshNodes
	}
	nextNodes := subscribe.MergeFiltered(nodesBySource, a.includeRe, a.excludeRe)

	alive := make([]*node.Node, 0, len(nextNodes))
	for _, candidate := range nextNodes {
		if candidate.Alive {
			alive = append(alive, candidate)
		}
	}
	previousSnapshot, snapshotErr := pool.LoadSnapshot(a.snapshotPath())
	if snapshotErr != nil {
		log.Printf("[alloc] load snapshot for subscription update: %v (ignored)", snapshotErr)
	}
	nextAssignments := pool.Allocate(alive, a.cfg.PortRange[0], a.cfg.PortRange[1], previousSnapshot)

	a.mu.Lock()
	a.cfg.Subscriptions = nextSubscriptions
	a.cfg.Groups = nextGroups
	a.nodes = nextNodes
	a.assigns = nextAssignments
	// 先清除旧名和目标名的用量，再按“同一来源且仍启用”的条件恢复，避免 URL
	// 变化后界面继续展示旧订阅套餐信息。
	delete(a.subInfos, current.Name)
	delete(a.subInfos, next.Name)
	if next.IsEnabled() && !sourceChanged && !freshInfo.IsZero() {
		a.subInfos[next.Name] = freshInfo
	}
	a.mu.Unlock()

	if err := a.regenerateLocked(nextAssignments); err != nil {
		return config.Subscription{}, a.rollbackSubscriptionLocked(
			oldSubscriptions, oldGroups, oldNodes, oldAssignments, oldInfos, err, false,
		)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return config.Subscription{}, a.rollbackSubscriptionLocked(
			oldSubscriptions, oldGroups, oldNodes, oldAssignments, oldInfos, persistErr, true,
		)
	}
	// 同名订阅改变 URL 或解析类型后，旧缓存无法证明属于新来源。正文缓存若无法
	// 失效就回滚整个设置事务，防止下一轮健康检测把旧节点静默恢复到新订阅下。
	if sourceChanged && current.Name == next.Name {
		if err := subscribe.InvalidateCache(stateDir, current.Name); err != nil {
			return config.Subscription{}, a.rollbackSubscriptionLocked(
				oldSubscriptions, oldGroups, oldNodes, oldAssignments, oldInfos, err, true,
			)
		}
	}

	// 快照只在运行态与配置文件都提交成功后更新。这样失败回滚不会让下一次刷新
	// 误用尚未提交的端口分配；快照写失败只影响未来稳定性，不反向破坏已生效代理。
	snapshot := &pool.Snapshot{Mapping: make(map[string]int, len(nextAssignments))}
	for _, assignment := range nextAssignments {
		snapshot.Mapping[assignment.Node.Key()] = assignment.Port
	}
	if err := pool.SaveSnapshot(a.snapshotPath(), snapshot); err != nil {
		log.Printf("[alloc] save subscription update snapshot: %v", err)
	}
	if err := node.SaveSnapshot(a.nodesSnapshotPath(), nextNodes); err != nil {
		log.Printf("[snapshot] 保存订阅编辑后的节点快照失败: %v", err)
	}
	return next, nil
}

// rollbackSubscriptionLocked 恢复订阅编辑事务开始前的配置、运行态与可选磁盘状态。
// 调用方必须持有 refreshing 锁，确保回滚期间没有其它刷新穿插。
//
// 参数：
//   - subscriptions: []config.Subscription，事务前订阅快照。
//   - groups: []config.NodeGroup，事务前策略组快照。
//   - nodes: []*node.Node，事务前节点快照。
//   - assignments: []pool.Assignment，事务前稳定端口分配。
//   - infos: map[string]subscribe.UserInfo，事务前订阅用量缓存。
//   - cause: error，触发回滚的原始错误。
//   - restoreDisk: bool，是否额外把旧配置重写回磁盘。
//
// 返回值：error，始终包含 cause；运行态或磁盘恢复失败时合并返回全部错误。
//
// 错误情况：恢复失败不会静默降级，调用方会收到可诊断的组合错误。
func (a *App) rollbackSubscriptionLocked(
	subscriptions []config.Subscription,
	groups []config.NodeGroup,
	nodes []*node.Node,
	assignments []pool.Assignment,
	infos map[string]subscribe.UserInfo,
	cause error,
	restoreDisk bool,
) error {
	a.mu.Lock()
	a.cfg.Subscriptions = subscriptions
	a.cfg.Groups = groups
	a.nodes = nodes
	a.assigns = assignments
	a.subInfos = infos
	a.mu.Unlock()

	joined := cause
	if rollbackErr := a.regenerateLocked(assignments); rollbackErr != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复订阅编辑前运行态失败: %w", rollbackErr))
	}
	if restoreDisk {
		a.mu.Lock()
		rollbackErr := a.persistLocked()
		a.mu.Unlock()
		if rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复订阅编辑前配置文件失败: %w", rollbackErr))
		}
	}
	return joined
}

// validateSubscriptionFields 校验单个订阅值对象，不依赖 Config 的其它字段。
//
// 参数：
//   - sub: config.Subscription，已经完成空白与大小写规范化的候选值。
//
// 返回值：error，名称、URL 或类型非法时返回；合法时返回 nil。
//
// 错误情况：只允许 HTTP(S) URL 和 auto|clash|share 类型；名称不能为空。
func validateSubscriptionFields(sub config.Subscription) error {
	if sub.Name == "" {
		return fmt.Errorf("订阅名不能为空")
	}
	if !strings.HasPrefix(sub.URL, "http://") && !strings.HasPrefix(sub.URL, "https://") {
		return fmt.Errorf("订阅地址必须是 http(s) URL")
	}
	switch sub.Type {
	case "auto", "clash", "share":
		return nil
	default:
		return fmt.Errorf("订阅类型 %q 无效（auto|clash|share）", sub.Type)
	}
}

// cloneSubscriptionInfos 复制订阅用量状态 map，供事务失败时完整恢复。
//
// 参数：
//   - infos: map[string]subscribe.UserInfo，当前应用层用量状态。
//
// 返回值：map[string]subscribe.UserInfo，可独立增删的副本。
//
// 错误情况：无；nil 输入返回空 map，保持后续写入安全。
func cloneSubscriptionInfos(infos map[string]subscribe.UserInfo) map[string]subscribe.UserInfo {
	out := make(map[string]subscribe.UserInfo, len(infos))
	for name, info := range infos {
		out[name] = info
	}
	return out
}

// RemoveSubscription 按名字删除订阅并持久化；不允许删除最后一个订阅。
func (a *App) RemoveSubscription(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, s := range a.cfg.Subscriptions {
		if s.Name == name {
			if len(a.cfg.Subscriptions) == 1 && len(a.cfg.ManualNodes) == 0 {
				return fmt.Errorf("不能删除最后一个订阅")
			}
			a.cfg.Subscriptions = append(a.cfg.Subscriptions[:i], a.cfg.Subscriptions[i+1:]...)
			return a.persistLocked()
		}
	}
	return fmt.Errorf("订阅 %q 不存在", name)
}

// autoSubName 根据 URL 主机名生成不冲突的订阅名。
func autoSubName(url string, existing []config.Subscription) string {
	base := "sub"
	if u, err := urlpkg.Parse(url); err == nil && u.Hostname() != "" {
		base = u.Hostname()
	}
	taken := map[string]bool{}
	for _, s := range existing {
		taken[s.Name] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		if name := fmt.Sprintf("%s-%d", base, i); !taken[name] {
			return name
		}
	}
}

// RefreshSubscription 只刷新单个订阅：重新拉取该订阅并检测新节点，提交时与
// 其它来源的最新节点重新合并，再执行端口分配与热更新。
//
// 并发模型：拉取与测速只持有按订阅名的操作锁（同一订阅串行，不同订阅并行），
// 且只操作本次拉取的私有节点对象，不触碰共享节点；合并、热更新与快照保存等
// 提交阶段才获取 refreshing 全局锁。
func (a *App) RefreshSubscription(ctx context.Context, name string) error {
	unlock := a.lockSubOp(name)
	defer unlock()

	a.mu.RLock()
	var target *config.Subscription
	for i := range a.cfg.Subscriptions {
		if a.cfg.Subscriptions[i].Name == name {
			sub := a.cfg.Subscriptions[i]
			target = &sub
			break
		}
	}
	stateDir := a.cfg.StateDir
	a.mu.RUnlock()
	if target == nil {
		return fmt.Errorf("subscription %q not found", name)
	}
	if !target.IsEnabled() {
		return fmt.Errorf("订阅 %q 已禁用，请先启用后再刷新", name)
	}

	fresh, info, err := subscribe.FetchWithInfoOptions(ctx, *target, stateDir, a.subscriptionFetchOptions())
	if err != nil {
		var w *subscribe.FetchWarning
		if !errors.As(err, &w) {
			return err
		}
		log.Printf("[subscribe] %v", err) // 拉取失败，降级使用缓存节点
	}

	// fresh 是本次拉取的私有对象（尚未经 Merge 改名/挂接共享状态），合并前测速
	// 不读写共享节点，因此不需要 refreshing 锁，可与其它订阅的操作并行。
	a.checkNodes(ctx, fresh, a.dialerTargets()...)

	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	// 拉取/测速期间订阅可能被改名、改地址、停用或删除；提交前重新校验，
	// 否则会把已失效来源的节点重新并入运行态，或覆盖较新的编辑结果。
	if err := a.subscriptionUnchangedLocked(name, *target); err != nil {
		return err
	}

	// 其它来源沿用最新节点（提交前可能已被并发操作更新），与该订阅的新节点
	// 重新合并（Merge 按稳定身份去重、保证名称唯一；名称变化不影响端口稳定
	// 映射，后者按节点 Key 对齐快照）
	groups := map[string][]*node.Node{name: fresh}
	for _, n := range a.Nodes() {
		if n.Subscription != name {
			groups[n.Subscription] = append(groups[n.Subscription], n)
		}
	}
	nodes := subscribe.MergeFiltered(groups, a.includeRe, a.excludeRe)
	if len(nodes) == 0 {
		return fmt.Errorf("no nodes available from any subscription")
	}
	if !info.IsZero() {
		a.mu.Lock()
		a.subInfos[name] = info
		a.mu.Unlock()
	}
	return a.applyNodes(ctx, nodes)
}

// TestSubscription 只对单个订阅的现有节点做健康检测/延迟测试，
// 不重新拉取订阅；完成后把结果按节点 Key 回填到最新节点集，再重新分配端口并热更新。
//
// 并发模型：与 RefreshSubscription 相同，测速在共享节点的克隆上进行（不同订阅可
// 并行，也不与概览读取产生数据竞争），只有回填与热更新的提交阶段持有 refreshing 锁。
func (a *App) TestSubscription(ctx context.Context, name string) error {
	unlock := a.lockSubOp(name)
	defer unlock()

	a.mu.RLock()
	found := false
	enabled := false
	var target config.Subscription
	for _, subscription := range a.cfg.Subscriptions {
		if subscription.Name == name {
			found = true
			enabled = subscription.IsEnabled()
			target = subscription
			break
		}
	}
	a.mu.RUnlock()
	if !found {
		return fmt.Errorf("订阅 %q 不存在", name)
	}
	if !enabled {
		return fmt.Errorf("订阅 %q 已禁用，请先启用后再测速", name)
	}

	// 克隆待测节点：pool.Check 会原地写回 Alive/Delay/FailReason，直接检测共享
	// 节点会与并发的概览读取、其它订阅提交产生数据竞争。提交时按 Key 回填结果。
	nodes := a.Nodes()
	var checkList []*node.Node
	for _, n := range nodes {
		if n.Subscription == name {
			cloned := *n
			checkList = append(checkList, &cloned)
		}
	}
	if len(checkList) == 0 {
		return fmt.Errorf("订阅 %s 当前没有节点", name)
	}
	a.checkNodes(ctx, checkList, a.dialerTargets()...)

	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	if err := a.subscriptionUnchangedLocked(name, target); err != nil {
		return err
	}
	latest := a.Nodes()
	byKey := make(map[string]*node.Node, len(latest))
	for _, n := range latest {
		byKey[n.Key()] = n
	}
	for _, checked := range checkList {
		current := byKey[checked.Key()]
		if current == nil {
			continue // 测速期间节点已被其它操作移除或替换
		}
		current.Alive = checked.Alive
		current.Delay = checked.Delay
		current.FailReason = checked.FailReason
	}
	return a.applyNodes(ctx, latest)
}

// lockSubOp 获取指定订阅的操作锁，使同一订阅的刷新/测速串行执行。
//
// 参数：
//   - name: string，订阅名称。
//
// 返回值：func()，释放函数，调用方应 defer 调用。
//
// 错误情况：无；锁表按需懒创建，不校验订阅是否存在。
func (a *App) lockSubOp(name string) func() {
	a.subOpMu.Lock()
	if a.subOps == nil {
		a.subOps = make(map[string]*sync.Mutex)
	}
	mu := a.subOps[name]
	if mu == nil {
		mu = &sync.Mutex{}
		a.subOps[name] = mu
	}
	a.subOpMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// subscriptionUnchangedLocked 校验订阅在慢速阶段（拉取/测速）期间未被修改；
// 调用方必须持有 refreshing 锁。
//
// 参数：
//   - name: string，操作开始时的订阅名。
//   - target: config.Subscription，操作开始时读取的订阅快照。
//
// 返回值：error，订阅被改名、改地址、改类型、停用或删除时返回，提交方应丢弃本次结果。
//
// 错误情况：无并发副作用；只读校验。
func (a *App) subscriptionUnchangedLocked(name string, target config.Subscription) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, subscription := range a.cfg.Subscriptions {
		if subscription.Name != name {
			continue
		}
		switch {
		case !subscription.IsEnabled():
			return fmt.Errorf("订阅 %q 已停用，本次结果已丢弃", name)
		case subscription.URL != target.URL || subscription.Type != target.Type:
			return fmt.Errorf("订阅 %q 在操作期间被修改，本次结果已丢弃，请重试", name)
		}
		return nil
	}
	return fmt.Errorf("订阅 %q 已不存在，本次结果已丢弃", name)
}

// filterEnabledSubscriptionNodes 按订阅启用状态过滤运行节点，同时始终保留手动节点。
//
// 参数：
//   - nodes: []*node.Node，当前内存节点快照。
//   - subscriptions: []config.Subscription，当前订阅配置快照。
//
// 返回值：[]*node.Node，只包含启用订阅和 manual 来源的节点；保持原顺序。
//
// 错误情况：无；来源不存在于配置中的陈旧节点会被过滤，避免删除订阅后继续监听。
func filterEnabledSubscriptionNodes(nodes []*node.Node, subscriptions []config.Subscription) []*node.Node {
	enabled := map[string]bool{subscribe.ManualSubscription: true}
	for _, subscription := range subscriptions {
		enabled[subscription.Name] = subscription.IsEnabled()
	}
	out := make([]*node.Node, 0, len(nodes))
	for _, candidate := range nodes {
		if candidate != nil && enabled[candidate.Subscription] {
			out = append(out, candidate)
		}
	}
	return out
}

// ManualNodeEntry 是手动节点列表的展示项（供 API 返回）。
type ManualNodeEntry struct {
	Index int            `json:"index"`
	URL   string         `json:"url,omitempty"`   // 字符串条目（代理 URL/分享链接）
	Type  string         `json:"type,omitempty"`  // 结构化条目的出站协议（tailscale/openvpn/...）
	Name  string         `json:"name"`            // 解析出的节点名（fragment/name 字段/兜底），解析失败为空
	Proxy map[string]any `json:"proxy,omitempty"` // 结构化 VPN 出站映射（凭据字段已打码）
}

// ManualNodes 返回配置中的手动节点列表（供 API 展示）。
// 结构化条目的出站映射经 config.RedactMapping 打码，完整凭据不进入列表响应。
func (a *App) ManualNodes() []ManualNodeEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]ManualNodeEntry, 0, len(a.cfg.ManualNodes))
	for i, entry := range a.cfg.ManualNodes {
		item := ManualNodeEntry{Index: i, Name: subscribe.ManualNodeName(entry)}
		switch typed := entry.(type) {
		case string:
			item.URL = typed
		case map[string]any:
			item.Type, _ = typed["type"].(string)
			item.Proxy = config.RedactMapping(typed)
		}
		out = append(out, item)
	}
	return out
}

// AddManualNode 添加手动节点并持久化；name 非空且 URL 无 fragment 时附加为节点名。
// 重复 URL 被拒绝。调用方负责随后触发 Refresh。
func (a *App) AddManualNode(rawURL, name string) (ManualNodeEntry, error) {
	rawURL = strings.TrimSpace(rawURL)
	name = strings.TrimSpace(name)
	if rawURL == "" {
		return ManualNodeEntry{}, fmt.Errorf("节点 URL 不能为空")
	}
	if _, err := subscribe.ParseManualNode(rawURL); err != nil {
		return ManualNodeEntry{}, fmt.Errorf("节点 URL 解析失败: %w", err)
	}
	if name != "" && !strings.Contains(rawURL, "#") {
		rawURL += "#" + urlpkg.PathEscape(name)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.cfg.ManualNodes {
		if e == rawURL {
			return ManualNodeEntry{}, fmt.Errorf("节点 %q 已存在", rawURL)
		}
	}
	a.cfg.ManualNodes = append(a.cfg.ManualNodes, rawURL)
	if err := a.persistLocked(); err != nil {
		return ManualNodeEntry{}, err
	}
	return ManualNodeEntry{Index: len(a.cfg.ManualNodes) - 1, URL: rawURL, Name: subscribe.ManualNodeName(rawURL)}, nil
}

// AddManualProxy 添加结构化隧道类（VPN）手动节点并持久化。
// name 非空时覆盖映射中的 name 字段；name 为空时要求映射自带 name。
// 映射按隧道类型白名单与必填凭据校验（subscribe.ParseManualNode），其余字段透传。
// 与既有手动节点同名（解析后的节点名冲突）会被拒绝。调用方负责随后触发 Refresh。
func (a *App) AddManualProxy(mapping map[string]any, name string) (ManualNodeEntry, error) {
	name = strings.TrimSpace(name)
	candidate := make(map[string]any, len(mapping)+1)
	for k, v := range mapping {
		candidate[k] = v
	}
	if name != "" {
		candidate["name"] = name
	}
	parsed, err := subscribe.ParseManualNode(candidate)
	if err != nil {
		return ManualNodeEntry{}, fmt.Errorf("结构化节点校验失败: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.cfg.ManualNodes {
		if subscribe.ManualNodeName(e) == parsed.Name {
			return ManualNodeEntry{}, fmt.Errorf("节点 %q 已存在", parsed.Name)
		}
	}
	a.cfg.ManualNodes = append(a.cfg.ManualNodes, candidate)
	if err := a.persistLocked(); err != nil {
		return ManualNodeEntry{}, err
	}
	typ, _ := candidate["type"].(string)
	return ManualNodeEntry{Index: len(a.cfg.ManualNodes) - 1, Name: parsed.Name, Type: typ}, nil
}

// RemoveManualNode 按下标删除手动节点并持久化。调用方负责随后触发 Refresh。
func (a *App) RemoveManualNode(index int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if index < 0 || index >= len(a.cfg.ManualNodes) {
		return fmt.Errorf("手动节点下标 %d 不存在", index)
	}
	if len(a.cfg.ManualNodes) == 1 && len(a.cfg.Subscriptions) == 0 {
		return fmt.Errorf("不能删除最后一个节点来源（已无订阅）")
	}
	a.cfg.ManualNodes = append(a.cfg.ManualNodes[:index], a.cfg.ManualNodes[index+1:]...)
	return a.persistLocked()
}
