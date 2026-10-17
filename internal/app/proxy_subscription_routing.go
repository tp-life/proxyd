package app

// 代理域应用层：把「订阅自带路由元信息」调和进运行配置。
//
// 默认关闭：只有订阅显式开启 import-subscription-rules 时，其 proxy-groups / rules /
// rule-providers 才会参与 mihomo 配置生成。调和总是整订阅替换——每次都从正文缓存
// （或本次刚拉取的正文）重新规划，因此订阅刷新、停用、删除、改名都能自然收敛，
// 不会留下上一轮的策略组或规则集引用。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"proxyd/internal/config"
	"proxyd/internal/proxy/groupstate"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/routing"
	"proxyd/internal/proxy/subscribe"
)

// subscriptionImport 是单个订阅当前生效的路由导入结果。
type subscriptionImport struct {
	status SubscriptionImportStatus
	plan   routing.Plan
	// groupNames 是订阅原文组名到最终组名的对应表（未改名的组也会记录），
	// 用于把手选状态（按最终组名存放）下次规划时还原到正确的组上。
	groupNames map[string]string
	// planMembers 是最终组名到本轮成员集合的映射，用于判断手选值是否真的生效。
	planMembers map[string]map[string]bool
}

// SubscriptionImportStatus 是单个订阅路由导入的状态视图（供 API/UI 展示）。
//
// 只包含计数、策略组名与稳定的错误文本，不含订阅正文、节点名或任何凭据。
type SubscriptionImportStatus struct {
	// Enabled 表示该订阅是否开启了路由导入。
	Enabled bool `json:"enabled"`
	// Groups 是导入的策略组数量。
	Groups int `json:"groups"`
	// Rules 是导入的规则条数；含被后置到规则列表末尾的兜底规则。
	Rules int `json:"rules"`
	// Providers 是导入的规则集数量。
	Providers int `json:"providers"`
	// SkippedRules 是因目标策略组当前没有可用成员而被跳过的规则条数。
	SkippedRules int `json:"skipped_rules,omitempty"`
	// SkipNotes 是去重后的跳过原因，供界面直接展示。
	SkipNotes []string `json:"skip_notes,omitempty"`
	// GroupEntries 是导入的策略组明细，用于提示「该组只有 DIRECT 成员」这类会让
	// 流量绕过代理的订阅侧事实。
	GroupEntries []SubscriptionImportGroup `json:"group_entries,omitempty"`
	// Error 是本次调和失败的原因；失败时该订阅的策略组与规则不会写入配置。
	Error string `json:"error,omitempty"`
}

// SubscriptionImportGroup 是单个导入策略组的安全展示项。
type SubscriptionImportGroup struct {
	// Name 是最终写入 mihomo 的策略组名（也是手选接口使用的键）。
	Name string `json:"name"`
	// AuthoredName 是订阅原文里的组名（覆盖项存档使用的键）。
	AuthoredName string `json:"authored_name,omitempty"`
	// Type 是分组类型（select/url-test/...）。
	Type string `json:"type"`
	// Members 是成员数量。
	Members int `json:"members"`
	// ProxyMembers 是成员中「真实代理节点」的数量；为 0 表示该组当前只能直连或拒绝，
	// 命中该组的流量不会走代理。
	ProxyMembers int `json:"proxy_members"`
	// Selected 是该组当前生效的手选出口；空表示未选择（mihomo 落成员首位）。
	Selected string `json:"selected,omitempty"`
	// AddedNodes 是用户追加到该组的出口节点。
	AddedNodes []string `json:"added_nodes,omitempty"`
	// FallbackApplied 表示该组的默认出口已交给内置 PROXY 组：成员首位是 PROXY，未手选
	// 时跟随主端口的默认出口，而不是按订阅作者只写了 DIRECT 的成员列表直连。
	FallbackApplied bool `json:"fallback_applied,omitempty"`
}

// initSubscriptionImports 准备路由导入状态表。
//
// 参数：无；由 app.New 在构造 App 时调用。
// 返回值：无。
// 错误情况：无；只初始化内存 map。
func (a *App) initSubscriptionImports() {
	a.subImports = map[string]*subscriptionImport{}
}

// reconcileSubscriptionRouting 按当前订阅配置与节点集合重建全部路由导入。
//
// 参数：无。正文一律取自订阅的「已确认缓存」（state-dir/cache/<订阅名>.cache）。
//
// 返回值：无。
//
// 错误情况：单个订阅规划失败只记录状态与日志，不阻断其它订阅，也不让刷新失败——
// 路由导入是尽力而为的增强能力，失败时该订阅退回「只导入节点」的历史行为。规划失败
// 时不会写入 sidecar，因此不会留下指向缺失规则集的 RULE-SET 规则。
//
// 与正文缓存的一致性契约：proxyd 的订阅拉取路径在解析成功后才会写缓存，失败时读取
// 的就是上一次已确认正文（FetchPreviewWithInfoOptions 的缓存降级），因此缓存内容始终
// 等于「产出当前节点集合的那份正文」。所有调用点都被要求在写入新缓存之后再调和，
// 这保证内存里的分流结果与重启后从缓存恢复的结果一致。
func (a *App) reconcileSubscriptionRouting() {
	a.mu.RLock()
	subs := append([]config.Subscription(nil), a.cfg.Subscriptions...)
	cfgGroups := append([]string(nil), groupNames(a.cfg.Groups)...)
	userProviders := make([]string, 0, len(a.cfg.RuleProviders))
	for key := range a.cfg.RuleProviders {
		userProviders = append(userProviders, key)
	}
	stateDir := a.cfg.StateDir
	groupFallback := a.cfg.EffectiveGroupFallback()
	nodes := append([]*node.Node(nil), a.nodes...)
	a.mu.RUnlock()

	// 可用节点名按订阅来源分组：规划器用它过滤订阅策略组成员，避免引用不存在的代理。
	available := map[string][]string{}
	for _, candidate := range nodes {
		if candidate == nil || !candidate.Alive {
			continue
		}
		available[candidate.Subscription] = append(available[candidate.Subscription], candidate.Name)
	}

	used := subscribe.Namespace{
		Groups:    map[string]bool{},
		Providers: map[string]bool{},
	}
	for _, name := range config.ReservedPolicyNames {
		used.Groups[name] = true
	}
	// 用户分组与节点名同处 mihomo 的代理命名空间，必须一起回避。
	for _, name := range cfgGroups {
		used.Groups[name] = true
	}
	for _, candidate := range nodes {
		if candidate != nil {
			used.Groups[candidate.Name] = true
		}
	}
	used.Providers[config.AdBlockProviderName] = true
	for _, key := range userProviders {
		used.Providers[key] = true
	}

	// 全局手选状态（group-selected.json）按最终组名存放，在规划前读取一次；
	// 规划器需要的是「按订阅原文组名」的覆盖项，两者在拿到 GroupStates 后对应。
	selections, selErr := groupstate.Load(a.groupSelectedPath())
	if selErr != nil {
		log.Printf("[groupstate] %v (ignored)", selErr)
		selections = map[string]string{}
	}

	imports := make(map[string]*subscriptionImport, len(subs))
	for _, sub := range subs {
		if !sub.IsEnabled() || !sub.ImportRulesEnabled() {
			continue
		}
		item := &subscriptionImport{status: SubscriptionImportStatus{Enabled: true}}
		overrides, err := loadGroupOverrides(stateDir, sub.Name)
		if err != nil {
			// 覆盖文件读不出来时不能当作「用户没有选择」继续跑：那会把用户已经把
			// 出口切到节点的分组悄悄退回订阅作者的全 DIRECT 成员。
			item.status.Error = fmt.Sprintf("读取策略组覆盖失败: %v", err)
			imports[sub.Name] = item
			continue
		}
		body, err := os.ReadFile(subscribe.CachePath(stateDir, sub.Name))
		if err != nil {
			item.status.Error = "尚无已确认的订阅缓存，同步成功后即可导入"
			imports[sub.Name] = item
			continue
		}
		contribution, buildErr := subscribe.BuildContribution(subscribe.ContributionRequest{
			Subscription:  sub.Name,
			Body:          body,
			NodeNames:     available[sub.Name],
			GroupRequests: overridesWithSelections(overrides, selections, stateDir, sub.Name),
			GroupFallback: groupFallback,
		}, used)
		if buildErr != nil {
			item.status.Error = buildErr.Error()
			log.Printf("[subscribe] 订阅 %s 自带路由元信息导入失败，本轮仅导入节点: %v", sub.Name, buildErr)
			imports[sub.Name] = item
			continue
		}
		item.plan = routing.Plan{
			Groups:        contribution.Groups,
			Rules:         contribution.Rules,
			FallbackRules: contribution.FallbackRules,
			Providers:     withProviderPaths(stateDir, sub.Name, contribution.Providers),
		}
		item.status.Groups = len(contribution.Groups)
		// 兜底规则（原文 MATCH/FINAL）也算导入：它们被放到规则列表末尾，仍然是订阅
		// 贡献的分流内容，卡片上的总数不应该把它漏掉。
		item.status.Rules = len(contribution.Rules) + len(contribution.FallbackRules)
		item.status.Providers = len(contribution.Providers)
		item.status.SkippedRules = len(contribution.SkippedRules)
		item.status.SkipNotes = skipNotes(contribution.SkippedRules)
		item.status.GroupEntries = groupEntries(nodeSetFor(available[sub.Name]), contribution.GroupStates)
		item.groupNames = make(map[string]string, len(contribution.GroupStates))
		item.planMembers = make(map[string]map[string]bool, len(contribution.Groups))
		for _, group := range contribution.Groups {
			members := make(map[string]bool, len(group.Proxies))
			for _, member := range group.Proxies {
				members[member] = true
			}
			item.planMembers[group.Name] = members
		}
		for _, state := range contribution.GroupStates {
			// 未改名的组也要登记：手选状态按最终组名存放，缺少对应关系会让下一轮
			// 规划找不到组、手选值被静默忽略。
			item.groupNames[state.AuthoredName] = state.FinalName
		}
		imports[sub.Name] = item
	}

	a.mu.Lock()
	a.subImports = imports
	a.mu.Unlock()

	// 记录本轮「原文组名 -> 最终组名」对应表：手选状态是按最终组名存的，下一轮规划
	// 需要靠这张表把它还原到正确的组上（改名规则依赖当时的命名冲突情况）。
	for _, sub := range subs {
		item := imports[sub.Name]
		if item == nil || item.status.Error != "" {
			continue
		}
		overrides, err := loadGroupOverrides(stateDir, sub.Name)
		if err != nil {
			log.Printf("[subscribe] %v", err)
			continue
		}
		if err := a.writeGroupOverrides(sub.Name, overrides, item.groupNames); err != nil {
			log.Printf("[subscribe] %v", err)
		}
	}
}

// overridesWithSelections 把全局手选状态并入某订阅的策略组覆盖项。
//
// 参数：
//   - overrides: routing.GroupOverrides，该订阅的追加节点存档（按订阅原文组名）。
//   - selections: map[string]string，全局手选状态（按最终组名）。
//   - stateDir: string，状态目录根路径。
//   - subscription: string，订阅名。
//
// 返回值：map[string]routing.GroupRequest，供规划器使用的覆盖表。
//
// 错误情况：无。手选状态按最终组名存放，而覆盖表按订阅原文组名索引，两者的对应关系
// 由上一轮的 GroupStates 档案补齐；找不到对应关系时手选值会在本轮被忽略（下一次
// 保存选择时重新写入正确键），不会误配到其它分组。
func overridesWithSelections(overrides routing.GroupOverrides, selections map[string]string, stateDir, subscription string) map[string]routing.GroupRequest {
	requests := overrides.Requests()
	names, err := loadGroupNameMap(stateDir, subscription)
	if err != nil || len(names) == 0 {
		return requests
	}
	for authored, final := range names {
		selected := selections[final]
		if selected == "" {
			continue
		}
		if requests == nil {
			requests = map[string]routing.GroupRequest{}
		}
		request := requests[authored]
		request.SelectedNode = selected
		requests[authored] = request
	}
	return requests
}

// skipNotes 汇总被跳过规则的原因。
//
// 参数：skipped 为规划器给出的跳过项（含原文行与原因）。
//
// 返回值：[]string，按字典序去重后的原因；无跳过项时返回 nil。
//
// 错误情况：无。只返回原因不返回规则行：行里可能带上用户订阅里的具体域名/规则集名，
// 而卡片只需要说清「为什么少了规则」。
func skipNotes(skipped []subscribe.SkippedRule) []string {
	if len(skipped) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(skipped))
	notes := make([]string, 0, len(skipped))
	for _, item := range skipped {
		reason := strings.TrimSpace(item.Reason)
		if reason == "" || seen[reason] {
			continue
		}
		seen[reason] = true
		notes = append(notes, reason)
	}
	sort.Strings(notes)
	return notes
}

// nodeSetFor 把节点名列表转成集合，便于统计策略组里有多少真实代理成员。
//
// 参数：names 为节点名列表。
// 返回值：map[string]bool，节点名集合。
// 错误情况：无。
func nodeSetFor(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

// groupEntries 生成策略组展示明细，并统计每组的真实代理成员数。
//
// 参数：
//   - nodeSet: map[string]bool，该订阅当前可用节点名集合。
//   - states: []subscribe.GroupState，规划器给出的「订阅意图 + 用户覆盖」合成结果。
//
// 返回值：[]SubscriptionImportGroup，与输入同序的展示项。
//
// 错误情况：无。成员名与节点名精确比较：组间引用（指向另一个订阅组）不计入代理成员，
// 因此「ProxyMembers=0」精确表示该组只能直连或拒绝。
func groupEntries(nodeSet map[string]bool, states []subscribe.GroupState) []SubscriptionImportGroup {
	out := make([]SubscriptionImportGroup, 0, len(states))
	for _, state := range states {
		entry := SubscriptionImportGroup{
			Name:            state.FinalName,
			AuthoredName:    state.AuthoredName,
			Type:            state.Type,
			Selected:        state.Selected,
			AddedNodes:      append([]string(nil), state.AddedMembers...),
			FallbackApplied: state.FallbackApplied,
		}
		for _, member := range state.Members() {
			entry.Members++
			if nodeSet[member] {
				entry.ProxyMembers++
			}
		}
		out = append(out, entry)
	}
	return out
}

// withProviderPaths 为订阅规则集分配状态目录下的绝对缓存路径。
//
// 参数：
//   - stateDir: string，状态目录根路径。
//   - subscription: string，订阅名，用于隔离不同订阅的同名规则集。
//   - providers: []routing.Provider，规划器产出的规则集引用。
//
// 返回值：[]routing.Provider，Path 已补齐的副本。
//
// 错误情况：无。订阅原文里的相对路径（如 ./ruleset/x.yaml）依赖 mihomo 进程工作目录，
// 会被替换成 state-dir 下的绝对路径，避免不同启动方式导致缓存位置漂移或互相覆盖。
func withProviderPaths(stateDir, subscription string, providers []routing.Provider) []routing.Provider {
	out := make([]routing.Provider, 0, len(providers))
	dir := filepath.Join(stateDir, "ruleset", sanitizeStateSegment(subscription))
	for _, provider := range providers {
		copied := provider
		if !provider.IsInline() {
			// inline 规则集内容随配置内联，没有可缓存的远端文件。
			copied.Path = filepath.Join(dir, sanitizeStateSegment(provider.Name)+".yaml")
		}
		out = append(out, copied)
	}
	return out
}

// sanitizeStateSegment 把订阅名或规则集名清洗成安全的路径片段。
//
// 参数：name 为待清洗文本。
// 返回值：只含字母数字与 -_. 的片段；全部被清洗掉时返回 unnamed。
// 错误情况：无。
func sanitizeStateSegment(name string) string {
	var builder strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	out := builder.String()
	if out == "" || out == "." || out == ".." {
		return "unnamed"
	}
	return out
}

// SetSubscriptionGroupFallback 设置订阅自带策略组的兜底出口策略。
//
// 参数：
//   - mode: string，auto|proxy|off（见 config.GroupFallback* 常量）。
//
// 返回值：error，取值非法、热更新或持久化失败时返回。
//
// 错误情况：采用「改内存 → 重新生成（失败回滚）→ 落盘（失败回滚）」的事务语义；
// 兜底策略只影响订阅组是否追加内置 PROXY 成员，因此重新生成是唯一的运行态影响面。
func (a *App) SetSubscriptionGroupFallback(mode string) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	switch mode {
	case config.GroupFallbackAuto, config.GroupFallbackProxy, config.GroupFallbackOff:
	default:
		return fmt.Errorf("subscription-group-fallback 取值 %q 无效（auto|proxy|off）", mode)
	}
	a.mu.Lock()
	if a.cfg.EffectiveGroupFallback() == mode {
		a.mu.Unlock()
		return nil
	}
	old := a.cfg.SubscriptionGroupFallback
	a.cfg.SubscriptionGroupFallback = mode
	a.mu.Unlock()

	if err := a.regenerateCurrentLocked(); err != nil {
		return a.rollbackGroupFallbackLocked(old, err, false)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return a.rollbackGroupFallbackLocked(old, persistErr, true)
	}
	return nil
}

// rollbackGroupFallbackLocked 恢复兜底策略变更前的配置、运行态与可选磁盘状态。
//
// 参数：
//   - old: string，变更前的取值（空表示未配置，等价 auto）。
//   - cause: error，触发回滚的原始错误。
//   - restoreDisk: bool，true 表示需要把旧配置重写回磁盘。
//
// 返回值：error，始终包含 cause；恢复失败时合并返回全部错误。
//
// 错误情况：调用方必须持有 refreshing 锁；恢复失败不会静默，避免界面提示与运行态不一致。
func (a *App) rollbackGroupFallbackLocked(old string, cause error, restoreDisk bool) error {
	a.mu.Lock()
	a.cfg.SubscriptionGroupFallback = old
	a.mu.Unlock()

	joined := cause
	if rollbackErr := a.regenerateCurrentLocked(); rollbackErr != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复旧兜底策略运行态失败: %w", rollbackErr))
	}
	if restoreDisk {
		a.mu.Lock()
		rollbackErr := a.persistLocked()
		a.mu.Unlock()
		if rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复旧兜底策略配置失败: %w", rollbackErr))
		}
	}
	return joined
}

// SubscriptionImportStatuses 返回各订阅路由导入状态快照（供 API 展示）。
//
// 参数：无。
// 返回值：map[string]SubscriptionImportStatus，订阅名到状态的拷贝；没有导入的订阅不出现。
// 错误情况：无。
func (a *App) SubscriptionImportStatuses() map[string]SubscriptionImportStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]SubscriptionImportStatus, len(a.subImports))
	for name, item := range a.subImports {
		if item == nil {
			continue
		}
		out[name] = item.status
	}
	return out
}

// ImportedGroups 返回全部导入策略组的展示项（按订阅配置顺序）。
//
// 参数：无。
// 返回值：[]ImportedGroupEntry，含最终组名、订阅作者成员、用户追加节点与生效选中项。
//
// 错误情况：无。只读取内存中的导入结果，不访问订阅地址或磁盘。
func (a *App) ImportedGroups() []ImportedGroupEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()
	selections := map[string]string{}
	if selected, err := groupstate.Load(a.groupSelectedPath()); err == nil {
		selections = selected
	}
	var out []ImportedGroupEntry
	for _, sub := range a.cfg.Subscriptions {
		if !sub.ImportRulesEnabled() {
			continue
		}
		item := a.subImports[sub.Name]
		if item == nil || item.status.Error != "" {
			continue
		}
		for _, group := range item.status.GroupEntries {
			entry := importEntryFromStatus(sub.Name, group, item)
			entry.Selected = selections[group.Name]
			entry.SelectionActive = entry.Selected != "" && item.planMembers[group.Name][entry.Selected]
			out = append(out, entry)
		}
	}
	return out
}

// ImportedGroupEntry 是一个导入策略组的对外展示项。
type ImportedGroupEntry struct {
	// Subscription 是来源订阅名。
	Subscription string `json:"subscription"`
	// Name 是最终写入 mihomo 的策略组名，也是手选接口使用的标识。
	Name string `json:"name"`
	// AuthoredName 是订阅原文组名。
	AuthoredName string `json:"authored_name,omitempty"`
	// Type 是分组类型。
	Type string `json:"type"`
	// Members 是成员数量（订阅作者成员 + 用户追加成员）。
	Members int `json:"members"`
	// ProxyMembers 是成员中的真实代理节点数量；0 表示该组只能直连或拒绝。
	ProxyMembers int `json:"proxy_members"`
	// AddedNodes 是用户追加的出口节点。
	AddedNodes []string `json:"added_nodes,omitempty"`
	// Selected 是当前记录的手选出口。
	Selected string `json:"selected,omitempty"`
	// SelectionActive 表示该手选值确实在当前组成员里（即真的会生效）；false 说明
	// 节点已下线或已改名，mihomo 会回退到成员首位。
	SelectionActive bool `json:"selection_active,omitempty"`
	// FallbackApplied 表示该组未手选时跟随主端口默认出口（内置 PROXY 组兜底），
	// 而不是按订阅作者只写了 DIRECT 的成员列表直连。
	FallbackApplied bool `json:"fallback_applied,omitempty"`
	// SelectEnabled 表示该组类型是否支持手选出口（仅 select 组支持）。
	SelectEnabled bool `json:"select_enabled"`
}

// importForGroup 查找某个最终组名所属的订阅导入项。
//
// 参数：groupName 为最终组名。
// 返回值：订阅名与该订阅的导入项；找不到时返回空串与 nil。调用方须已持有 a.mu。
//
// 错误情况：无。
func (a *App) importForGroup(groupName string) (string, *subscriptionImport) {
	for _, sub := range a.cfg.Subscriptions {
		item := a.subImports[sub.Name]
		if item == nil || item.status.Error != "" {
			continue
		}
		for _, group := range item.status.GroupEntries {
			if group.Name == groupName {
				return sub.Name, item
			}
		}
	}
	return "", nil
}

// SetImportedGroupSelected 为订阅导入的策略组手选出口节点。
//
// 参数：
//   - groupName: string，导入策略组的最终名称（overview 的 import.group_entries[].name）。
//   - nodeName: string，"DIRECT"、"REJECT" 或一个当前可用节点名。
//
// 返回值：error，分组不存在、类型不可手选、节点不可用、覆盖存档或热更新失败时返回。
//
// 错误情况：选择写入两处状态——全局 group-selected.json（生成时写 default-selected，
// 与内置/用户分组同一机制）与该订阅的策略组覆盖存档（记录「该节点是用户添加的出口」，
// 因为订阅作者的成员列表里通常没有节点）。节点不在订阅作者成员里时会自动加入该组出口，
// 这样「订阅只给了 DIRECT，用户想让它走代理」这一最常见的场景一次点击即可完成。
// 任一步失败都会恢复旧状态并重新生成配置，不会留下半套选择。
func (a *App) SetImportedGroupSelected(groupName, nodeName string) error {
	groupName = strings.TrimSpace(groupName)
	nodeName = strings.TrimSpace(nodeName)
	if groupName == "" || nodeName == "" {
		return fmt.Errorf("分组名与出口节点不能为空")
	}
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	a.mu.RLock()
	subscription, item := a.importForGroup(groupName)
	var nodes []*node.Node
	for _, candidate := range a.nodes {
		if candidate != nil && candidate.Alive {
			nodes = append(nodes, candidate)
		}
	}
	stateDir := a.cfg.StateDir
	a.mu.RUnlock()
	if item == nil {
		return fmt.Errorf("导入策略组 %q 不存在（可能订阅已停用或导入已关闭）", groupName)
	}

	var target *ImportedGroupEntry
	for _, entry := range a.importedGroupsFromItem(subscription, item) {
		if entry.Name == groupName {
			copied := entry
			target = &copied
			break
		}
	}
	if target == nil {
		return fmt.Errorf("导入策略组 %q 不存在", groupName)
	}
	if !target.SelectEnabled {
		return fmt.Errorf("分组 %q 类型为 %s，仅 select 分组支持手动选中出口", groupName, target.Type)
	}
	alive := map[string]bool{}
	for _, candidate := range nodes {
		alive[candidate.Name] = true
	}
	// 出口取值统一成 mihomo 能识别的写法（大小写与空白容错）。
	nodeName = canonicalExit(nodeName)
	added := target.AddedNodes
	switch nodeName {
	case "DIRECT", "REJECT":
		// 内置策略始终可选，与内置 PROXY 组的语义一致。
	case "AUTO":
		// AUTO 是 proxyd 内置的 url-test 组，可以作为订阅组的出口使用；它只在存在
		// 健康节点时才会被生成，因此这里要求至少一个可用节点。
		if len(alive) == 0 {
			return fmt.Errorf("当前没有可用节点，AUTO 不可选")
		}
		if !containsString(added, nodeName) {
			added = append(added, nodeName)
		}
	default:
		if !alive[nodeName] {
			return fmt.Errorf("节点 %q 当前不可用，无法作为出口", nodeName)
		}
		if !containsString(added, nodeName) {
			added = append(added, nodeName)
		}
	}

	authored := target.AuthoredName
	if authored == "" {
		authored = groupName
	}

	// 1) 覆盖存档：只替换该分组的条目（整体读出再整体写回），保留其它分组与对应表。
	if err := a.setOverrideAdded(stateDir, subscription, authored, nodeName, added); err != nil {
		return err
	}

	// 2) 全局选中状态：与内置/用户分组共用一份存档与 default-selected 机制。
	selectedPath := a.groupSelectedPath()
	oldSelected, selErr := groupstate.Load(selectedPath)
	if selErr != nil {
		log.Printf("[groupstate] %v (ignored)", oldSelected)
		oldSelected = map[string]string{}
	}
	nextSelected := make(map[string]string, len(oldSelected)+1)
	for key, value := range oldSelected {
		nextSelected[key] = value
	}
	nextSelected[groupName] = nodeName

	if err := groupstate.Save(selectedPath, nextSelected); err != nil {
		// 选中状态没写成功时恢复覆盖存档，避免留下「添加了出口但没有选择」。
		if rollbackErr := a.setOverrideAdded(stateDir, subscription, authored, "", target.AddedNodes); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("恢复策略组覆盖存档失败: %w", rollbackErr))
		}
		return err
	}
	if err := a.regenerateCurrentLocked(); err != nil {
		// 热更新失败：把两处状态都退回旧值，并重新生成旧运行态。
		joined := fmt.Errorf("导入策略组出口热更新失败: %w", err)
		if rollbackErr := groupstate.Save(selectedPath, oldSelected); rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复旧分组选中状态失败: %w", rollbackErr))
		}
		if rollbackErr := a.setOverrideAdded(stateDir, subscription, authored, target.Selected, target.AddedNodes); rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复策略组覆盖存档失败: %w", rollbackErr))
		}
		if rollbackErr := a.regenerateCurrentLocked(); rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复旧策略组运行态失败: %w", rollbackErr))
		}
		return joined
	}
	return nil
}

// canonicalExit 把出口取值规范化成 mihomo 识别的写法。
//
// 参数：value 为界面或 API 传入的出口取值。
// 返回值：DIRECT/REJECT/AUTO 转成大写，其余原样返回。
//
// 错误情况：无。只做大小写与空白归一，不做合法性判断。
func canonicalExit(value string) string {
	trimmed := strings.TrimSpace(value)
	switch strings.ToUpper(trimmed) {
	case "DIRECT", "REJECT", "AUTO":
		return strings.ToUpper(trimmed)
	default:
		return trimmed
	}
}

// importedGroupsFromItem 把一个订阅导入项转换成展示项。
//
// 参数：
//   - subscription: string，订阅名。
//   - item: *subscriptionImport，必须非 nil 且没有导入错误。
//
// 返回值：[]ImportedGroupEntry，含当前生效选中项。
//
// 错误情况：无。调用方须已持有 a.mu（读锁）或保证运行态不再变化。
func (a *App) importedGroupsFromItem(subscription string, item *subscriptionImport) []ImportedGroupEntry {
	selections := map[string]string{}
	if selected, err := groupstate.Load(a.groupSelectedPath()); err == nil {
		selections = selected
	}
	out := make([]ImportedGroupEntry, 0, len(item.status.GroupEntries))
	for _, group := range item.status.GroupEntries {
		entry := importEntryFromStatus(subscription, group, item)
		entry.Selected = selections[group.Name]
		entry.SelectionActive = entry.Selected != "" && item.planMembers[group.Name][entry.Selected]
		out = append(out, entry)
	}
	return out
}

// importEntryFromStatus 把一条导入状态转换成对外展示项（不含手选状态）。
//
// 参数：
//   - subscription: string，订阅名。
//   - group: SubscriptionImportGroup，导入状态里的组明细。
//   - item: *subscriptionImport，该订阅的导入结果（提供最终组名与成员集合）。
//
// 返回值：ImportedGroupEntry，Selected/SelectionActive 留空由调用方补齐。
//
// 错误情况：无。
func importEntryFromStatus(subscription string, group SubscriptionImportGroup, item *subscriptionImport) ImportedGroupEntry {
	return ImportedGroupEntry{
		Subscription:    subscription,
		Name:            group.Name,
		AuthoredName:    group.AuthoredName,
		Type:            group.Type,
		Members:         group.Members,
		ProxyMembers:    group.ProxyMembers,
		AddedNodes:      append([]string(nil), group.AddedNodes...),
		FallbackApplied: group.FallbackApplied,
		SelectEnabled:   strings.EqualFold(group.Type, config.GroupTypeSelect),
	}
}

// mergedRoutingLocked 合并全部订阅的路由导入计划。
//
// 参数：无；调用方须持有 a.mu（读锁）。
//
// 返回值：routing.Plan，按配置中订阅顺序拼接的组、规则与规则集引用。
//
// 错误情况：无。规划失败的订阅不会出现在结果里，因此不会出现指向缺失规则集的
// RULE-SET 规则（那会让 mihomo 拒绝整份配置）。
func (a *App) mergedRoutingLocked() routing.Plan {
	plans := make([]routing.Plan, 0, len(a.subImports))
	for _, sub := range a.cfg.Subscriptions {
		if !sub.ImportRulesEnabled() {
			continue
		}
		item := a.subImports[sub.Name]
		if item == nil || item.status.Error != "" {
			continue
		}
		plans = append(plans, item.plan)
	}
	return routing.MergeOrdered(plans)
}

// importOverrideFile 是订阅策略组覆盖项的存档格式。
//
// 只保存用户自己的选择（手选出口与追加节点）以及「原文组名 -> 最终组名」的对应表；
// 订阅正文、节点凭据、规则内容都不落这里。
type importOverrideFile struct {
	// Overrides 是按订阅原文组名索引的覆盖项。
	Overrides routing.GroupOverrides `json:"overrides,omitempty"`
	// GroupNames 是原文组名到最终组名的对应表，用于把全局手选状态（按最终组名存放）
	// 在下一轮规划时还原到正确的组上。
	GroupNames map[string]string `json:"group_names,omitempty"`
}

// writeGroupOverrides 原子写入订阅的策略组覆盖项存档。
//
// 参数：
//   - name: string，订阅名。
//   - overrides: routing.GroupOverrides，用户覆盖项（为空时删除存档文件）。
//   - groupNames: map[string]string，原文组名到最终组名的对应表。
//
// 返回值：error，写入或清理失败时返回。
//
// 错误情况：目录创建与原子写入失败都会返回；写入失败时旧文件保持不变。空覆盖项会删除
// 文件，使「用户取消了所有覆盖」与「从未覆盖过」在磁盘上等价。
func (a *App) writeGroupOverrides(name string, overrides routing.GroupOverrides, groupNames map[string]string) error {
	path := subscribe.SubscriptionOverridesPath(a.cfg.StateDir, name)
	if len(overrides) == 0 && len(groupNames) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("清理订阅 %s 策略组覆盖存档失败: %w", name, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(importOverrideFile{Overrides: overrides, GroupNames: groupNames}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化订阅 %s 策略组覆盖失败: %w", name, err)
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

// setOverrideAdded 在存档中精确更新某个分组的手选项与追加节点。
//
// 参数：
//   - stateDir: string，状态目录根路径。
//   - subscription: string，订阅名。
//   - authored: string，订阅原文组名（存档键）。
//   - selected: string，手选出口节点名；空字符串表示清除手选项。
//   - added: []string，该组当前的追加节点。
//
// 返回值：error，读取或写入失败时返回。
//
// 错误情况：只替换目标分组、并整体读出再整体写回，因此不会覆盖其它分组的覆盖项，
// 也不会覆盖「原文组名 -> 最终组名」对应表（后者由调和过程维护）。目标分组既没有
// 手选项也没有追加节点时删除该条目，让存档不留下空覆盖。
func (a *App) setOverrideAdded(stateDir, subscription, authored, selected string, added []string) error {
	file, err := readGroupOverrideFile(stateDir, subscription)
	if err != nil {
		return err
	}
	if file.Overrides == nil {
		file.Overrides = routing.GroupOverrides{}
	}
	override := file.Overrides[authored]
	override.SelectedNode = selected
	override.AddedNodes = append([]string(nil), added...)
	if len(override.AddedNodes) == 0 {
		override.AddedNodes = nil
	}
	if override.SelectedNode == "" && len(override.AddedNodes) == 0 {
		delete(file.Overrides, authored)
	} else {
		file.Overrides[authored] = override
	}
	return a.writeGroupOverrides(subscription, file.Overrides, file.GroupNames)
}

// readGroupOverrideFile 读取订阅的策略组覆盖存档。
//
// 参数：stateDir 与 name 定位存档文件。
// 返回值：importOverrideFile，文件不存在时返回零值。
//
// 错误情况：文件存在但无法读取或解析时返回错误。调用方必须把它当作失败而不是
// 「用户没有覆盖」——否则会把用户已经切到节点的出口悄悄退回订阅作者的全 DIRECT 成员，
// 让命中该组的流量被改成直连。
func readGroupOverrideFile(stateDir, name string) (importOverrideFile, error) {
	data, err := os.ReadFile(subscribe.SubscriptionOverridesPath(stateDir, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return importOverrideFile{}, nil
		}
		return importOverrideFile{}, err
	}
	var file importOverrideFile
	if err := json.Unmarshal(data, &file); err != nil {
		return importOverrideFile{}, fmt.Errorf("解析订阅 %s 策略组覆盖失败: %w", name, err)
	}
	return file, nil
}

// loadGroupOverrides 读取订阅的覆盖项，供规划器使用。
//
// 参数：stateDir 与 name 定位存档文件。
// 返回值：routing.GroupOverrides，用户覆盖项；没有存档时返回 nil。
// 错误情况：读取或解析失败时返回错误，调用方应放弃本轮导入而不是按「无覆盖」处理。
func loadGroupOverrides(stateDir, name string) (routing.GroupOverrides, error) {
	file, err := readGroupOverrideFile(stateDir, name)
	if err != nil {
		return nil, err
	}
	return file.Overrides, nil
}

// loadGroupNameMap 读取订阅上一轮的「原文组名 -> 最终组名」对应表。
//
// 参数：stateDir 与 name 定位存档文件。
// 返回值：map[string]string，对应表；没有存档时返回 nil。
// 错误情况：读取或解析失败时返回错误，调用方按「没有对应关系」降级（仅影响手选值
// 的还原，不会影响成员与规则）。
func loadGroupNameMap(stateDir, name string) (map[string]string, error) {
	file, err := readGroupOverrideFile(stateDir, name)
	if err != nil {
		return nil, err
	}
	return file.GroupNames, nil
}

// pruneOrphanImportSidecars 清理已经没有对应订阅的策略组覆盖存档。
//
// 参数：无。
// 返回值：无；删除失败只记录日志。
// 错误情况：目录不可读时直接返回，避免启动阶段因状态目录问题失败。
func (a *App) pruneOrphanImportSidecars() {
	dir := filepath.Join(a.cfg.StateDir, "groups")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	known := map[string]bool{}
	for _, sub := range a.cfg.Subscriptions {
		known[filepath.Base(subscribe.SubscriptionOverridesPath(a.cfg.StateDir, sub.Name))] = true
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || known[name] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			log.Printf("[subscribe] 清理孤儿策略组覆盖存档 %s 失败: %v", name, err)
		}
	}
}
