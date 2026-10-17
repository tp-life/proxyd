// Package routing 定义「订阅自带路由元信息」在各层之间传递的值对象。
//
// 这些类型必须位于独立叶子包：订阅解析（internal/proxy/subscribe）产出它们，
// 配置生成（internal/proxy/core）与编排层（internal/app）消费它们，而这三层之间
// 不能形成 import 环。
//
// 包内只描述数据与合并规则，不包含解析、命名冲突消解或 mihomo 语义判断——那些
// 分别属于 subscribe 与 core。
package routing

import "sort"

// ProxyGroupName 是 proxyd 内置的主出口选择组名（规则模式 MATCH 兜底与「默认出口」
// 的落点）。订阅路由导入用它作为策略组的兜底出口。
const ProxyGroupName = "PROXY"

// Group 是一个可直接写入 mihomo proxy-groups 的策略组。
//
// Name 已经过全局命名空间去重，Proxies 中的成员名也已完成重写，因此调用方可以
// 原样写入配置。Proxies 的顺序是语义的一部分：mihomo 的 select 组在没有
// default-selected 时落到成员首位（见 adapter/outboundgroup 的 selectedProxy）。
type Group struct {
	// Name 是最终策略组名。
	Name string `json:"name"`
	// Type 是 mihomo 分组类型（select/url-test/fallback/load-balance 等）。
	Type string `json:"type"`
	// Proxies 是最终成员名，顺序即订阅作者声明的优先级；应用了默认出口兜底时，
	// 内置 PROXY 组排在首位。
	Proxies []string `json:"proxies"`
}

// Provider 是一个规则集引用：http 类型由 mihomo 负责下载与缓存，inline 类型内联在
// 配置里直接生效。
//
// Behavior 是导入时必须显式给出的字段；Path 是 proxyd 在状态目录下为远程规则集指定的
// 绝对缓存路径；Payload 是 inline 规则集的内联内容；Config 承载剩余高级字段
// （如 format、proxy），由调用方补齐后写入 rule-providers。
type Provider struct {
	// Name 是最终 rule-provider 键名。
	Name string `json:"name"`
	// Behavior 是规则集类型：domain|ipcidr|classical。
	Behavior string `json:"behavior"`
	// Type 是规则集来源类型：http（默认）或 inline。
	Type string `json:"type,omitempty"`
	// URL 是 http 规则集的下载地址。
	URL string `json:"url,omitempty"`
	// Path 是 http 规则集缓存的绝对路径；为空时由 mihomo 按 URL 哈希决定。
	Path string `json:"path,omitempty"`
	// Payload 是 inline 规则集的内联规则行。
	Payload []string `json:"payload,omitempty"`
	// Config 是除 name/behavior/type/url/path/payload/interval 之外的透传字段。
	Config map[string]any `json:"config,omitempty"`
}

// IsInline 返回该规则集是否为内联内容。
func (p Provider) IsInline() bool { return p.Type == "inline" }

// GroupOverride 是用户在一个导入策略组上的覆盖项：手选出口与追加的出口节点。
//
// 覆盖项按「原始组名」存档而不是最终组名：改名（命名冲突）是某一轮的规划结果，
// 下一轮可能因为用户删除同名分组而变回原名，用原始名存档能让用户选择跨轮保持。
type GroupOverride struct {
	// SelectedNode 是用户手选的出口节点名；空表示未选择。
	SelectedNode string `json:"selected,omitempty"`
	// AddedNodes 是用户追加到该组出口集合的节点名。
	AddedNodes []string `json:"added,omitempty"`
}

// GroupOverrides 是某个订阅的全部策略组覆盖项（存档格式）。
type GroupOverrides map[string]GroupOverride

// Requests 把存档格式转换成规划器需要的请求表。
//
// 参数：无。
// 返回值：map[string]GroupRequest，可直接传给 subscribe.ContributionRequest.GroupRequests；
// 接收者为 nil 或空时返回 nil。
//
// 错误情况：无；切片字段做浅拷贝，避免调用方后续修改影响存档副本。
func (o GroupOverrides) Requests() map[string]GroupRequest {
	if len(o) == 0 {
		return nil
	}
	out := make(map[string]GroupRequest, len(o))
	for name, override := range o {
		out[name] = GroupRequest{
			SelectedNode: override.SelectedNode,
			AddedNodes:   append([]string(nil), override.AddedNodes...),
		}
	}
	return out
}

// GroupRequest 是一个导入策略组上的用户覆盖项（规划器输入）。
//
// 订阅给的成员列表是只读的「订阅作者意图」；用户在这个基础上可以指定手选出口，
// 并把订阅没有列出的节点追加为该组出口。追加走成员并集而不是替换：订阅下一版新增
// 成员时不会丢掉用户的追加项，用户的追加项也不会被订阅刷新抹掉。
type GroupRequest struct {
	// SelectedNode 是用户为该组手选的出口节点名；空字符串表示未选择。
	SelectedNode string
	// AddedNodes 是用户追加到该组出口集合的节点名。
	AddedNodes []string
}

// Plan 是一次配置生成要写入的所有订阅路由内容。
type Plan struct {
	// Groups 是全部订阅贡献的策略组，按加入顺序排列。
	Groups []Group
	// Rules 是全部订阅贡献的规则行，按加入顺序排列。
	Rules []string
	// FallbackRules 是订阅原文里兜底规则（MATCH/FINAL）改写后的结果：它们必须写在
	// 整份规则列表的最后，由生成阶段追加在内置规则之后。
	//
	// 不能与 Rules 混在一起：mihomo 按顺序匹配，兜底规则一旦落在中间，它之后的规则
	// （用户自定义规则、广告拦截、内置 CN 直连等）就永远不会命中。
	FallbackRules []string
	// Providers 是全部订阅贡献的规则集引用，按加入顺序排列。
	Providers []Provider
}

// IsZero 判断计划是否为空（没有任何订阅贡献内容）。
func (p Plan) IsZero() bool {
	return len(p.Groups) == 0 && len(p.Rules) == 0 && len(p.FallbackRules) == 0 && len(p.Providers) == 0
}

// GroupCount 返回计划中的策略组数量。
func (p Plan) GroupCount() int { return len(p.Groups) }

// RuleCount 返回计划中的规则条数（不含后置的兜底规则）。
func (p Plan) RuleCount() int { return len(p.Rules) }

// FallbackRuleCount 返回计划中后置的兜底规则条数。
func (p Plan) FallbackRuleCount() int { return len(p.FallbackRules) }

// ProviderCount 返回计划中的规则集数量。
func (p Plan) ProviderCount() int { return len(p.Providers) }

// MergeOrdered 按给定顺序拼接多个订阅计划。
//
// 参数：parts 为按期望顺序排列的订阅计划（调用方通常按配置文件中的订阅顺序传入）。
//
// 返回值：Plan，依次拼接各组、规则、后置兜底规则与 provider。
//
// 错误情况：无。与 Merge 的区别是不做订阅名排序：配置生成需要与用户可见的订阅顺序
// 保持一致，同时仍然是确定性的（相同配置产生逐字节相同的输出）。
func MergeOrdered(parts []Plan) Plan {
	var merged Plan
	for _, part := range parts {
		merged.Groups = append(merged.Groups, part.Groups...)
		merged.Rules = append(merged.Rules, part.Rules...)
		merged.FallbackRules = append(merged.FallbackRules, part.FallbackRules...)
		merged.Providers = append(merged.Providers, part.Providers...)
	}
	return merged
}

// Merge 把多个订阅贡献合并为一份计划。
//
// 参数：
//   - parts: map[string]Plan，订阅名到该订阅贡献的映射。
//
// 返回值：Plan，按订阅名排序后依次拼接各组、规则、后置兜底规则与 provider，保证相同
// 输入产生逐字节相同的配置（这是热更新「配置未变化就跳过」快路径的前提）。
//
// 错误情况：无。组名与 provider 名在生成贡献时就已经全局去重，合并只做拼接，
// 因此不会产生重名。
func Merge(parts map[string]Plan) Plan {
	names := make([]string, 0, len(parts))
	for name := range parts {
		names = append(names, name)
	}
	sort.Strings(names)

	ordered := make([]Plan, 0, len(names))
	for _, name := range names {
		ordered = append(ordered, parts[name])
	}
	return MergeOrdered(ordered)
}
