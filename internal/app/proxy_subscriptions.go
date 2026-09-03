package app

// 代理域：订阅与手动节点的查询、增删改用例。

import (
	"context"
	"errors"
	"fmt"
	"log"
	urlpkg "net/url"
	"strings"

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
// 错误情况：该方法只提交配置，不拉取订阅；启用后的首次拉取由 API 异步触发。
// 持久化失败会删除刚追加的内存项，保持运行态与磁盘一致。
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
// 与配置文件作为一次事务提交。启用订阅前必须成功拉取远端内容，或通过 FetchWarning
// 明确证明存在可解析缓存；否则旧的禁用状态保持不变。
//
// 参数：
//   - ctx: context.Context，控制订阅拉取和目标节点健康检测的取消/超时。
//   - currentName: string，要编辑的现有订阅名。
//   - next: config.Subscription，目标订阅值；Enabled 为 nil 时沿用旧状态。
//
// 返回值：
//   - config.Subscription，规范化并成功提交的目标值。
//   - error，订阅不存在、字段/唯一性非法、拉取无缓存、无可用出口、核心热更新、
//     配置持久化或回滚失败时返回。
//
// 错误情况：方法持有 refreshing 锁串行化整个事务。任何提交失败都会恢复旧订阅、
// 策略组引用、节点、端口 assignments、用量信息和 mihomo 配置；组合失败不会被吞掉。
func (a *App) UpdateSubscription(ctx context.Context, currentName string, next config.Subscription) (config.Subscription, error) {
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
	stateDir := a.cfg.StateDir
	oldSubscriptions := append([]config.Subscription(nil), a.cfg.Subscriptions...)
	oldGroups := cloneNodeGroups(a.cfg.Groups)
	oldNodes := append([]*node.Node(nil), a.nodes...)
	oldAssignments := append([]pool.Assignment(nil), a.assigns...)
	oldInfos := cloneSubscriptionInfos(a.subInfos)
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

	// 来源未变化（URL/类型未改且保持启用）时无需重新拉取：复用现有节点及其健康状态，
	// 只更新订阅归属标签。改名/原样保存因此立即完成，不再阻塞在订阅网络 I/O 上。
	// 旧配置可能缺省 type（等价 auto），比较前先归一化，避免误判为来源变化。
	currentType := current.Type
	if currentType == "" {
		currentType = "auto"
	}
	sourceChanged := current.URL != next.URL || currentType != next.Type
	needFetch := next.IsEnabled() && (!current.IsEnabled() || sourceChanged)

	var freshNodes []*node.Node
	var freshInfo subscribe.UserInfo
	if needFetch {
		var fetchErr error
		freshNodes, freshInfo, fetchErr = subscribe.FetchWithInfo(ctx, next, stateDir)
		if fetchErr != nil {
			var warning *subscribe.FetchWarning
			if !errors.As(fetchErr, &warning) {
				return config.Subscription{}, fmt.Errorf("启用订阅前拉取失败且没有可用缓存: %w", fetchErr)
			}
			log.Printf("[subscribe] %v", fetchErr)
		}
		if len(freshNodes) == 0 {
			return config.Subscription{}, fmt.Errorf("订阅 %q 没有可用的节点内容，未提交启用", next.Name)
		}
	} else if next.IsEnabled() {
		for _, existing := range oldNodes {
			if existing == nil || existing.Subscription != current.Name {
				continue
			}
			cloned := *existing
			cloned.Subscription = next.Name
			freshNodes = append(freshNodes, &cloned)
		}
		// 用量缓存跟随改名迁移，避免概览页的流量/到期信息在改名后丢失
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
	if needFetch {
		checkList := make([]*node.Node, 0, len(freshNodes))
		for _, candidate := range nextNodes {
			if candidate.Subscription == next.Name {
				checkList = append(checkList, candidate)
			}
		}
		pool.Check(ctx, checkList, a.cfg.HealthURL, a.cfg.HealthTimeout.D(), 32, groupNames(nextGroups)...)
	}

	alive := make([]*node.Node, 0, len(nextNodes))
	for _, candidate := range nextNodes {
		if candidate.Alive {
			alive = append(alive, candidate)
		}
	}
	// 健康节点保护只约束真正改变来源的提交（换 URL/重新启用）；纯改名不会降低可用性，
	// 不应因为节点此刻全部失效而被拒绝。
	if needFetch && len(nextNodes) > 0 && len(alive) == 0 {
		return config.Subscription{}, fmt.Errorf("订阅设置未提交：当前没有任何健康节点可维持代理运行")
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
	if next.IsEnabled() && !freshInfo.IsZero() {
		a.subInfos[next.Name] = freshInfo
	}
	if current.Name != next.Name {
		delete(a.subInfos, current.Name)
	}
	if !next.IsEnabled() {
		delete(a.subInfos, next.Name)
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

// RefreshSubscription 只刷新单个订阅：重新拉取该订阅，与其它来源的现有节点
// 重新合并后只检测该订阅的节点，再执行端口分配与热更新。
func (a *App) RefreshSubscription(ctx context.Context, name string) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	a.mu.RLock()
	var target *config.Subscription
	for i := range a.cfg.Subscriptions {
		if a.cfg.Subscriptions[i].Name == name {
			sub := a.cfg.Subscriptions[i]
			target = &sub
			break
		}
	}
	a.mu.RUnlock()
	if target == nil {
		return fmt.Errorf("subscription %q not found", name)
	}
	if !target.IsEnabled() {
		return fmt.Errorf("订阅 %q 已禁用，请先启用后再刷新", name)
	}

	fresh, info, err := subscribe.FetchWithInfo(ctx, *target, a.cfg.StateDir)
	if err != nil {
		var w *subscribe.FetchWarning
		if !errors.As(err, &w) {
			return err
		}
		log.Printf("[subscribe] %v", err) // 拉取失败，降级使用缓存节点
	}

	// 其它来源沿用现有节点，与该订阅的新节点重新合并（Merge 按稳定身份去重、
	// 保证名称唯一；名称变化不影响端口稳定映射，后者按节点 Key 对齐快照）
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

	// 只检测该订阅的节点，其它节点沿用上次检测结果
	var checkList []*node.Node
	for _, n := range nodes {
		if n.Subscription == name {
			checkList = append(checkList, n)
		}
	}
	pool.Check(ctx, checkList, a.cfg.HealthURL, a.cfg.HealthTimeout.D(), 32, a.dialerTargets()...)
	return a.applyNodes(ctx, nodes)
}

// TestSubscription 只对单个订阅的现有节点做健康检测/延迟测试，
// 不重新拉取订阅；完成后重新分配端口并热更新。
func (a *App) TestSubscription(ctx context.Context, name string) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.RLock()
	found := false
	enabled := false
	for _, subscription := range a.cfg.Subscriptions {
		if subscription.Name == name {
			found = true
			enabled = subscription.IsEnabled()
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

	nodes := a.Nodes()
	var checkList []*node.Node
	for _, n := range nodes {
		if n.Subscription == name {
			checkList = append(checkList, n)
		}
	}
	if len(checkList) == 0 {
		return fmt.Errorf("订阅 %s 当前没有节点", name)
	}
	pool.Check(ctx, checkList, a.cfg.HealthURL, a.cfg.HealthTimeout.D(), 32, a.dialerTargets()...)
	return a.applyNodes(ctx, nodes)
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
	Index int    `json:"index"`
	URL   string `json:"url"`
	Name  string `json:"name"` // 解析出的节点名（fragment/兜底），解析失败为空
}

// ManualNodes 返回配置中的手动节点列表（供 API 展示）。
func (a *App) ManualNodes() []ManualNodeEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]ManualNodeEntry, 0, len(a.cfg.ManualNodes))
	for i, u := range a.cfg.ManualNodes {
		out = append(out, ManualNodeEntry{Index: i, URL: u, Name: subscribe.ManualNodeName(u)})
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
