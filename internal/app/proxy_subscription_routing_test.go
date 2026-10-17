package app

// 订阅自带路由元信息导入的应用层测试：调和、撤销、sidecar 与配置注入。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/proxy/core"
	"proxyd/internal/proxy/groupstate"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/subscribe"
)

// routingSubscriptionBody 是一份带自有分流策略的订阅正文。
const routingSubscriptionBody = `
proxies:
  - name: "香港 01"
    type: ss
    server: hk.example.com
    port: 8388
    cipher: aes-256-gcm
    password: pass
    udp: true
proxy-groups:
  - name: "AI策略"
    type: select
    proxies: ["香港 01", DIRECT]
rules:
  - DOMAIN-SUFFIX,openai.com,AI策略
  - MATCH,AI策略
rule-providers:
  OpenAI:
    type: http
    behavior: domain
    url: https://example.com/openai.yaml
    path: ./ruleset/openai.yaml
    interval: 86400
`

// newRoutingTestApp 构造一个订阅已开启路由导入、且正文缓存已写入的 App。
func newRoutingTestApp(t *testing.T, importRules bool, body string) (*App, config.Subscription) {
	t.Helper()
	stateDir := t.TempDir()
	enabled := true
	sub := config.Subscription{Name: "portwave", URL: "https://example.com/sub", Type: "clash", Enabled: &enabled}
	if importRules {
		sub.ImportRules = &enabled
	}
	cfg := &config.Config{
		Listen:        "127.0.0.1",
		Mode:          "rule",
		LogLevel:      "silent",
		StateDir:      stateDir,
		HealthURL:     "http://www.gstatic.com/generate_204",
		HealthTimeout: config.Duration(3 * time.Second),
		Rules:         []string{"MATCH,PROXY"},
		Subscriptions: []config.Subscription{sub},
	}
	a, err := New(cfg, "")
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.runner.Shutdown)

	cacheDir := filepath.Join(stateDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("创建缓存目录失败: %v", err)
	}
	if err := os.WriteFile(subscribe.CachePath(stateDir, sub.Name), []byte(body), 0o600); err != nil {
		t.Fatalf("写入订阅缓存失败: %v", err)
	}
	return a, sub
}

// publishAliveNode 把一个健康节点放入运行态，供路由规划解析策略组成员。
func publishAliveNode(t *testing.T, a *App, name, sub string) {
	t.Helper()
	candidate := &node.Node{
		Name:         name,
		Subscription: sub,
		Mapping: map[string]any{
			"name": name, "type": "socks5", "server": "127.0.0.1", "port": 1080,
		},
	}
	candidate.Alive = true
	a.mu.Lock()
	a.nodes = []*node.Node{candidate}
	a.mu.Unlock()
}

func TestReconcileSubscriptionRoutingImportsMeta(t *testing.T) {
	a, sub := newRoutingTestApp(t, true, routingSubscriptionBody)
	publishAliveNode(t, a, "香港 01", sub.Name)

	a.reconcileSubscriptionRouting()

	statuses := a.SubscriptionImportStatuses()
	status, ok := statuses[sub.Name]
	if !ok {
		t.Fatalf("缺少订阅导入状态: %#v", statuses)
	}
	if status.Error != "" {
		t.Fatalf("调和不应失败: %s", status.Error)
	}
	if status.Groups != 1 || status.Rules != 2 || status.Providers != 1 {
		t.Fatalf("导入计数异常: %#v", status)
	}
	if len(status.GroupEntries) != 1 || status.GroupEntries[0].ProxyMembers != 1 {
		t.Fatalf("策略组明细应统计真实代理成员: %#v", status.GroupEntries)
	}

	a.mu.RLock()
	plan := a.mergedRoutingLocked()
	a.mu.RUnlock()
	if plan.GroupCount() != 1 || plan.RuleCount() != 1 || plan.FallbackRuleCount() != 1 || plan.ProviderCount() != 1 {
		t.Fatalf("合并计划异常: %#v", plan)
	}
	if plan.Rules[0] != "DOMAIN-SUFFIX,openai.com,AI策略" {
		t.Errorf("常规规则顺序异常: %#v", plan.Rules)
	}
	// 兜底规则单独后置：留在 Rules 里会遮蔽它之后的自定义规则、广告拦截与内置规则。
	if plan.FallbackRules[0] != "MATCH,PROXY" {
		t.Errorf("兜底规则必须指向内置 PROXY: %#v", plan.FallbackRules)
	}
	if plan.Providers[0].Path == "" || !strings.HasPrefix(plan.Providers[0].Path, a.cfg.StateDir) {
		t.Errorf("规则集缓存路径必须在 state-dir 下: %#v", plan.Providers[0])
	}
	// 覆盖存档登记「原文组名 -> 最终组名」：手选状态按最终组名存放，缺了这张表
	// 下一轮规划就无法把手选值还原到组上。
	names, err := loadGroupNameMap(a.cfg.StateDir, sub.Name)
	if err != nil {
		t.Fatalf("读取组名对应表失败: %v", err)
	}
	if names["AI策略"] != plan.Groups[0].Name {
		t.Fatalf("组名对应表异常: %#v", names)
	}
}

func TestReconcileSubscriptionRoutingDisabledRemovesEverything(t *testing.T) {
	a, sub := newRoutingTestApp(t, true, routingSubscriptionBody)
	publishAliveNode(t, a, "香港 01", sub.Name)
	a.reconcileSubscriptionRouting()
	if names, err := loadGroupNameMap(a.cfg.StateDir, sub.Name); err != nil || len(names) == 0 {
		t.Fatalf("开启导入后应写入组名对应表: %#v (%v)", names, err)
	}

	// 关闭订阅级开关：整订阅撤销，运行态导入结果一并清理。
	a.mu.Lock()
	a.cfg.Subscriptions[0].ImportRules = nil
	a.mu.Unlock()
	a.reconcileSubscriptionRouting()

	if _, ok := a.SubscriptionImportStatuses()[sub.Name]; ok {
		t.Error("关闭导入后不应保留导入状态")
	}
	a.mu.RLock()
	plan := a.mergedRoutingLocked()
	a.mu.RUnlock()
	if !plan.IsZero() {
		t.Errorf("关闭导入后不应有合并计划: %#v", plan)
	}
}

func TestReconcileSubscriptionRoutingWithoutCacheFailsClosed(t *testing.T) {
	a, sub := newRoutingTestApp(t, true, routingSubscriptionBody)
	publishAliveNode(t, a, "香港 01", sub.Name)
	if err := os.Remove(subscribe.CachePath(a.cfg.StateDir, sub.Name)); err != nil {
		t.Fatalf("删除缓存失败: %v", err)
	}
	a.reconcileSubscriptionRouting()

	status := a.SubscriptionImportStatuses()[sub.Name]
	if status.Error == "" {
		t.Fatal("没有正文缓存时必须记录失败状态")
	}
	a.mu.RLock()
	plan := a.mergedRoutingLocked()
	a.mu.RUnlock()
	if !plan.IsZero() {
		t.Errorf("失败订阅不应产生计划: %#v", plan)
	}
}

func TestReconcileSubscriptionRoutingRenamesConflicts(t *testing.T) {
	a, sub := newRoutingTestApp(t, true, routingSubscriptionBody)
	publishAliveNode(t, a, "香港 01", sub.Name)
	// 用户已有同名分组：订阅组必须改名，规则目标同步跟随。
	a.mu.Lock()
	a.cfg.Groups = []config.NodeGroup{{Name: "AI策略", Port: 42050, Subscription: sub.Name}}
	a.mu.Unlock()

	a.reconcileSubscriptionRouting()
	a.mu.RLock()
	plan := a.mergedRoutingLocked()
	a.mu.RUnlock()
	if plan.GroupCount() != 1 || plan.Groups[0].Name == "AI策略" {
		t.Fatalf("与用户分组同名的订阅组必须改名: %#v", plan.Groups)
	}
	for _, rule := range plan.Rules {
		if strings.HasSuffix(rule, ",AI策略") {
			t.Errorf("规则仍指向旧组名: %s", rule)
		}
	}
}

func TestReconcileSubscriptionRoutingRejectsDanglingTarget(t *testing.T) {
	body := strings.Replace(routingSubscriptionBody, "DOMAIN-SUFFIX,openai.com,AI策略", "DOMAIN-SUFFIX,openai.com,不存在的组", 1)
	a, sub := newRoutingTestApp(t, true, body)
	publishAliveNode(t, a, "香港 01", sub.Name)

	a.reconcileSubscriptionRouting()
	status := a.SubscriptionImportStatuses()[sub.Name]
	if status.Error == "" {
		t.Fatal("悬空规则目标必须记录失败状态")
	}
	if !strings.Contains(status.Error, "不存在的组") {
		t.Errorf("错误信息应指明悬空目标: %s", status.Error)
	}
	a.mu.RLock()
	plan := a.mergedRoutingLocked()
	a.mu.RUnlock()
	if !plan.IsZero() {
		t.Errorf("失败订阅不应产生部分计划: %#v", plan)
	}
}

// TestReconcileSubscriptionRoutingSkipsRulesOfDroppedGroups 验证指向「本轮变空」策略组的
// 规则只跳过自己：订阅仍然导入其余规则，并在状态里给出跳过条数与原因。
func TestReconcileSubscriptionRoutingSkipsRulesOfDroppedGroups(t *testing.T) {
	body := `
proxies:
  - name: "香港 01"
    type: ss
    server: hk.example.com
    port: 8388
    cipher: aes-256-gcm
    password: pass
proxy-groups:
  - name: "空组"
    type: select
    proxies: ["已下线的节点"]
rules:
  - DOMAIN-SUFFIX,openai.com,空组
  - DOMAIN-SUFFIX,example.com,DIRECT
`
	a, sub := newRoutingTestApp(t, true, body)
	publishAliveNode(t, a, "香港 01", sub.Name)
	a.reconcileSubscriptionRouting()

	status := a.SubscriptionImportStatuses()[sub.Name]
	if status.Error != "" {
		t.Fatalf("空组不应让整订阅导入失败: %s", status.Error)
	}
	if status.SkippedRules != 1 || len(status.SkipNotes) != 1 {
		t.Fatalf("跳过项未记录到状态: %#v", status)
	}
	if status.Rules != 1 || status.Groups != 0 {
		t.Fatalf("其余规则应照常导入: %#v", status)
	}
	a.mu.RLock()
	plan := a.mergedRoutingLocked()
	a.mu.RUnlock()
	if len(plan.Rules) != 1 || plan.Rules[0] != "DOMAIN-SUFFIX,example.com,DIRECT" {
		t.Fatalf("计划里的规则异常: %#v", plan.Rules)
	}
}

func TestReconcileSubscriptionRoutingFlagsDirectOnlyGroups(t *testing.T) {
	body := strings.Replace(routingSubscriptionBody, `proxies: ["香港 01", DIRECT]`, `proxies: [DIRECT]`, 1)
	a, sub := newRoutingTestApp(t, true, body)
	publishAliveNode(t, a, "香港 01", sub.Name)

	a.reconcileSubscriptionRouting()
	status := a.SubscriptionImportStatuses()[sub.Name]
	if len(status.GroupEntries) != 1 {
		t.Fatalf("缺少策略组明细: %#v", status)
	}
	if status.GroupEntries[0].ProxyMembers != 0 {
		t.Fatalf("只有 DIRECT 成员的组不应统计出代理成员: %#v", status.GroupEntries[0])
	}
	if status.GroupEntries[0].Members != 1 {
		t.Fatalf("成员总数应为 1: %#v", status.GroupEntries[0])
	}
}

// TestImportedGroupFollowsMainExitByDefault 验证兜底语义：订阅作者只写了 DIRECT 的组在
// 未手选时跟随主端口的默认出口（内置 PROXY 组），而不是静默直连。
func TestImportedGroupFollowsMainExitByDefault(t *testing.T) {
	directOnly := strings.Replace(routingSubscriptionBody, `proxies: ["香港 01", DIRECT]`, `proxies: [DIRECT]`, 1)
	a, sub := newRoutingTestApp(t, true, directOnly)
	publishAliveNode(t, a, "香港 01", sub.Name)
	a.reconcileSubscriptionRouting()

	groups := a.ImportedGroups()
	if len(groups) != 1 || !groups[0].FallbackApplied {
		t.Fatalf("只有直连成员的订阅组应跟随默认出口: %#v", groups)
	}

	a.mu.RLock()
	plan := a.mergedRoutingLocked()
	cfg := a.cfg
	nodes := append([]*node.Node(nil), a.nodes...)
	a.mu.RUnlock()
	// 默认出口必须排在成员首位：mihomo 的 select 组在未设置 default-selected 时取成员
	// 首位，写在末位等于没生效（那是这条特性最初的真实缺陷）。
	if got := plan.Groups[0].Proxies; len(got) != 2 || got[0] != "PROXY" || got[1] != "DIRECT" {
		t.Fatalf("默认出口应为成员首位的 PROXY: %#v", got)
	}

	// 主端口默认出口选到节点：订阅组的默认出口随之落在该节点上（配置里通过 PROXY 引用）。
	selected := map[string]string{"PROXY": "香港 01"}
	generated, err := core.BuildWithRouting(cfg, nil, nodes, nil, selected, plan)
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	yaml := string(generated.YAML())
	if !strings.Contains(yaml, "default-selected: 香港 01") {
		t.Fatalf("主端口默认出口未写入配置:\n%s", yaml)
	}
	// 订阅组自己没有手选值，因此不写 default-selected：让它落到成员首位（PROXY）。
	if strings.Count(yaml, "default-selected:") != 1 {
		t.Fatalf("未手选的订阅组不应写入 default-selected:\n%s", yaml)
	}
}

// TestImportedGroupFallbackOffKeepsSubscriptionIntent 验证关掉兜底后完全尊重订阅作者：
// 只有 DIRECT 成员的组不会挂上 PROXY。
func TestImportedGroupFallbackOffKeepsSubscriptionIntent(t *testing.T) {
	directOnly := strings.Replace(routingSubscriptionBody, `proxies: ["香港 01", DIRECT]`, `proxies: [DIRECT]`, 1)
	a, sub := newRoutingTestApp(t, true, directOnly)
	publishAliveNode(t, a, "香港 01", sub.Name)
	a.mu.Lock()
	a.cfg.SubscriptionGroupFallback = config.GroupFallbackOff
	a.mu.Unlock()
	a.reconcileSubscriptionRouting()

	groups := a.ImportedGroups()
	if groups[0].FallbackApplied {
		t.Fatalf("关闭兜底后不应追加 PROXY: %#v", groups[0])
	}
	a.mu.RLock()
	plan := a.mergedRoutingLocked()
	a.mu.RUnlock()
	for _, member := range plan.Groups[0].Proxies {
		if member == "PROXY" {
			t.Fatalf("关闭兜底后不应出现 PROXY 成员: %#v", plan.Groups[0].Proxies)
		}
	}
}

// TestImportedGroupWithNodesKeepsAuthoredMembers 验证订阅作者已经配好节点的组不被兜底
// 改写：auto 模式下只有「没有任何代理成员」的组才追加 PROXY。
func TestImportedGroupWithNodesKeepsAuthoredMembers(t *testing.T) {
	a, sub := newRoutingTestApp(t, true, routingSubscriptionBody)
	publishAliveNode(t, a, "香港 01", sub.Name)
	a.reconcileSubscriptionRouting()

	groups := a.ImportedGroups()
	if groups[0].FallbackApplied {
		t.Fatalf("已有代理成员的组不应被兜底改写: %#v", groups[0])
	}
	a.mu.RLock()
	plan := a.mergedRoutingLocked()
	a.mu.RUnlock()
	for _, member := range plan.Groups[0].Proxies {
		if member == "PROXY" {
			t.Fatalf("已有代理成员的组不应追加 PROXY: %#v", plan.Groups[0].Proxies)
		}
	}
}

// TestImportedGroupManualSelectionOverridesFallback 验证手选出口覆盖默认出口：
// 选中某个节点后 default-selected 指向该节点，优先级高于「成员首位 = PROXY」。
func TestImportedGroupManualSelectionOverridesFallback(t *testing.T) {
	directOnly := strings.Replace(routingSubscriptionBody, `proxies: ["香港 01", DIRECT]`, `proxies: [DIRECT]`, 1)
	a, sub := newRoutingTestApp(t, true, directOnly)
	publishAliveNode(t, a, "香港 01", sub.Name)
	a.reconcileSubscriptionRouting()
	name := a.ImportedGroups()[0].Name
	if err := a.SetImportedGroupSelected(name, "香港 01"); err != nil {
		t.Fatalf("选择失败: %v", err)
	}

	a.mu.RLock()
	plan := a.mergedRoutingLocked()
	a.mu.RUnlock()
	members := plan.Groups[0].Proxies
	// PROXY 仍在成员里（手选节点下线时回落到主端口默认出口），但手选值通过
	// default-selected 生效，不依赖成员顺序。
	if len(members) < 3 || members[0] != "PROXY" || members[1] != "香港 01" {
		t.Fatalf("手选节点应被追加且 PROXY 仍作为默认出口: %#v", members)
	}

	selected, err := groupstate.Load(a.groupSelectedPath())
	if err != nil {
		t.Fatalf("读取选中状态失败: %v", err)
	}
	cfg := a.cfg
	nodes := a.Nodes()
	generated, err := core.BuildWithRouting(cfg, nil, nodes, nil, selected, plan)
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	if !strings.Contains(string(generated.YAML()), "default-selected: 香港 01") {
		t.Fatalf("手选出口未写入配置:\n%s", generated.YAML())
	}
}

// TestImportedGroupSelectionAddsNodeAndPersists 验证「订阅只给了 DIRECT，用户把出口切到
// 某个节点」这一核心场景：一次选择就完成成员追加 + 选中项持久化，并在重新规划后仍然生效。
func TestImportedGroupSelectionAddsNodeAndPersists(t *testing.T) {
	// 用「作者只给了 DIRECT」的组：这正是真实机场订阅的常见形态，也是用户需要
	// 手动指定出口的场景。
	directOnly := strings.Replace(routingSubscriptionBody, `proxies: ["香港 01", DIRECT]`, `proxies: [DIRECT]`, 1)
	a, sub := newRoutingTestApp(t, true, directOnly)
	publishAliveNode(t, a, "香港 01", sub.Name)
	a.reconcileSubscriptionRouting()

	groups := a.ImportedGroups()
	if len(groups) != 1 {
		t.Fatalf("应导入 1 个策略组: %#v", groups)
	}
	if groups[0].ProxyMembers != 0 {
		t.Fatalf("订阅原文组应只有 DIRECT 成员: %#v", groups[0])
	}

	if err := a.SetImportedGroupSelected(groups[0].Name, "香港 01"); err != nil {
		t.Fatalf("选择导入组出口失败: %v", err)
	}

	groups = a.ImportedGroups()
	if groups[0].Selected != "香港 01" || !groups[0].SelectionActive {
		t.Fatalf("选中项未生效: %#v", groups[0])
	}
	if len(groups[0].AddedNodes) != 1 || groups[0].AddedNodes[0] != "香港 01" {
		t.Fatalf("选中的节点应被追加为出口: %#v", groups[0])
	}
	if groups[0].ProxyMembers != 1 {
		t.Fatalf("追加后应统计到 1 个真实代理成员: %#v", groups[0])
	}

	// 重新规划（模拟下一轮刷新）：手选与追加必须跨轮保持。
	a.reconcileSubscriptionRouting()
	groups = a.ImportedGroups()
	if groups[0].Selected != "香港 01" || !groups[0].SelectionActive {
		t.Fatalf("重新规划后选中项丢失: %#v", groups[0])
	}
	if len(groups[0].AddedNodes) != 1 {
		t.Fatalf("重新规划后追加节点丢失: %#v", groups[0])
	}
}

// TestImportedGroupSelectionAcceptsAuto 验证可以把订阅组的出口交给内置 AUTO 组：
// 这样命中该组的流量会跟随「自动最快」出口，而不是订阅作者留下的直连。
func TestImportedGroupSelectionAcceptsAuto(t *testing.T) {
	directOnly := strings.Replace(routingSubscriptionBody, `proxies: ["香港 01", DIRECT]`, `proxies: [DIRECT]`, 1)
	a, sub := newRoutingTestApp(t, true, directOnly)
	publishAliveNode(t, a, "香港 01", sub.Name)
	a.reconcileSubscriptionRouting()

	if err := a.SetImportedGroupSelected("AI策略", "AUTO"); err != nil {
		t.Fatalf("选择 AUTO 失败: %v", err)
	}
	groups := a.ImportedGroups()
	if groups[0].Selected != "AUTO" || !groups[0].SelectionActive {
		t.Fatalf("AUTO 选中项未生效: %#v", groups[0])
	}

	a.mu.RLock()
	plan := a.mergedRoutingLocked()
	cfg := a.cfg
	nodes := append([]*node.Node(nil), a.nodes...)
	a.mu.RUnlock()
	selected, err := groupstate.Load(a.groupSelectedPath())
	if err != nil {
		t.Fatalf("读取选中状态失败: %v", err)
	}
	generated, err := core.BuildWithRouting(cfg, nil, nodes, nil, selected, plan)
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	if !strings.Contains(string(generated.YAML()), "default-selected: AUTO") {
		t.Fatalf("配置未写入 AUTO 选中项:\n%s", generated.YAML())
	}
}

// TestImportedGroupSelectionSurvivesRenamedGroup 验证与用户分组重名、被自动改名的导入组
// 仍然可以被手选出口：覆盖存档按订阅原文组名存放，而手选状态按最终组名存放，两者必须
// 通过「原文组名 -> 最终组名」对应表正确对起来。
func TestImportedGroupSelectionSurvivesRenamedGroup(t *testing.T) {
	directOnly := strings.Replace(routingSubscriptionBody, `proxies: ["香港 01", DIRECT]`, `proxies: [DIRECT]`, 1)
	a, sub := newRoutingTestApp(t, true, directOnly)
	publishAliveNode(t, a, "香港 01", sub.Name)
	// 用户已有一个同名分组：导入组会被改名。
	a.mu.Lock()
	a.cfg.Groups = []config.NodeGroup{{Name: "AI策略", Port: 42050, Subscription: sub.Name}}
	a.mu.Unlock()
	a.reconcileSubscriptionRouting()

	groups := a.ImportedGroups()
	if len(groups) != 1 {
		t.Fatalf("应导入 1 个策略组: %#v", groups)
	}
	finalName := groups[0].Name
	if finalName == "AI策略" {
		t.Fatalf("与用户分组同名的导入组应被改名: %#v", groups[0])
	}
	if err := a.SetImportedGroupSelected(finalName, "香港 01"); err != nil {
		t.Fatalf("对改名后的导入组选择出口失败: %v", err)
	}

	groups = a.ImportedGroups()
	if groups[0].Name != finalName {
		t.Fatalf("改名结果不应因选择而改变: %#v", groups[0])
	}
	if groups[0].Selected != "香港 01" || !groups[0].SelectionActive {
		t.Fatalf("改名组的手选出口未生效: %#v", groups[0])
	}

	// 下一轮规划（重新读取存档）后仍然生效。
	a.reconcileSubscriptionRouting()
	groups = a.ImportedGroups()
	if groups[0].Selected != "香港 01" || !groups[0].SelectionActive {
		t.Fatalf("重新规划后改名组的选中项丢失: %#v", groups[0])
	}
}

// TestImportedGroupSelectionRejectsUnavailableNode 验证选择不可用节点被拒绝，
// 且不会留下半套覆盖状态。
func TestImportedGroupSelectionRejectsUnavailableNode(t *testing.T) {
	a, sub := newRoutingTestApp(t, true, routingSubscriptionBody)
	publishAliveNode(t, a, "香港 01", sub.Name)
	a.reconcileSubscriptionRouting()
	name := a.ImportedGroups()[0].Name

	if err := a.SetImportedGroupSelected(name, "不存在的节点"); err == nil {
		t.Fatal("选择不可用节点必须失败")
	}
	groups := a.ImportedGroups()
	if groups[0].Selected != "" || len(groups[0].AddedNodes) != 0 {
		t.Fatalf("失败的选择不应留下覆盖状态: %#v", groups[0])
	}
}

// TestGeneratedConfigAppliesImportedGroupSelection 验证选中项最终写进 mihomo 配置的
// default-selected 字段（这是它真正生效的唯一途径）。
func TestGeneratedConfigAppliesImportedGroupSelection(t *testing.T) {
	a, sub := newRoutingTestApp(t, true, routingSubscriptionBody)
	publishAliveNode(t, a, "香港 01", sub.Name)
	a.reconcileSubscriptionRouting()
	name := a.ImportedGroups()[0].Name
	if err := a.SetImportedGroupSelected(name, "香港 01"); err != nil {
		t.Fatalf("选择失败: %v", err)
	}

	a.mu.RLock()
	imported := a.mergedImportedLocked()
	plan := a.mergedRoutingLocked()
	cfg := a.cfg
	nodes := append([]*node.Node(nil), a.nodes...)
	a.mu.RUnlock()
	selected, err := groupstate.Load(a.groupSelectedPath())
	if err != nil {
		t.Fatalf("读取选中状态失败: %v", err)
	}

	generated, err := core.BuildWithRouting(cfg, nil, nodes, imported, selected, plan)
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	if !strings.Contains(string(generated.YAML()), "default-selected: 香港 01") {
		t.Fatalf("配置未写入手选出口:\n%s", generated.YAML())
	}
}

// TestGeneratedConfigContainsImportedRouting 打通「订阅正文 → 规划 → 生成配置」全链路：
// 生成的 mihomo YAML 必须带上订阅的策略组、规则与规则集引用，且兜底规则指向内置 PROXY。
func TestGeneratedConfigContainsImportedRouting(t *testing.T) {
	a, sub := newRoutingTestApp(t, true, routingSubscriptionBody)
	publishAliveNode(t, a, "香港 01", sub.Name)
	a.reconcileSubscriptionRouting()

	a.mu.RLock()
	imported := a.mergedImportedLocked()
	plan := a.mergedRoutingLocked()
	cfg := a.cfg
	nodes := append([]*node.Node(nil), a.nodes...)
	a.mu.RUnlock()

	generated, err := core.BuildWithRouting(cfg, nil, nodes, imported, nil, plan)
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	yaml := string(generated.YAML())
	for _, want := range []string{"AI策略", "DOMAIN-SUFFIX,openai.com,AI策略", "MATCH,PROXY", "openai.yaml"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("生成配置缺少 %q:\n%s", want, yaml)
		}
	}
	if strings.Contains(yaml, "MATCH,AI策略") {
		t.Error("订阅兜底规则必须被重定向到内置 PROXY")
	}
}

// TestGeneratedConfigOmitsRoutingWhenDisabled 验证默认关闭时生成配置与历史行为一致：
// 不含订阅策略组，兜底规则仍来自 proxyd 自身。
func TestGeneratedConfigOmitsRoutingWhenDisabled(t *testing.T) {
	a, sub := newRoutingTestApp(t, false, routingSubscriptionBody)
	publishAliveNode(t, a, "香港 01", sub.Name)
	a.reconcileSubscriptionRouting()

	a.mu.RLock()
	plan := a.mergedRoutingLocked()
	cfg := a.cfg
	nodes := append([]*node.Node(nil), a.nodes...)
	a.mu.RUnlock()
	if !plan.IsZero() {
		t.Fatalf("未开启导入时不应有计划: %#v", plan)
	}

	generated, err := core.BuildWithRouting(cfg, nil, nodes, nil, nil, plan)
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	if strings.Contains(string(generated.YAML()), "AI策略") {
		t.Error("未开启导入时配置不应包含订阅策略组")
	}
}

func TestPruneOrphanImportSidecars(t *testing.T) {
	a, sub := newRoutingTestApp(t, true, routingSubscriptionBody)
	publishAliveNode(t, a, "香港 01", sub.Name)
	a.reconcileSubscriptionRouting()

	orphan := subscribe.SubscriptionOverridesPath(a.cfg.StateDir, "已删除的订阅")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		t.Fatalf("创建存档目录失败: %v", err)
	}
	if err := os.WriteFile(orphan, []byte(`{"overrides":{}}`), 0o600); err != nil {
		t.Fatalf("写入孤儿存档失败: %v", err)
	}
	a.pruneOrphanImportSidecars()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("孤儿存档应被清理")
	}
	if _, err := os.Stat(subscribe.SubscriptionOverridesPath(a.cfg.StateDir, sub.Name)); err != nil {
		t.Errorf("有效订阅的存档不应被清理: %v", err)
	}
}
