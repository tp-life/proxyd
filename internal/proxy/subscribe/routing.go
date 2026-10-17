package subscribe

// 订阅自带路由元信息（proxy-groups / rules / rule-providers）的解析与导入规划。
//
// Clash 订阅的顶层结构里，proxies 之外的段落承载了订阅作者的分流策略。proxyd 默认
// 只取 proxies（见 ParseClash），本文件负责在用户显式开启「导入订阅规则」后，把
// 这些段落翻译成 mihomo 能接受的策略组、规则与规则集引用。
//
// 这里只做纯计算：不读写文件、不访问网络、不依赖 App 状态。命名冲突与悬空引用
// 一律 fail-closed——mihomo 对规则目标与重名组是硬校验（proxy not found /
// the duplicate name），放过一条错误引用会让整份配置解析失败、代理数据面起不来，
// 因此宁可放弃该订阅的路由导入。

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"proxyd/internal/proxy/routing"
)

// rule-provider 的 behavior 取值：mihomo 只支持这三种。
const (
	behaviorDomain    = "domain"
	behaviorIPCIDR    = "ipcidr"
	behaviorClassical = "classical"
)

// providerBehaviors 是 mihomo 支持的 rule-provider behavior 取值集合。
var providerBehaviors = map[string]bool{
	behaviorDomain:    true,
	behaviorIPCIDR:    true,
	behaviorClassical: true,
}

// defaultPolicies 是无需由策略组定义即可被 mihomo 解析的内置策略名。
//
// 规则目标命中这里的名字时不参与悬空检查。
var defaultPolicies = map[string]bool{
	"DIRECT":      true,
	"REJECT":      true,
	"REJECT-DROP": true,
	"PASS":        true,
	"COMPATIBLE":  true,
	"PROXY":       true,
	"AUTO":        true,
	"GLOBAL":      true,
}

// ruleParams 是 mihomo 规则尾部可选参数段的固定集合。
//
// mihomo 用 ParseRulePayload 解析规则行：目标策略是「去掉尾部参数后的那一段」，
// 而不是行尾。IP-CIDR,10.0.0.0/8,DIRECT,no-resolve 的目标是 DIRECT，no-resolve
// 是参数；把最后一个逗号段当目标会把它误判成策略名并拒绝整条规则。
var ruleParams = map[string]bool{
	"no-resolve": true,
	"src":        true,
}

// IsRuleParam 返回某段文本是否是 mihomo 规则行的尾部参数。
//
// 参数：part 为规则行按逗号切分出的某一段（已去空白）。
// 返回值：属于 no-resolve/src 参数时返回 true。
// 错误情况：无。用于定位规则目标，不参与规则语义校验。
func IsRuleParam(part string) bool { return ruleParams[part] }

// groupNameReserved 返回订阅策略组名是否与 proxyd 内置策略组重名。
//
// 参数：name 为订阅声明的策略组名。
// 返回值：与内置 PROXY/AUTO 同名（忽略大小写）时返回 true。
// 错误情况：无。
func groupNameReserved(name string) bool {
	return strings.EqualFold(name, "PROXY") || strings.EqualFold(name, "AUTO")
}

// proxyGroup 是订阅声明的策略组（成员顺序保持原文）。
type proxyGroup struct {
	Name    string
	Type    string
	Members []string
}

// RoutingMeta 是订阅正文里除节点之外的路由元信息摘要。
//
// 它是只读值对象，只包含展示所需的事实与计数，不含任何凭据；供概览接口与界面
// 说明「这份订阅自带多少规则/策略组」。
type RoutingMeta struct {
	// Groups 是订阅声明的策略组。
	Groups []GroupMeta `json:"groups,omitempty"`
	// Rules 是订阅声明的规则行原文。
	Rules []string `json:"rules,omitempty"`
	// RuleProviderNames 是订阅声明的 rule-provider 名（已排序）。
	RuleProviderNames []string `json:"rule_provider_names,omitempty"`
	// HasDNS 表示订阅是否声明了 dns 段；proxyd 不导入订阅 dns，仅用于界面提示。
	HasDNS bool `json:"has_dns,omitempty"`
	// HasProxyProviders 表示订阅是否依赖 proxy-providers 动态节点源。
	HasProxyProviders bool `json:"has_proxy_providers,omitempty"`
}

// GroupMeta 是订阅策略组的安全展示项。
type GroupMeta struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Members []string `json:"members,omitempty"`
}

// RuleCount 返回订阅声明的规则条数。
func (m RoutingMeta) RuleCount() int { return len(m.Rules) }

// GroupCount 返回订阅声明的策略组数量。
func (m RoutingMeta) GroupCount() int { return len(m.Groups) }

// ProviderCount 返回订阅声明的 rule-provider 数量。
func (m RoutingMeta) ProviderCount() int { return len(m.RuleProviderNames) }

// IsZero 判断订阅是否完全没有自带路由元信息。
func (m RoutingMeta) IsZero() bool {
	return m.GroupCount() == 0 && m.RuleCount() == 0 && m.ProviderCount() == 0
}

// routingSource 是一次解析得到的订阅路由原文。
type routingSource struct {
	meta      RoutingMeta
	groups    []proxyGroup
	rules     []string
	providers map[string]map[string]any
}

// parseRoutingSource 解析订阅正文里的路由元信息。
//
// 参数：body 为订阅正文原始字节（Clash YAML）。
// 返回值：
//   - routingSource：策略组、规则、provider 配置与展示摘要。
//   - error：YAML 无法解析时返回。
//
// 错误情况：类型不符的单条条目（例如 proxy-groups 里混入标量）被跳过而不报错，
// 因为订阅转换器经常产出类型不一致的条目，跳过单条坏数据比整体拒绝更符合预期；
// 真正危险的引用问题在 BuildContribution 阶段判定。Groups 成员顺序保持原文。
func parseRoutingSource(body []byte) (routingSource, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return routingSource{}, fmt.Errorf("解析订阅路由元信息失败: %w", err)
	}
	source := routingSource{providers: map[string]map[string]any{}}
	if doc == nil {
		return source, nil
	}
	for _, raw := range anySlice(doc["proxy-groups"]) {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(anyString(m["name"]))
		if name == "" {
			continue
		}
		group := proxyGroup{
			Name:    name,
			Type:    strings.TrimSpace(anyString(m["type"])),
			Members: anyStringSlice(m["proxies"]),
		}
		source.groups = append(source.groups, group)
		source.meta.Groups = append(source.meta.Groups, GroupMeta{
			Name: group.Name, Type: group.Type, Members: group.Members,
		})
	}
	for _, raw := range anySlice(doc["rules"]) {
		if line := strings.TrimSpace(anyString(raw)); line != "" {
			source.rules = append(source.rules, line)
			source.meta.Rules = append(source.meta.Rules, line)
		}
	}
	if raw, ok := doc["rule-providers"].(map[string]any); ok {
		for name, value := range raw {
			source.meta.RuleProviderNames = append(source.meta.RuleProviderNames, name)
			m, ok := value.(map[string]any)
			if !ok {
				continue
			}
			copied := make(map[string]any, len(m))
			for k, v := range m {
				copied[k] = v
			}
			source.providers[name] = copied
		}
		sort.Strings(source.meta.RuleProviderNames)
	}
	if dns, ok := doc["dns"].(map[string]any); ok && len(dns) > 0 {
		source.meta.HasDNS = true
	}
	if raw, ok := doc["proxy-providers"]; ok && raw != nil {
		source.meta.HasProxyProviders = true
	}
	return source, nil
}

// ParseRoutingMeta 解析 Clash 订阅正文里的路由元信息摘要。
//
// 参数：body 为订阅正文原始字节（Clash YAML）。
// 返回值：RoutingMeta 摘要；正文不是 YAML 映射或缺少相关段落时返回零值。
// 错误情况：YAML 无法解析时返回错误，调用方必须把它当作「没有元信息」而不是
// 「没有规则」，避免把解析失败误报成订阅很干净。
func ParseRoutingMeta(body []byte) (RoutingMeta, error) {
	source, err := parseRoutingSource(body)
	if err != nil {
		return RoutingMeta{}, err
	}
	return source.meta, nil
}

// Contribution 是一个订阅对代理配置的完整贡献。
//
// 组、规则与 provider 引用都已经过命名空间去重和目标重写，可以直接写入 mihomo
// 配置；撤销时按 Subscription 整体替换。
type Contribution struct {
	// Subscription 是贡献来源的订阅名（也是撤销时的键）。
	Subscription string
	// Groups 是重写后的策略组。
	Groups []routing.Group
	// Rules 是重写后的规则行。
	Rules []string
	// Providers 是重写后的规则集引用。
	Providers []routing.Provider
	// Meta 是订阅原始元信息摘要，供界面展示。
	Meta RoutingMeta
	// Groups 中每一项的 Authored 标记成员是否来自订阅原文；同时携带该组当前生效的
	// 手选出口与用户追加节点，供控制台展示与再次编辑。
	GroupStates []GroupState
	// FallbackRules 是订阅原文里 MATCH/FINAL 兜底规则改写后的结果。它们由生成阶段
	// 追加在整份规则列表的最后，不参与 Rules 的顺序。
	FallbackRules []string
	// SkippedRules 是订阅声明过目标策略组、但该组在当前节点集合下不可用时被跳过的
	// 规则。整订阅仍然保持导入，跳过项由调用方计入状态并展示原因。
	SkippedRules []SkippedRule
}

// SkippedRule 是一条被跳过的订阅规则及其原因。
type SkippedRule struct {
	// Line 是订阅原文里的规则行。
	Line string
	// Reason 是稳定的中文原因，可直接展示给用户。
	Reason string
}

// GroupState 是一个导入策略组上「订阅意图 + 用户覆盖」的合成结果。
type GroupState struct {
	// AuthoredName 是订阅原文里的组名。
	AuthoredName string
	// FinalName 是写入 mihomo 的最终组名（可能因命名冲突被改名）。
	FinalName string
	// Type 是分组类型。
	Type string
	// AuthoredMembers 是订阅作者给出的可用成员（已按当前健康节点过滤）。
	AuthoredMembers []string
	// AddedMembers 是用户追加的可用成员。
	AddedMembers []string
	// Selected 是当前生效的手选出口；空表示未选择（mihomo 落成员首位）。
	Selected string
	// FallbackApplied 表示该组已追加内置 PROXY 组作为兜底出口（跟随主端口默认出口）。
	FallbackApplied bool
}

// Members 返回该组「订阅作者 + 用户追加」的成员列表（用户追加项在前）。
//
// 不含自动前置的默认出口 PROXY：那是导入策略，不是订阅声明的成员，界面通过
// FallbackApplied 单独展示。
func (g GroupState) Members() []string {
	out := make([]string, 0, len(g.AddedMembers)+len(g.AuthoredMembers))
	out = append(out, g.AddedMembers...)
	out = append(out, g.AuthoredMembers...)
	return out
}

// IsZero 判断该订阅是否没有任何可用贡献。
func (c Contribution) IsZero() bool {
	return len(c.Groups) == 0 && len(c.Rules) == 0 && len(c.FallbackRules) == 0 && len(c.Providers) == 0
}

// ContributionRequest 描述单个订阅参与命名空间规划所需的输入。
type ContributionRequest struct {
	// Subscription 是订阅名。
	Subscription string
	// Body 是订阅正文原始字节。
	Body []byte
	// NodeNames 是该订阅最终可用节点的名称集合（节点合并去重与改名之后）。
	//
	// 订阅组里引用的节点名若不在其中会被剔除：mihomo 会因为组内引用不存在的代理
	// 让整份配置解析失败。
	NodeNames []string
	// GroupRequests 是按原始组名给出的用户覆盖：手选出口节点与追加的出口节点。
	//
	// nil 或缺少某项表示该组沿用订阅作者的成员列表。
	GroupRequests map[string]routing.GroupRequest
	// GroupFallback 控制订阅策略组的默认出口是否改由内置 PROXY 组承担：
	//   - "off"：保持订阅作者成员列表原样；
	//   - "proxy"：所有组都把 PROXY 放在成员首位；
	//   - 其它（含空值，即默认 auto）：仅当组内没有任何真实代理节点成员时如此。
	//
	// 兜底把 PROXY 放在成员列表首位，因为 mihomo 的 select 组在没有 default-selected
	// 时落到成员首位（见 adapter/outboundgroup 的 selectedProxy）；写在末位等于没生效。
	// 手选出口（default-selected）优先级高于成员顺序，因此手动指定过出口的组不受影响。
	GroupFallback string
}

// Namespace 是一次配置生成中与策略组/provider 名冲突相关的占位集合。
//
// 所有集合都按「已占用」语义使用：规划器只读取它们，并保证不同订阅之间也不会
// 互相占用同一个最终名称。
type Namespace struct {
	// Groups 是已经占用的策略组名（内置 PROXY/AUTO、用户分组名、节点名）。
	Groups map[string]bool
	// Providers 是已经占用的 rule-provider 键名（用户手写 rule-providers 与保留名）。
	Providers map[string]bool
}

// BuildContribution 把单个订阅的路由元信息翻译成可直接写入配置的贡献。
//
// 参数：
//   - req: ContributionRequest，订阅名、正文与该订阅最终可用节点名。
//   - used: Namespace，已被占用的组名与 provider 键名；会被原地扩充，使多订阅
//     顺序规划时互不冲突。
//
// 返回值：
//   - Contribution：重写后的组、规则、后置兜底规则与 provider 引用；订阅没有元信息
//     时为零值。只有规则没有策略组的订阅（规则目标全是内置策略与订阅自有规则集）
//     同样可以导入。
//   - error：规则引用了订阅未声明的策略组/provider，或 provider 配置非法时返回错误，
//     调用方必须整体跳过该订阅的路由导入。
//
// 错误情况：基本不做部分导入。存在悬空引用时若只导入「能用的一部分」，会把订阅的
// 分流意图静默改成另一套出口，比不导入更难排查。唯一的例外是「订阅声明过、但本轮
// 因可用节点过滤而变空」的策略组：指向它的规则会记入 SkippedRules 后被跳过，其余
// 规则照常导入——组被丢空是 proxyd 侧的过滤结果，不是订阅自身的引用错误，让一个组
// 的节点全部下线连带丢掉全部规则并不合理。
func BuildContribution(req ContributionRequest, used Namespace) (Contribution, error) {
	source, err := parseRoutingSource(req.Body)
	if err != nil {
		return Contribution{}, err
	}
	if source.meta.IsZero() {
		return Contribution{}, nil
	}
	contribution := Contribution{Subscription: req.Subscription, Meta: source.meta}

	providerNames, providerEntries, err := planProviders(source.providers, used)
	if err != nil {
		return Contribution{}, err
	}
	contribution.Providers = providerEntries

	nodeSet := make(map[string]bool, len(req.NodeNames))
	for _, name := range req.NodeNames {
		if name != "" {
			nodeSet[name] = true
		}
	}

	// 第一遍：为可用的组分配最终名称。组内没有任何可用成员时跳过该组——mihomo
	// 不接受空组，而订阅组常见的「只有节点名成员」在节点被过滤后会变成空组；
	// 用户追加过出口节点的组即使订阅成员全部失效也要保留，否则用户的选择会失效。
	groupNames := make(map[string]string, len(source.groups))
	droppedGroups := map[string]bool{}
	plannedGroups := make([]proxyGroup, 0, len(source.groups))
	requestedNodes := map[string][]string{}
	for _, group := range source.groups {
		request := req.GroupRequests[group.Name]
		added := resolveAddedNodes(request.AddedNodes, nodeSet)
		requestedNodes[group.Name] = added
		if len(added) == 0 && !groupHasUsableMember(group, source.groups, nodeSet) {
			// 记下被丢弃的组名：引用它的规则会被单条跳过而不是让整订阅导入失败——
			// 组被丢弃是 proxyd 侧的节点过滤结果，不是订阅自身的引用错误。
			droppedGroups[group.Name] = true
			continue
		}
		finalName := uniqueGroupName(group.Name, req.Subscription, used.Groups)
		used.Groups[finalName] = true
		groupNames[group.Name] = finalName
		plannedGroups = append(plannedGroups, group)
	}

	// 第二遍：把成员名翻译成最终名称（节点名保持不变，组引用换成重命名后的组），
	// 并把用户追加的出口节点前置——用户显式添加的出口应当排在订阅作者列表之前，
	// 未手选时「组成员首位」才会落在用户添加的节点上。
	resolved := make([]routing.Group, 0, len(plannedGroups))
	states := make([]GroupState, 0, len(plannedGroups))
	for _, group := range plannedGroups {
		authored := resolveGroupMembers(group.Members, source.groups, groupNames, nodeSet)
		added := requestedNodes[group.Name]
		members := append(append([]string(nil), added...), authored...)
		if len(members) == 0 {
			return Contribution{}, fmt.Errorf("订阅 %s 的策略组 %q 在当前节点集合下没有可用成员", req.Subscription, group.Name)
		}
		groupType := group.Type
		if groupType == "" {
			groupType = "select"
		}
		// 兜底判定只看订阅作者给的成员：用户手选/追加的出口是覆盖层，不应该因为它
		// 存在就把默认出口摘掉——否则手选节点一旦下线，该组会退回「只剩 DIRECT」的直连行为。
		fallback := needsProxyFallback(req.GroupFallback, authored, nodeSet)
		if fallback {
			// 默认出口放在首位：mihomo 的 select 组在未选择 default-selected 时落成员
			// 首位，因此订阅作者只写了 DIRECT 的组会跟随主端口默认出口。手选出口写入
			// default-selected，优先级高于成员顺序，仍然覆盖这里。
			members = append([]string{routing.ProxyGroupName}, members...)
		}
		resolved = append(resolved, routing.Group{Name: groupNames[group.Name], Type: groupType, Proxies: members})

		state := GroupState{
			AuthoredName:    group.Name,
			FinalName:       groupNames[group.Name],
			Type:            groupType,
			AuthoredMembers: authored,
			AddedMembers:    added,
			FallbackApplied: fallback,
		}
		if selected := strings.TrimSpace(req.GroupRequests[group.Name].SelectedNode); selected != "" {
			// 只接受仍然可用的选中项：节点下线或改名后 mihomo 会静默回退成员首位，
			// 这里显式丢弃，让界面能提示「选择已失效」而不是显示一个假选中值。
			for _, member := range members {
				if member == selected {
					state.Selected = selected
					break
				}
			}
		}
		states = append(states, state)
	}
	contribution.Groups = resolved
	contribution.GroupStates = states

	for _, line := range source.rules {
		rewritten, fallback, err := rewriteRule(line, groupNames, providerNames, droppedGroups)
		if err != nil {
			// 目标组是订阅自己声明过的组、只是本轮被节点过滤丢空：跳过这一条而不是
			// 放弃整个订阅——否则一个组的节点全部下线就会连带丢掉全部规则。
			var dangling *ruleRewriteError
			if errors.As(err, &dangling) && dangling.droppedGroup {
				contribution.SkippedRules = append(contribution.SkippedRules, SkippedRule{Line: line, Reason: dangling.Error()})
				continue
			}
			return Contribution{}, fmt.Errorf("订阅 %s 的规则 %q 无法导入: %w", req.Subscription, line, err)
		}
		if fallback {
			// MATCH/FINAL 单独收集，由生成阶段放到整份规则列表的最后。
			contribution.FallbackRules = append(contribution.FallbackRules, rewritten)
			continue
		}
		contribution.Rules = append(contribution.Rules, rewritten)
	}
	return contribution, nil
}

// needsProxyFallback 判断某个订阅策略组是否要把默认出口交给内置 PROXY 组。
//
// 参数：
//   - mode: string，订阅策略组默认出口策略（off|proxy|其它=auto）。
//   - authored: []string，订阅作者给出的可用成员（已过滤，不含用户追加项）。
//   - nodeSet: map[string]bool，该订阅最终可用节点名集合。
//
// 返回值：需要把 PROXY 放在成员首位时返回 true。
//
// 错误情况：无。判定规则：
//   - off：永不改写；
//   - proxy：始终改写；
//   - auto（含空值）：订阅作者的成员里没有任何真实代理节点时改写。这样「只给了
//     DIRECT/REJECT」的组会跟随主端口默认出口，而作者已经配好节点的组保持原样。
func needsProxyFallback(mode string, authored []string, nodeSet map[string]bool) bool {
	switch mode {
	case "off":
		return false
	case "proxy":
		return true
	}
	for _, member := range authored {
		if nodeSet[member] {
			return false // 已经有真实代理节点，不需要兜底
		}
	}
	return true
}

// resolveAddedNodes 过滤用户追加的出口项。
//
// 参数：
//   - added: []string，用户为该组追加的出口（节点名或内置策略名）。
//   - nodeSet: map[string]bool，该订阅最终可用节点名集合。
//
// 返回值：[]string，保持输入顺序、去重且仍然可用的出口项。
//
// 错误情况：无。节点下线或改名后会被剔除，避免写出 mihomo 无法解析的组成员；
// AUTO/DIRECT/REJECT 等内置策略不是节点，必须单独放行——否则用户把订阅组的出口
// 交给「自动最快」的选择会被静默丢掉，命中该组的流量继续按订阅作者的直连走。
func resolveAddedNodes(added []string, nodeSet map[string]bool) []string {
	if len(added) == 0 {
		return nil
	}
	out := make([]string, 0, len(added))
	seen := map[string]bool{}
	for _, name := range added {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		if defaultPolicies[name] {
			seen[name] = true
			out = append(out, strings.ToUpper(name))
			continue
		}
		if !nodeSet[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// groupHasUsableMember 判断策略组在本轮节点集合下是否还有可用成员。
//
// 参数：
//   - group: proxyGroup，待判断的组。
//   - groups: []proxyGroup，订阅声明的全部组（用于识别组间引用）。
//   - nodeSet: map[string]bool，该订阅最终可用节点名集合。
//
// 返回值：至少有一个成员能落到「可用节点 / 订阅自有组 / 内置策略」时返回 true。
//
// 错误情况：无。
func groupHasUsableMember(group proxyGroup, groups []proxyGroup, nodeSet map[string]bool) bool {
	for _, member := range group.Members {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}
		if nodeSet[member] || declaredGroup(groups, member) {
			return true
		}
		if defaultPolicies[member] && !groupNameReserved(member) {
			return true
		}
	}
	return false
}

// declaredGroup 判断名称是否是订阅声明的策略组。
//
// 参数：groups 为订阅声明的全部组；name 为待判断名称。
// 返回值：存在同名（大小写敏感）组时返回 true。
// 错误情况：无。
func declaredGroup(groups []proxyGroup, name string) bool {
	for _, group := range groups {
		if group.Name == name {
			return true
		}
	}
	return false
}

// resolveGroupMembers 把组成员翻译成 mihomo 最终名称。
//
// 参数：
//   - members: []string，订阅声明的成员名。
//   - groups: []proxyGroup，订阅声明的全部组。
//   - groupNames: map[string]string，订阅组名到最终组名。
//   - nodeSet: map[string]bool，该订阅最终可用节点名集合。
//
// 返回值：[]string，可直接写入 mihomo 的成员名（保持原顺序、去重）。
//
// 错误情况：无；无法解析的成员被剔除——成员缺失只缩小策略组范围，不会让配置失效，
// 因此不需要像规则目标那样 fail-closed。
func resolveGroupMembers(members []string, groups []proxyGroup, groupNames map[string]string, nodeSet map[string]bool) []string {
	out := make([]string, 0, len(members))
	seen := map[string]bool{}
	for _, member := range members {
		member = strings.TrimSpace(member)
		if member == "" || seen[member] {
			continue
		}
		switch {
		case nodeSet[member]:
			seen[member] = true
			out = append(out, member)
		case defaultPolicies[member]:
			// 内置策略（含 DIRECT/REJECT 与 proxyd 的 PROXY/AUTO）原样保留：
			// 订阅里显式写 PROXY 视为引用 proxyd 的默认出口组，语义见 rewriteRule。
			seen[member] = true
			out = append(out, strings.ToUpper(member))
		default:
			if final, ok := groupNames[member]; ok {
				seen[member] = true
				out = append(out, final)
			}
		}
	}
	return out
}

// planProviders 为订阅的 rule-providers 分配最终键名。
//
// 参数：
//   - configs: map[string]map[string]any，provider 名到原始配置。
//   - used: Namespace，已占用的 provider 键名；会被原地扩充。
//
// 返回值：
//   - map[string]string：订阅原文 provider 名到最终键名。
//   - []routing.Provider：按名称排序的重写结果；path/interval 由应用层补齐，
//     因此这里不携带这两项。
//   - error：behavior 非法、url 缺失或 type 不是 http 时返回。
//
// 错误情况：任何一项无法被 mihomo 加载时返回错误，调用方应放弃该订阅的路由导入，
// 而不是生成一份缺规则集的配置（后者会让 RULE-SET 规则整体失效）。
func planProviders(configs map[string]map[string]any, used Namespace) (map[string]string, []routing.Provider, error) {
	if len(configs) == 0 {
		return map[string]string{}, nil, nil
	}
	names := make([]string, 0, len(configs))
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)

	remap := make(map[string]string, len(names))
	out := make([]routing.Provider, 0, len(names))
	for _, name := range names {
		raw := configs[name]
		behavior := strings.ToLower(strings.TrimSpace(anyString(raw["behavior"])))
		if behavior == "" {
			behavior = behaviorDomain
		}
		if !providerBehaviors[behavior] {
			return nil, nil, fmt.Errorf("规则集 %s 的 behavior %q 不支持", name, behavior)
		}
		providerType := strings.ToLower(strings.TrimSpace(anyString(raw["type"])))
		if providerType == "" {
			providerType = "http"
		}
		url := strings.TrimSpace(anyString(raw["url"]))
		var payload []string
		switch providerType {
		case "http":
			if url == "" {
				return nil, nil, fmt.Errorf("规则集 %s 缺少 url", name)
			}
		case "inline":
			// inline 规则集把内容直接内联在订阅正文里，mihomo 原生支持；机场订阅常用它
			// 承载「作者自己补充的规则」，不导入会丢掉订阅的一部分分流意图。
			payload = anyStringSlice(raw["payload"])
			if len(payload) == 0 {
				return nil, nil, fmt.Errorf("规则集 %s 是 inline 类型但没有 payload", name)
			}
		default:
			// file/其它来源依赖订阅作者本机路径，导入后必然加载失败。
			return nil, nil, fmt.Errorf("规则集 %s 的 type %q 不支持（仅支持 http 与 inline）", name, providerType)
		}
		config := make(map[string]any, len(raw))
		for k, v := range raw {
			switch k {
			case "path", "interval", "behavior", "url", "type", "format", "proxy", "payload":
				continue // 路径、刷新周期与传输细节由 proxyd 统一决定；payload 单独携带
			default:
				config[k] = v
			}
		}
		finalName := uniqueProviderName(name, used.Providers)
		used.Providers[finalName] = true
		remap[name] = finalName
		out = append(out, routing.Provider{
			Name:     finalName,
			Behavior: behavior,
			Type:     providerType,
			URL:      url,
			Payload:  payload,
			Config:   config,
		})
	}
	return remap, out, nil
}

// ruleRewriteError 是规则无法导入的原因。
//
// DroppedGroup 为 true 表示目标策略组确实由订阅声明过，只是本轮因为节点集合过滤而
// 不可用；这类错误允许调用方跳过单条规则，而不是放弃整个订阅的规则导入。
type ruleRewriteError struct {
	message      string
	droppedGroup bool
}

// Error 实现 error 接口。
func (e *ruleRewriteError) Error() string { return e.message }

// rewriteRule 重写单条规则的目标策略。
//
// 参数：
//   - line: string，订阅原文规则行。
//   - groupNames: map[string]string，订阅组名到最终组名。
//   - providerNames: map[string]string，订阅 provider 名到最终键名。
//   - droppedGroups: map[string]bool，订阅声明过、但本轮因节点过滤被丢弃的组名。
//
// 返回值：
//   - string：可写入 mihomo 的规则行。
//   - bool：该规则是否为兜底规则（MATCH/FINAL），需要由调用方后置到规则列表末尾。
//   - error：该规则无法安全导入时返回。
//
// 错误情况：
//   - 兜底规则 MATCH/FINAL（mihomo 写作两段 MATCH,策略）归一化为 MATCH 并把目标重定向
//     到 proxyd 内置 PROXY 组，让「默认出口」继续决定未命中流量，而不是被订阅兜底组
//     接管；返回值的 fallback 标记为 true，由调用方后置到规则列表末尾。
//   - RULE-SET 引用的 provider 不在订阅声明里时返回错误（mihomo 会直接拒绝配置）。
//   - 目标既不是内置策略、也不是订阅自有组时返回错误：订阅引用了外部策略（例如
//     用户手写分组），导入后语义无法确定，静默改写会造成错误分流。
//   - 目标是订阅声明过、但本轮被丢弃的组时返回规则级错误（DroppedGroup=true）。
//   - SUB-RULE 依赖独立的内联规则语法，proxyd 不导入，按错误处理。
func rewriteRule(line string, groupNames, providerNames map[string]string, droppedGroups map[string]bool) (string, bool, error) {
	parts := strings.Split(line, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if len(parts) < 2 {
		return "", false, &ruleRewriteError{message: "规则段数不足"}
	}
	ruleType := strings.ToUpper(parts[0])
	if strings.HasPrefix(ruleType, "SUB-RULE") {
		return "", false, &ruleRewriteError{message: "不支持 SUB-RULE"}
	}

	// 目标策略位置：跳过尾部参数段后的最后一段。
	targetIndex := len(parts) - 1
	for targetIndex > 1 && ruleParams[parts[targetIndex]] {
		targetIndex--
	}
	target := parts[targetIndex]
	if target == "" {
		return "", false, &ruleRewriteError{message: "缺少目标策略"}
	}

	// 兜底规则在 mihomo 里写作两段（MATCH,策略），因此长度校验放在兜底分支之后。
	// mihomo 只认 MATCH：Clash 的 FINAL 别名必须归一化，否则自检会以
	// "unsupported rule type: FINAL" 拒绝整份配置（连带把代理数据面打挂）。
	if ruleType == "MATCH" || ruleType == "FINAL" {
		parts[0] = "MATCH"
		parts[targetIndex] = routing.ProxyGroupName
		return strings.Join(parts, ","), true, nil
	}
	if targetIndex < 2 {
		return "", false, &ruleRewriteError{message: "规则段数不足"}
	}
	if ruleType == "RULE-SET" {
		// RULE-SET,<provider>,<策略>[,参数]：provider 段紧邻策略段之前。
		providerIndex := targetIndex - 1
		finalProvider, ok := providerNames[parts[providerIndex]]
		if !ok {
			return "", false, &ruleRewriteError{message: fmt.Sprintf("引用了不存在的规则集 %q", parts[providerIndex])}
		}
		parts[providerIndex] = finalProvider
	}
	if final, ok := groupNames[target]; ok {
		parts[targetIndex] = final
		return strings.Join(parts, ","), false, nil
	}
	if defaultPolicies[target] {
		parts[targetIndex] = strings.ToUpper(target)
		return strings.Join(parts, ","), false, nil
	}
	if droppedGroups[target] {
		return "", false, &ruleRewriteError{
			message:      fmt.Sprintf("目标策略组 %q 在当前节点集合下没有可用成员，已跳过该规则", target),
			droppedGroup: true,
		}
	}
	return "", false, &ruleRewriteError{message: fmt.Sprintf("目标策略 %q 不是订阅自有组或内置策略", target)}
}

// uniqueGroupName 返回策略组在全局命名空间中不冲突的最终名称。
//
// 参数：
//   - preferred: string，订阅声明的组名。
//   - subscription: string，冲突时使用的订阅名前缀。
//   - taken: map[string]bool，已占用的组名集合（含内置 PROXY/AUTO、用户分组名与节点名）。
//
// 返回值：string，直接可用的组名；与内置组同名时也会改名，避免覆盖 proxyd 语义。
//
// 错误情况：无。
func uniqueGroupName(preferred, subscription string, taken map[string]bool) string {
	if groupNameReserved(preferred) {
		// 与内置 PROXY/AUTO 同名时直接换成「订阅名 · 原名」，不沿用订阅作者的
		// 名字，避免规则目标把订阅组误解析成 proxyd 的默认出口组。
		return uniquePlanName("", subscription, taken)
	}
	return uniquePlanName(preferred, subscription, taken)
}

// uniqueProviderName 返回 rule-provider 在全局命名空间中不冲突的最终键名。
//
// 参数：
//   - preferred: string，订阅声明的 provider 名。
//   - taken: map[string]bool，已占用的 provider 键名集合（含 adblock 保留名）。
//
// 返回值：string，直接可用的键名。
//
// 错误情况：无。
func uniqueProviderName(preferred string, taken map[string]bool) string {
	return uniquePlanName(preferred, "sub", taken)
}

// uniquePlanName 返回在 taken 中不冲突的最终名称。
//
// 参数：
//   - preferred: string，期望使用的名称。
//   - scope: string，冲突时使用的命名空间前缀（订阅名或 sub）；为空时使用 sub。
//   - taken: map[string]bool，已占用名称集合。
//
// 返回值：string，直接可用的名称；避让是大小写不敏感的，因为 mihomo 的策略组名
// 比较对大小写敏感但用户很难区分，保守避让更安全。
//
// 错误情况：无。
func uniquePlanName(preferred, scope string, taken map[string]bool) string {
	preferred = strings.TrimSpace(preferred)
	isTaken := func(name string) bool {
		for existing := range taken {
			if strings.EqualFold(existing, name) {
				return true
			}
		}
		return false
	}
	base := strings.TrimSpace(scope)
	if base == "" {
		base = "sub"
	}
	if preferred != "" && !isTaken(preferred) {
		return preferred
	}
	candidate := base
	if preferred != "" {
		candidate = fmt.Sprintf("%s · %s", base, preferred)
	}
	if !isTaken(candidate) {
		return candidate
	}
	for i := 2; ; i++ {
		next := fmt.Sprintf("%s #%d", candidate, i)
		if !isTaken(next) {
			return next
		}
	}
}

// anyString 把 YAML 解码出的任意标量转成字符串。
func anyString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// anySlice 把 YAML 序列转成 []any；非序列返回 nil。
func anySlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

// anyStringSlice 把 YAML 字符串序列转成 []string，忽略空项。
func anyStringSlice(v any) []string {
	items := anySlice(v)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s := strings.TrimSpace(anyString(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}
