package app

import (
	"path/filepath"
	"testing"

	"proxyd/internal/config"
	"proxyd/internal/proxy/core"
)

// TestCustomRuleEditAndReorder 验证自定义规则支持原位编辑和顺序调整，且变更会同步
// 写入运行配置与磁盘配置；远程规则和内置 rules 不在该用例的修改范围内。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于创建隔离配置并报告断言失败。
//
// 返回值：无。
//
// 错误情况：编辑/移动失败、目标顺序错误或磁盘 custom-rules 不一致时测试失败。
func TestCustomRuleEditAndReorder(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		ManualNodes: []any{"socks5://127.0.0.1:1080#manual"},
		Listen:      "127.0.0.1",
		PortRange:   [2]int{42000, 42010},
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
		CustomRules: []string{
			"DOMAIN-SUFFIX,first.example,DIRECT",
			"DOMAIN-SUFFIX,second.example,PROXY",
		},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("保存初始配置失败: %v", err)
	}
	a, err := New(cfg, cfgPath)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)

	if err := a.UpdateRule(1, "DOMAIN-SUFFIX,edited.example,REJECT"); err != nil {
		t.Fatalf("编辑自定义规则失败: %v", err)
	}
	if err := a.MoveRule(1, 0); err != nil {
		t.Fatalf("移动自定义规则失败: %v", err)
	}
	want := []string{
		"DOMAIN-SUFFIX,edited.example,REJECT",
		"DOMAIN-SUFFIX,first.example,DIRECT",
	}
	got := a.CustomRules()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("内存规则顺序异常: %#v", got)
	}
	onDisk, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("读取提交后的配置失败: %v", err)
	}
	if len(onDisk.CustomRules) != len(want) || onDisk.CustomRules[0] != want[0] || onDisk.CustomRules[1] != want[1] {
		t.Fatalf("磁盘规则顺序异常: %#v", onDisk.CustomRules)
	}
}

// TestCustomRulePersistenceFailureRollsBack 验证自定义规则热更新成功但持久化失败时，
// 内存配置和 mihomo 运行态都会恢复，不能留下只在当前进程有效的幽灵规则。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用目录路径稳定制造配置保存失败。
//
// 返回值：无。
//
// 错误情况：方法未返回错误或失败后规则列表发生变化时测试失败。
func TestCustomRulePersistenceFailureRollsBack(t *testing.T) {
	cfg := &config.Config{
		Listen:      "127.0.0.1",
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
		CustomRules: []string{"DOMAIN-SUFFIX,old.example,DIRECT"},
	}
	a, err := New(cfg, cfg.StateDir)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)
	if err := a.UpdateRule(0, "DOMAIN-SUFFIX,new.example,PROXY"); err == nil {
		t.Fatal("配置路径是目录时编辑规则应返回持久化错误")
	}
	if got := a.CustomRules(); len(got) != 1 || got[0] != "DOMAIN-SUFFIX,old.example,DIRECT" {
		t.Fatalf("持久化失败后规则未恢复: %#v", got)
	}
}

// TestEffectiveRulesSnapshotAndQuery 验证「当前生效规则」快照的来源标注、筛选与分页。
//
// 覆盖点：
//   - 未应用过配置时 Available 为 false，不把「代理停用」误报成错误。
//   - 成功热更新后按匹配顺序返回全部规则，并带来源键与中文名。
//   - 来源筛选同时接受来源键与中文展示名，未知名返回错误。
//   - 关键字筛选大小写不敏感，Position 保持全局匹配位置。
//   - offset/limit 正确切片，超界返回空页而不是报错。
//
// 参数：t 为 Go 测试上下文。
// 返回值：无；任一断言不成立时测试失败。
// 错误情况：规则顺序、来源、筛选结果或分页边界不符时失败。
func TestEffectiveRulesSnapshotAndQuery(t *testing.T) {
	cfg := &config.Config{
		Listen:      "127.0.0.1",
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
		CustomRules: []string{"DOMAIN-SUFFIX,first.example,DIRECT"},
	}
	a, err := New(cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)

	if status, err := a.EffectiveRules(EffectiveRulesQuery{}); err != nil || status.Available {
		t.Fatalf("尚未应用配置时不应报告可用: %#v err=%v", status, err)
	}
	// 参数校验与可用性无关：代理没生效时来源名写错同样要报错，不能被「没有规则」掩盖。
	if _, err := a.EffectiveRules(EffectiveRulesQuery{Source: "nope"}); err == nil {
		t.Fatal("代理未生效时未知来源也必须报错")
	}

	// 一次规则事务会完成生成 + 热更新，从而写入生效规则快照。
	if err := a.AddRule("DOMAIN-SUFFIX,second.example,PROXY"); err != nil {
		t.Fatalf("添加自定义规则失败: %v", err)
	}
	status, err := a.EffectiveRules(EffectiveRulesQuery{})
	if err != nil {
		t.Fatalf("查询生效规则失败: %v", err)
	}
	if !status.Available || status.Total != 3 || status.Matched != 3 {
		t.Fatalf("生效规则应为首条自定义 + 第二条自定义 + 内置 1 条: %#v", status)
	}
	if status.Rules[0].Line != "DOMAIN-SUFFIX,first.example,DIRECT" || status.Rules[0].Source != string(core.RuleSourceCustom) {
		t.Fatalf("首条规则应为最早的自定义规则: %#v", status.Rules[0])
	}
	if status.Rules[0].SourceLabel != core.RuleSourceCustom.Label() {
		t.Fatalf("来源应带中文展示名: %#v", status.Rules[0])
	}
	if status.Rules[2].Line != "MATCH,PROXY" || status.Rules[2].Source != string(core.RuleSourceBuiltin) {
		t.Fatalf("内置规则应排在自定义规则之后: %#v", status.Rules[2])
	}
	counts := map[string]int{}
	for _, source := range status.Sources {
		counts[source.Source] = source.Count
	}
	if counts[string(core.RuleSourceCustom)] != 2 || counts[string(core.RuleSourceBuiltin)] != 1 {
		t.Fatalf("来源条数不正确: %#v", status.Sources)
	}

	// 来源筛选：键与中文名等价；未知来源必须报错，便于 CLI 直接纠正拼写。
	byKey, err := a.EffectiveRules(EffectiveRulesQuery{Source: "custom"})
	if err != nil || byKey.Matched != 2 {
		t.Fatalf("按来源键筛选失败: %#v err=%v", byKey, err)
	}
	byLabel, err := a.EffectiveRules(EffectiveRulesQuery{Source: core.RuleSourceCustom.Label()})
	if err != nil || byLabel.Matched != 2 {
		t.Fatalf("按来源中文名筛选失败: %#v err=%v", byLabel, err)
	}
	if _, err := a.EffectiveRules(EffectiveRulesQuery{Source: "nope"}); err == nil {
		t.Fatal("未知来源必须返回错误")
	}

	// 关键字筛选大小写不敏感，且保留全局匹配位置（Position 不是筛选结果下标）。
	filtered, err := a.EffectiveRules(EffectiveRulesQuery{Keyword: "SECOND.EXAMPLE"})
	if err != nil || filtered.Matched != 1 {
		t.Fatalf("关键字筛选失败: %#v err=%v", filtered, err)
	}
	if filtered.Rules[0].Position != 2 {
		t.Fatalf("筛选结果应保留全局位置 2: %#v", filtered.Rules[0])
	}

	// 分页：offset 越界返回空页而不是错误；limit 被截断到上限。
	page, err := a.EffectiveRules(EffectiveRulesQuery{Limit: 1, Offset: 1})
	if err != nil || len(page.Rules) != 1 || page.Rules[0].Position != 2 {
		t.Fatalf("分页切片不正确: %#v err=%v", page, err)
	}
	if page.Limit != 1 || page.Total != 3 {
		t.Fatalf("分页元信息不正确: %#v", page)
	}
	beyond, err := a.EffectiveRules(EffectiveRulesQuery{Offset: 99})
	if err != nil || len(beyond.Rules) != 0 || beyond.Matched != 3 {
		t.Fatalf("越界 offset 应返回空页: %#v err=%v", beyond, err)
	}
	clamped, err := a.EffectiveRules(EffectiveRulesQuery{Limit: EffectiveRulesMaxLimit + 1})
	if err != nil || clamped.Limit != EffectiveRulesMaxLimit {
		t.Fatalf("limit 应被截断到上限: %#v err=%v", clamped, err)
	}

	// 核心被停用后不再有生效配置，快照必须同步清空。
	a.clearAppliedConfig()
	if status, err := a.EffectiveRules(EffectiveRulesQuery{}); err != nil || status.Available || status.Total != 0 {
		t.Fatalf("清空生效配置后应报告不可用: %#v err=%v", status, err)
	}
}
