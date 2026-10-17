package core

// 规则来源标注：生成配置时同步记录每条规则出自哪一层，供控制台与 CLI 展示
// 「当前生效规则」。mihomo 只按顺序匹配，来源不是配置字段，因此必须在生成阶段
// 就地记录，不能事后从最终配置反推。

import "strings"

// RuleSource 标识一条进入最终配置的规则来自哪一层。
type RuleSource string

const (
	// RuleSourceGateway 是网关模块按设备生成的 SRC-IP-CIDR 分流规则。
	RuleSourceGateway RuleSource = "gateway"
	// RuleSourceCustom 是用户维护的 custom-rules。
	RuleSourceCustom RuleSource = "custom"
	// RuleSourceRuleURL 是远程规则源（rule-urls）导入的规则。
	RuleSourceRuleURL RuleSource = "rule-url"
	// RuleSourceSubscription 是订阅自带的规则（import-subscription-rules 开启时）。
	RuleSourceSubscription RuleSource = "subscription"
	// RuleSourceSubscriptionFallback 是订阅原文里 MATCH/FINAL 改写后的兜底规则，恒在最后。
	RuleSourceSubscriptionFallback RuleSource = "subscription-fallback"
	// RuleSourceAdBlock 是广告拦截插入的单条 RULE-SET 规则。
	RuleSourceAdBlock RuleSource = "adblock"
	// RuleSourceBuiltin 是 proxyd 内置规则（config.DefaultRules）。
	RuleSourceBuiltin RuleSource = "builtin"
)

// RuleSources 按实际拼接顺序返回全部规则来源。
//
// 返回值：[]RuleSource，顺序与 generate 写入 mihomo 的规则顺序一致。
//
// 错误情况：无。展示层据此给出稳定的来源清单（含零条来源），避免界面依赖出现顺序。
func RuleSources() []RuleSource {
	return []RuleSource{
		RuleSourceGateway,
		RuleSourceCustom,
		RuleSourceRuleURL,
		RuleSourceSubscription,
		RuleSourceAdBlock,
		RuleSourceBuiltin,
		RuleSourceSubscriptionFallback,
	}
}

// Label 返回来源的中文展示名。
//
// 返回值：string，界面与命令行共用的中文名；未知来源原样返回其键名。
//
// 错误情况：无。
func (s RuleSource) Label() string {
	switch s {
	case RuleSourceGateway:
		return "网关设备分流"
	case RuleSourceCustom:
		return "自定义规则"
	case RuleSourceRuleURL:
		return "远程规则源"
	case RuleSourceSubscription:
		return "订阅规则"
	case RuleSourceSubscriptionFallback:
		return "订阅兜底规则"
	case RuleSourceAdBlock:
		return "广告拦截"
	case RuleSourceBuiltin:
		return "内置规则"
	default:
		return string(s)
	}
}

// RuleEntry 是最终配置里的一条规则及其来源。
type RuleEntry struct {
	// Line 是写入 mihomo 的规则原文。
	Line string `json:"line"`
	// Source 是规则来源键。
	Source RuleSource `json:"source"`
}

// appendRuleEntries 把一批同源规则追加到来源清单。
//
// 参数：
//   - dst: []RuleEntry，已有清单。
//   - source: RuleSource，这批规则的来源。
//   - lines: []string，规则原文，顺序保持不变。
//
// 返回值：[]RuleEntry，追加后的清单。
//
// 错误情况：无；空 lines 不产生任何条目。
func appendRuleEntries(dst []RuleEntry, source RuleSource, lines []string) []RuleEntry {
	for _, line := range lines {
		dst = append(dst, RuleEntry{Line: line, Source: source})
	}
	return dst
}

// ruleEntriesToLines 抽取规则原文，供写入配置。
//
// 参数：entries 为来源清单。
// 返回值：[]string，与清单顺序一致的规则原文。
// 错误情况：无。
func ruleEntriesToLines(entries []RuleEntry) []string {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, entry.Line)
	}
	return lines
}

// isGeoRule 判断一条规则是否为 GEOSITE/GEOIP 规则（GEO 数据不可用时需要剔除）。
//
// 参数：line 为规则原文。
// 返回值：bool，规则类型为 GEOSITE 或 GEOIP 时返回 true。
// 错误情况：无。
func isGeoRule(line string) bool {
	head, _, _ := strings.Cut(line, ",")
	return strings.EqualFold(head, "GEOSITE") || strings.EqualFold(head, "GEOIP")
}

// stripGeoRuleEntries 剔除 GEOSITE/GEOIP 条目，与 stripGeoRules 共用 isGeoRule 判据。
//
// 参数：entries 为来源清单。
// 返回值：[]RuleEntry，不含 GEO 规则的新清单。
// 错误情况：无。
func stripGeoRuleEntries(entries []RuleEntry) []RuleEntry {
	out := make([]RuleEntry, 0, len(entries))
	for _, entry := range entries {
		if isGeoRule(entry.Line) {
			continue
		}
		out = append(out, entry)
	}
	return out
}
