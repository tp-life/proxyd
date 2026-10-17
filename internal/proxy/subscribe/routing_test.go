package subscribe

import (
	"strings"
	"testing"
)

// routingFixture 模拟一份带自有分流策略的 Clash 订阅，结构与真实机场订阅一致：
// 节点 + 策略组 + 规则 + 远程规则集。
const routingFixture = `
proxies:
  - name: "香港 01"
    type: ss
    server: hk.example.com
    port: 8388
    cipher: aes-256-gcm
    password: pass
  - name: "日本 01"
    type: trojan
    server: jp.example.com
    port: 443
    password: pass
proxy-groups:
  - name: "AI策略"
    type: select
    proxies: ["香港 01", "日本 01", DIRECT]
  - name: "社交网络"
    type: select
    proxies: ["日本 01", "AI策略"]
  - name: "Final兜底策略"
    type: select
    proxies: ["香港 01", DIRECT]
rules:
  - RULE-SET,AI,AI策略
  - DOMAIN-SUFFIX,openai.com,AI策略
  - DOMAIN-SUFFIX,twitter.com,社交网络
  - IP-CIDR,10.0.0.0/8,DIRECT,no-resolve
  - MATCH,Final兜底策略
rule-providers:
  AI:
    type: http
    behavior: classical
    url: https://example.com/ai.yaml
    path: ./ruleset/ai.yaml
    interval: 86400
`

func newNamespace(groups, providers []string) Namespace {
	ns := Namespace{Groups: map[string]bool{}, Providers: map[string]bool{}}
	for _, g := range groups {
		ns.Groups[g] = true
	}
	for _, p := range providers {
		ns.Providers[p] = true
	}
	return ns
}

func TestParseRoutingMeta(t *testing.T) {
	meta, err := ParseRoutingMeta([]byte(routingFixture))
	if err != nil {
		t.Fatalf("ParseRoutingMeta 失败: %v", err)
	}
	if meta.GroupCount() != 3 || meta.RuleCount() != 5 || meta.ProviderCount() != 1 {
		t.Fatalf("摘要计数异常: groups=%d rules=%d providers=%d",
			meta.GroupCount(), meta.RuleCount(), meta.ProviderCount())
	}
	if meta.GroupCount() > 0 && meta.Groups[0].Name != "AI策略" {
		t.Errorf("策略组顺序未保持原文: %#v", meta.Groups[0])
	}
	if len(meta.RuleProviderNames) != 1 || meta.RuleProviderNames[0] != "AI" {
		t.Errorf("provider 名异常: %#v", meta.RuleProviderNames)
	}
}

func TestParseRoutingMetaIgnoresPlainSubscription(t *testing.T) {
	body := []byte("proxies:\n  - name: n1\n    type: ss\n    server: a.example.com\n    port: 443\n")
	meta, err := ParseRoutingMeta(body)
	if err != nil {
		t.Fatalf("ParseRoutingMeta 失败: %v", err)
	}
	if !meta.IsZero() {
		t.Fatalf("无元信息的订阅应返回零值摘要: %#v", meta)
	}
}

func TestBuildContributionNoMeta(t *testing.T) {
	body := []byte("proxies:\n  - name: n1\n    type: ss\n    server: a.example.com\n    port: 443\n")
	got, err := BuildContribution(ContributionRequest{Subscription: "sub", Body: body}, newNamespace(nil, nil))
	if err != nil {
		t.Fatalf("BuildContribution 失败: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("无元信息订阅不应产生贡献: %#v", got)
	}
}

func TestBuildContributionKeepsGroupsAndRewritesFallback(t *testing.T) {
	used := newNamespace([]string{"PROXY", "AUTO", "香港 01", "日本 01"}, []string{"adblock"})
	got, err := BuildContribution(ContributionRequest{
		Subscription: "portwave",
		Body:         []byte(routingFixture),
		NodeNames:    []string{"香港 01", "日本 01"},
	}, used)
	if err != nil {
		t.Fatalf("BuildContribution 失败: %v", err)
	}
	if len(got.Groups) != 3 {
		t.Fatalf("应导入 3 个策略组: %#v", got.Groups)
	}
	byName := map[string]string{}
	for _, g := range got.Groups {
		byName[g.Name] = g.Type
		if g.Type != "select" {
			t.Errorf("组类型应保持原文: %#v", g)
		}
	}
	for _, want := range []string{"AI策略", "社交网络", "Final兜底策略"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("缺少策略组 %q: %#v", want, byName)
		}
	}
	// 组间引用必须重写；MATCH 兜底必须落到内置 PROXY，并且单独后置（不参与 Rules 顺序）。
	joined := strings.Join(got.Rules, "\n")
	if len(got.FallbackRules) != 1 || got.FallbackRules[0] != "MATCH,PROXY" {
		t.Errorf("兜底规则应单独后置并重定向到内置 PROXY: %#v", got.FallbackRules)
	}
	if strings.Contains(joined, "MATCH") {
		t.Errorf("兜底规则不应留在常规规则列表里: %s", joined)
	}
	if !strings.Contains(joined, "RULE-SET,AI,AI策略") {
		t.Errorf("RULE-SET 规则被错误改写: %s", joined)
	}
	if !strings.Contains(joined, "IP-CIDR,10.0.0.0/8,DIRECT,no-resolve") {
		t.Errorf("带附加参数的 DIRECT 规则被破坏: %s", joined)
	}
	if len(got.Providers) != 1 || got.Providers[0].Name != "AI" || got.Providers[0].Behavior != "classical" {
		t.Fatalf("provider 未正确规划: %#v", got.Providers)
	}
	if !used.Providers["AI"] {
		t.Error("规划后未登记 provider 占位")
	}
	if !got.Meta.HasDNS && !strings.Contains(routingFixture, "dns:") {
		t.Log("fixture 未包含 dns 段，HasDNS 断言跳过")
	}
}

func TestBuildContributionRenamesConflicts(t *testing.T) {
	used := newNamespace([]string{"香港 01", "日本 01", "AI策略", "PROXY", "AUTO"}, []string{"AI", "adblock"})
	got, err := BuildContribution(ContributionRequest{
		Subscription: "portwave",
		Body:         []byte(routingFixture),
		NodeNames:    []string{"香港 01", "日本 01"},
	}, used)
	if err != nil {
		t.Fatalf("BuildContribution 失败: %v", err)
	}
	names := map[string]bool{}
	for _, g := range got.Groups {
		names[g.Name] = true
	}
	if names["AI策略"] {
		t.Errorf("与用户分组同名的订阅组必须改名: %#v", names)
	}
	if len(got.Providers) != 1 || got.Providers[0].Name == "AI" {
		t.Errorf("与已占用键名冲突的 provider 必须改名: %#v", got.Providers)
	}
	// 规则目标必须跟随改名，否则 mihomo 会因 proxy not found 拒绝整份配置。
	for _, rule := range got.Rules {
		if strings.HasSuffix(rule, ",AI策略") {
			t.Errorf("规则仍指向被改名的旧组名: %s", rule)
		}
	}
	if len(got.FallbackRules) != 1 || got.FallbackRules[0] != "MATCH,PROXY" {
		t.Errorf("兜底规则应始终指向内置 PROXY: %#v", got.FallbackRules)
	}
}

func TestBuildContributionDropsUnavailableNodes(t *testing.T) {
	used := newNamespace(nil, nil)
	got, err := BuildContribution(ContributionRequest{
		Subscription: "sub",
		Body:         []byte(routingFixture),
		NodeNames:    []string{"日本 01"}, // 香港 01 不可用
	}, used)
	if err != nil {
		t.Fatalf("BuildContribution 失败: %v", err)
	}
	joined := strings.Join(got.Rules, "\n")
	if strings.Contains(joined, "香港 01") {
		t.Errorf("规则不应引用不可用节点: %s", joined)
	}
	for _, g := range got.Groups {
		for _, member := range g.Proxies {
			if member == "香港 01" {
				t.Errorf("组 %q 仍引用不可用节点", g.Name)
			}
		}
	}
}

func TestBuildContributionRejectsDanglingRuleTarget(t *testing.T) {
	body := []byte(`
proxies:
  - name: n1
    type: ss
    server: a.example.com
    port: 443
proxy-groups:
  - name: g1
    type: select
    proxies: [n1]
rules:
  - DOMAIN-SUFFIX,example.com,不存在的组
`)
	_, err := BuildContribution(ContributionRequest{
		Subscription: "sub", Body: body, NodeNames: []string{"n1"},
	}, newNamespace(nil, nil))
	if err == nil {
		t.Fatal("规则指向未声明策略组时必须拒绝导入")
	}
	if !strings.Contains(err.Error(), "不存在的组") {
		t.Errorf("错误信息应指明悬空目标: %v", err)
	}
}

func TestBuildContributionRejectsMissingProvider(t *testing.T) {
	body := []byte(`
proxies:
  - name: n1
    type: ss
    server: a.example.com
    port: 443
proxy-groups:
  - name: g1
    type: select
    proxies: [n1]
rules:
  - RULE-SET,缺失的规则集,g1
`)
	_, err := BuildContribution(ContributionRequest{
		Subscription: "sub", Body: body, NodeNames: []string{"n1"},
	}, newNamespace(nil, nil))
	if err == nil {
		t.Fatal("RULE-SET 引用不存在的规则集时必须拒绝导入")
	}
}

// TestBuildContributionKeepsFallbackWithoutGroups 验证订阅的兜底规则不依赖策略组：
// 组因为成员全部下线被丢弃时，MATCH 仍然要导入并落到内置 PROXY（它本就指向默认出口）。
func TestBuildContributionKeepsFallbackWithoutGroups(t *testing.T) {
	body := []byte(`
proxies:
  - name: n1
    type: ss
    server: a.example.com
    port: 443
proxy-groups:
  - name: g1
    type: select
    proxies: [已下线的节点]
rules:
  - MATCH,g1
`)
	got, err := BuildContribution(ContributionRequest{
		Subscription: "sub", Body: body, NodeNames: []string{"n1"},
	}, newNamespace(nil, nil))
	if err != nil {
		t.Fatalf("只有兜底规则时不应整体拒绝导入: %v", err)
	}
	if len(got.Groups) != 0 {
		t.Errorf("成员全部下线的组不应被导入: %#v", got.Groups)
	}
	if len(got.FallbackRules) != 1 || got.FallbackRules[0] != "MATCH,PROXY" {
		t.Errorf("兜底规则应落到内置 PROXY: %#v", got.FallbackRules)
	}
}

// TestBuildContributionSkipsRulesForDroppedGroups 验证指向「订阅声明过、但本轮因节点
// 过滤变空」的策略组的规则只跳过自己，不连带放弃整个订阅的规则导入。
func TestBuildContributionSkipsRulesForDroppedGroups(t *testing.T) {
	body := []byte(`
proxies:
  - name: n1
    type: ss
    server: a.example.com
    port: 443
proxy-groups:
  - name: 存活组
    type: select
    proxies: [n1]
  - name: 空组
    type: select
    proxies: [已下线的节点]
rules:
  - DOMAIN-SUFFIX,alive.example.com,存活组
  - DOMAIN-SUFFIX,gone.example.com,空组
  - DOMAIN-SUFFIX,direct.example.com,DIRECT
`)
	got, err := BuildContribution(ContributionRequest{
		Subscription: "sub", Body: body, NodeNames: []string{"n1"},
	}, newNamespace(nil, nil))
	if err != nil {
		t.Fatalf("引用空组的规则应被单条跳过而不是整订阅失败: %v", err)
	}
	joined := strings.Join(got.Rules, "\n")
	if !strings.Contains(joined, "DOMAIN-SUFFIX,alive.example.com,存活组") ||
		!strings.Contains(joined, "DOMAIN-SUFFIX,direct.example.com,DIRECT") {
		t.Errorf("其余规则必须照常导入: %s", joined)
	}
	if strings.Contains(joined, "gone.example.com") {
		t.Errorf("引用空组的规则必须被跳过: %s", joined)
	}
	if len(got.SkippedRules) != 1 || !strings.Contains(got.SkippedRules[0].Reason, "空组") {
		t.Errorf("跳过项应记录原因: %#v", got.SkippedRules)
	}
	if got.SkippedRules[0].Line != "DOMAIN-SUFFIX,gone.example.com,空组" {
		t.Errorf("跳过项应记录原文行: %#v", got.SkippedRules[0])
	}
}

// TestBuildContributionImportsRulesWithoutGroups 验证只有规则、没有策略组的订阅也能
// 导入：规则目标全是内置策略与订阅自有规则集时不依赖任何自定义分组。
func TestBuildContributionImportsRulesWithoutGroups(t *testing.T) {
	body := []byte(`
proxies:
  - name: n1
    type: ss
    server: a.example.com
    port: 443
rules:
  - RULE-SET,ads,REJECT
  - DOMAIN-SUFFIX,direct.example.com,DIRECT
rule-providers:
  ads:
    type: inline
    behavior: domain
    payload:
      - '+.ads.example.com'
`)
	got, err := BuildContribution(ContributionRequest{
		Subscription: "sub", Body: body, NodeNames: []string{"n1"},
	}, newNamespace(nil, nil))
	if err != nil {
		t.Fatalf("没有策略组的订阅应能只导入规则: %v", err)
	}
	if len(got.Groups) != 0 || len(got.Rules) != 2 || len(got.Providers) != 1 {
		t.Fatalf("规则导入结果异常: %#v", got)
	}
	if got.Rules[0] != "RULE-SET,ads,REJECT" {
		t.Errorf("规则集引用被错误改写: %#v", got.Rules)
	}
}

// TestBuildContributionNormalizesFinalRule 验证 Clash 的 FINAL 别名被归一化成 MATCH：
// mihomo 只认 MATCH，写出 FINAL 会让配置自检直接失败（unsupported rule type: FINAL），
// 连带把整个代理数据面打挂。
func TestBuildContributionNormalizesFinalRule(t *testing.T) {
	body := []byte(`
proxies:
  - name: n1
    type: ss
    server: a.example.com
    port: 443
proxy-groups:
  - name: g1
    type: select
    proxies: [n1]
rules:
  - DOMAIN-SUFFIX,example.com,g1
  - FINAL,g1
`)
	got, err := BuildContribution(ContributionRequest{
		Subscription: "sub", Body: body, NodeNames: []string{"n1"},
	}, newNamespace(nil, nil))
	if err != nil {
		t.Fatalf("BuildContribution 失败: %v", err)
	}
	if len(got.FallbackRules) != 1 || got.FallbackRules[0] != "MATCH,PROXY" {
		t.Fatalf("FINAL 应被归一化为 MATCH,PROXY: %#v", got.FallbackRules)
	}
}

// TestBuildContributionFallbackPutsProxyFirst 验证默认出口放在成员首位：mihomo 的
// select 组在未设置 default-selected 时取成员首位，写在末位等于没生效。
func TestBuildContributionFallbackPutsProxyFirst(t *testing.T) {
	body := []byte(`
proxies:
  - name: n1
    type: ss
    server: a.example.com
    port: 443
proxy-groups:
  - name: 只有直连
    type: select
    proxies: [DIRECT]
  - name: 作者有节点
    type: select
    proxies: [n1, DIRECT]
rules:
  - DOMAIN-SUFFIX,example.com,只有直连
`)
	got, err := BuildContribution(ContributionRequest{
		Subscription: "sub", Body: body, NodeNames: []string{"n1"},
	}, newNamespace(nil, nil))
	if err != nil {
		t.Fatalf("BuildContribution 失败: %v", err)
	}
	byName := map[string][]string{}
	for _, group := range got.Groups {
		byName[group.Name] = group.Proxies
	}
	if members := byName["只有直连"]; len(members) != 2 || members[0] != "PROXY" || members[1] != "DIRECT" {
		t.Errorf("只有直连成员的组应把 PROXY 放在首位: %#v", members)
	}
	if members := byName["作者有节点"]; len(members) != 2 || members[0] != "n1" {
		t.Errorf("作者已给可用节点的组不应改写: %#v", members)
	}
	for _, state := range got.GroupStates {
		want := state.AuthoredName == "只有直连"
		if state.FallbackApplied != want {
			t.Errorf("组 %q 的兜底标记异常: %v", state.AuthoredName, state.FallbackApplied)
		}
	}
}

// TestBuildContributionFallbackProxyModeRewritesAll 验证 proxy 模式改写所有订阅组。
func TestBuildContributionFallbackProxyModeRewritesAll(t *testing.T) {
	body := []byte(`
proxies:
  - name: n1
    type: ss
    server: a.example.com
    port: 443
proxy-groups:
  - name: 作者有节点
    type: select
    proxies: [n1, DIRECT]
`)
	got, err := BuildContribution(ContributionRequest{
		Subscription: "sub", Body: body, NodeNames: []string{"n1"},
		GroupFallback: "proxy",
	}, newNamespace(nil, nil))
	if err != nil {
		t.Fatalf("BuildContribution 失败: %v", err)
	}
	if len(got.Groups) != 1 || got.Groups[0].Proxies[0] != "PROXY" {
		t.Fatalf("proxy 模式应把所有组交给默认出口: %#v", got.Groups)
	}
}

func TestBuildContributionRejectsUnsupportedProvider(t *testing.T) {
	body := []byte(`
proxies:
  - name: n1
    type: ss
    server: a.example.com
    port: 443
proxy-groups:
  - name: g1
    type: select
    proxies: [n1]
rules:
  - RULE-SET,weird,g1
rule-providers:
  weird:
    type: file
    behavior: domain
    url: https://example.com/x.yaml
`)
	_, err := BuildContribution(ContributionRequest{
		Subscription: "sub", Body: body, NodeNames: []string{"n1"},
	}, newNamespace(nil, nil))
	if err == nil {
		t.Fatal("不支持的 provider 来源必须拒绝导入")
	}
}

func TestBuildContributionReservedGroupRenamed(t *testing.T) {
	body := []byte(`
proxies:
  - name: n1
    type: ss
    server: a.example.com
    port: 443
proxy-groups:
  - name: PROXY
    type: select
    proxies: [n1, DIRECT]
rules:
  - MATCH,PROXY
`)
	used := newNamespace([]string{"PROXY", "AUTO"}, nil)
	got, err := BuildContribution(ContributionRequest{
		Subscription: "portwave", Body: body, NodeNames: []string{"n1"},
	}, used)
	if err != nil {
		t.Fatalf("BuildContribution 失败: %v", err)
	}
	if len(got.Groups) != 1 || got.Groups[0].Name == "PROXY" {
		t.Fatalf("与内置组同名的订阅组必须改名: %#v", got.Groups)
	}
	// MATCH 必须落到 proxyd 的默认出口组，而不是订阅那个被改名的同名组。
	if len(got.FallbackRules) != 1 || got.FallbackRules[0] != "MATCH,PROXY" {
		t.Fatalf("兜底规则异常: %#v", got.FallbackRules)
	}
}

func TestMultipleSubscriptionsDoNotCollide(t *testing.T) {
	used := newNamespace([]string{"PROXY", "AUTO"}, []string{"adblock"})
	first, err := BuildContribution(ContributionRequest{
		Subscription: "a", Body: []byte(routingFixture), NodeNames: []string{"香港 01", "日本 01"},
	}, used)
	if err != nil {
		t.Fatalf("第一个订阅规划失败: %v", err)
	}
	second, err := BuildContribution(ContributionRequest{
		Subscription: "b", Body: []byte(routingFixture), NodeNames: []string{"香港 01", "日本 01"},
	}, used)
	if err != nil {
		t.Fatalf("第二个订阅规划失败: %v", err)
	}
	seen := map[string]bool{}
	for _, g := range append(append([]string{}, groupNameList(first)...), groupNameList(second)...) {
		if seen[g] {
			t.Fatalf("跨订阅出现重名策略组: %q", g)
		}
		seen[g] = true
	}
	if first.Providers[0].Name == second.Providers[0].Name {
		t.Fatalf("跨订阅 provider 键名冲突: %q", first.Providers[0].Name)
	}
}

func groupNameList(c Contribution) []string {
	out := make([]string, 0, len(c.Groups))
	for _, g := range c.Groups {
		out = append(out, g.Name)
	}
	return out
}
