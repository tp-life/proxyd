package core

import (
	"path/filepath"
	"strings"
	"testing"

	C "github.com/metacubex/mihomo/constant"

	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/routing"
)

// TestGenerateWithSubscriptionRouting 验证订阅自带策略组、规则与规则集被写入配置，
// 且顺序满足「用户自定义规则优先、订阅规则优先于广告拦截与内置兜底」。
func TestGenerateWithSubscriptionRouting(t *testing.T) {
	// mihomo 只接受 home 目录（proxyd 用 state-dir 作为 home）之下的规则集缓存路径，
	// 因此测试也必须把 home 指向临时目录，与运行时行为保持一致。
	stateDir := t.TempDir()
	C.SetHomeDir(stateDir)

	cfg := fakeConfig()
	cfg.CustomRules = []string{"DOMAIN-SUFFIX,internal.example,DIRECT"}
	// 用一条不含 MATCH 的内置规则，让「订阅兜底规则必须排在最后」可被无歧义断言。
	cfg.Rules = []string{"DOMAIN-SUFFIX,tail.example,DIRECT"}
	plan := routing.Plan{
		Groups: []routing.Group{
			{Name: "AI策略", Type: "select", Proxies: []string{"node-a", "DIRECT"}},
		},
		Rules:         []string{"RULE-SET,AI,AI策略", "DOMAIN-SUFFIX,example.com,AI策略"},
		FallbackRules: []string{"MATCH,PROXY"},
		Providers: []routing.Provider{
			{Name: "AI", Behavior: "classical", URL: "https://example.com/ai.yaml", Path: filepath.Join(stateDir, "ruleset", "sub", "AI.yaml")},
		},
	}
	n := fakeSubNode("node-a", "sub", 1)
	nodeA := n
	buf, err := GenerateWithRouting(cfg, []Assignment{{Port: 42001, Node: nodeA}}, []*node.Node{nodeA}, nil, nil, plan)
	if err != nil {
		t.Fatalf("GenerateWithRouting 失败: %v", err)
	}
	m := parseYAML(t, buf)

	groups, ok := m["proxy-groups"].([]any)
	if !ok || len(groups) != 3 {
		t.Fatalf("应生成 PROXY/AUTO/订阅组 三组: %#v", m["proxy-groups"])
	}
	last, _ := groups[2].(map[string]any)
	if last["name"] != "AI策略" || last["type"] != "select" {
		t.Errorf("订阅组未追加在内置组之后: %#v", last)
	}
	if _, ok := last["default-selected"]; ok {
		t.Errorf("未提供选中项时不应写 default-selected: %#v", last)
	}

	rules, _ := m["rules"].([]any)
	var flat []string
	for _, rule := range rules {
		flat = append(flat, rule.(string))
	}
	customIndex := indexOf(flat, "DOMAIN-SUFFIX,internal.example,DIRECT")
	subIndex := indexOf(flat, "RULE-SET,AI,AI策略")
	tailIndex := indexOf(flat, "DOMAIN-SUFFIX,tail.example,DIRECT")
	if customIndex < 0 || subIndex < 0 || customIndex > subIndex {
		t.Fatalf("规则顺序应为「自定义规则 → 订阅规则」: %v", flat)
	}
	if tailIndex < 0 || tailIndex < subIndex {
		t.Fatalf("规则顺序应为「订阅规则 → 内置规则」: %v", flat)
	}
	// 订阅原文的兜底规则必须排在整份列表最后：留在中间会遮蔽它之后的内置规则、
	// 广告拦截与自定义规则（mihomo 按顺序匹配，命中 MATCH 后不再往下看）。
	if len(flat) == 0 || flat[len(flat)-1] != "MATCH,PROXY" {
		t.Fatalf("兜底规则应位于规则列表末尾: %v", flat)
	}

	providers, ok := m["rule-providers"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 rule-providers 段: %#v", m["rule-providers"])
	}
	entry, ok := providers["AI"].(map[string]any)
	if !ok {
		t.Fatalf("缺少订阅规则集 AI: %#v", providers)
	}
	if entry["behavior"] != "classical" || entry["type"] != "http" {
		t.Errorf("规则集字段不正确: %#v", entry)
	}
	if entry["path"] != filepath.Join(stateDir, "ruleset", "sub", "AI.yaml") {
		t.Errorf("规则集路径未使用绝对路径: %#v", entry["path"])
	}
	if entry["interval"] != 86400 {
		t.Errorf("规则集刷新周期应为 86400: %#v", entry["interval"])
	}
}

// TestGenerateRejectsDuplicateSubscriptionGroup 验证绕过规划器直接构造重名计划时
// 生成阶段直接报错，而不是写出 mihomo 无法加载的配置。
func TestGenerateRejectsDuplicateSubscriptionGroup(t *testing.T) {
	cfg := fakeConfig()
	plan := routing.Plan{
		Groups: []routing.Group{{Name: "PROXY", Type: "select", Proxies: []string{"node-a"}}},
	}
	nodeA := fakeSubNode("node-a", "sub", 1)
	_, err := GenerateWithRouting(cfg, []Assignment{{Port: 42001, Node: nodeA}}, []*node.Node{nodeA}, nil, nil, plan)
	if err == nil {
		t.Fatal("与内置组重名的订阅组必须被拒绝")
	}
	if !strings.Contains(err.Error(), "重名") {
		t.Errorf("错误信息应说明重名: %v", err)
	}
}

// TestGenerateRejectsDuplicateProviderKey 验证订阅规则集与既有键名冲突时报错。
func TestGenerateRejectsDuplicateProviderKey(t *testing.T) {
	cfg := fakeConfig()
	cfg.RuleProviders = map[string]any{"AI": map[string]any{"type": "http"}}
	plan := routing.Plan{
		Groups:    []routing.Group{{Name: "AI策略", Type: "select", Proxies: []string{"node-a"}}},
		Providers: []routing.Provider{{Name: "AI", Behavior: "domain", URL: "https://example.com/x.yaml"}},
	}
	nodeA := fakeSubNode("node-a", "sub", 1)
	_, err := GenerateWithRouting(cfg, []Assignment{{Port: 42001, Node: nodeA}}, []*node.Node{nodeA}, nil, nil, plan)
	if err == nil {
		t.Fatal("规则集键名冲突必须被拒绝")
	}
}

// TestGenerateWithoutRoutingMatchesLegacy 验证零值计划与旧接口逐字节一致，
// 保证默认关闭该功能时不会改变任何既有行为。
func TestGenerateWithoutRoutingMatchesLegacy(t *testing.T) {
	cfg := fakeConfig()
	nodeA := fakeSubNode("node-a", "sub", 1)
	assigns := []Assignment{{Port: 42001, Node: nodeA}}
	legacy, err := GenerateWithState(cfg, assigns, []*node.Node{nodeA}, []string{"DOMAIN,extra.example,DIRECT"}, nil)
	if err != nil {
		t.Fatalf("GenerateWithState 失败: %v", err)
	}
	withEmptyPlan, err := GenerateWithRouting(cfg, assigns, []*node.Node{nodeA}, []string{"DOMAIN,extra.example,DIRECT"}, nil, routing.Plan{})
	if err != nil {
		t.Fatalf("GenerateWithRouting 失败: %v", err)
	}
	if string(legacy) != string(withEmptyPlan) {
		t.Fatal("零值计划必须与旧接口生成完全相同的配置")
	}
}

// TestGenerateRecordsRuleSources 验证生成阶段记录每条规则的来源标注，且标注顺序与
// 真正写入 mihomo 的 rules 完全一致——控制台「当前生效规则」直接消费这份标注。
func TestGenerateRecordsRuleSources(t *testing.T) {
	stateDir := t.TempDir()
	C.SetHomeDir(stateDir)

	cfg := fakeConfig()
	cfg.CustomRules = []string{"DOMAIN-SUFFIX,internal.example,DIRECT"}
	cfg.Rules = []string{"DOMAIN-SUFFIX,tail.example,DIRECT"}
	cfg.AdBlock.Enable = true
	plan := routing.Plan{
		Groups:        []routing.Group{{Name: "AI策略", Type: "select", Proxies: []string{"node-a", "DIRECT"}}},
		Rules:         []string{"DOMAIN-SUFFIX,example.com,AI策略"},
		FallbackRules: []string{"MATCH,PROXY"},
	}
	nodeA := fakeSubNode("node-a", "sub", 1)
	built, err := BuildWithRouting(cfg, []Assignment{{Port: 42001, Node: nodeA}}, []*node.Node{nodeA}, []string{"DOMAIN,extra.example,DIRECT"}, nil, plan)
	if err != nil {
		t.Fatalf("BuildWithRouting 失败: %v", err)
	}
	if _, err := built.Validate(); err != nil {
		t.Fatalf("配置自检失败: %v", err)
	}
	entries := built.Rules()

	m := parseYAML(t, built.YAML())
	rules, _ := m["rules"].([]any)
	if len(rules) != len(entries) {
		t.Fatalf("来源标注条数应与配置一致: 配置 %d 条，标注 %d 条", len(rules), len(entries))
	}
	for index, rule := range rules {
		if entries[index].Line != rule.(string) {
			t.Fatalf("第 %d 条规则顺序不一致: 配置 %q，标注 %q", index, rule, entries[index].Line)
		}
	}
	// 顺序断言：自定义 → 远程规则源 → 订阅 → 广告拦截 → 内置 → 订阅兜底。
	wantSources := []RuleSource{
		RuleSourceCustom,
		RuleSourceRuleURL,
		RuleSourceSubscription,
		RuleSourceAdBlock,
		RuleSourceBuiltin,
		RuleSourceSubscriptionFallback,
	}
	if len(entries) != len(wantSources) {
		t.Fatalf("用例规则条数应为 %d: %#v", len(wantSources), entries)
	}
	for index, want := range wantSources {
		if entries[index].Source != want {
			t.Errorf("第 %d 条来源应为 %q，实际 %q", index, want, entries[index].Source)
		}
	}
	if entries[len(entries)-1].Line != "MATCH,PROXY" {
		t.Errorf("兜底规则应带订阅兜底来源并位于末尾: %#v", entries)
	}
}

// TestStripGeoRuleEntriesMatchesStripGeoRules 验证 GEO 降级时规则原文与来源标注用同一
// 判据剔除，避免界面展示的规则集合与真正生效的配置不一致。
func TestStripGeoRuleEntriesMatchesStripGeoRules(t *testing.T) {
	entries := []RuleEntry{
		{Line: "GEOSITE,cn,DIRECT", Source: RuleSourceBuiltin},
		{Line: "geosite,private,DIRECT", Source: RuleSourceBuiltin},
		{Line: "GEOIP,CN,DIRECT,no-resolve", Source: RuleSourceBuiltin},
		{Line: "DOMAIN-SUFFIX,example.com,PROXY", Source: RuleSourceSubscription},
		{Line: "MATCH,PROXY", Source: RuleSourceSubscriptionFallback},
		{Line: "GEOIPX,CN,DIRECT", Source: RuleSourceCustom},
	}
	lines := ruleEntriesToLines(entries)
	keptLines := stripGeoRules(lines)
	keptEntries := stripGeoRuleEntries(entries)
	if len(keptLines) != len(keptEntries) {
		t.Fatalf("两条剔除路径长度不一致: %v / %#v", keptLines, keptEntries)
	}
	for index := range keptLines {
		if keptLines[index] != keptEntries[index].Line {
			t.Fatalf("第 %d 条剔除结果不一致: %q / %q", index, keptLines[index], keptEntries[index].Line)
		}
	}
	if len(keptEntries) != 3 {
		t.Fatalf("应只保留 3 条非 GEO 规则: %#v", keptEntries)
	}
}

func indexOf(values []string, target string) int {
	for i, value := range values {
		if value == target {
			return i
		}
	}
	return -1
}
