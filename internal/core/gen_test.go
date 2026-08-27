package core

import (
	"fmt"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub/executor"
	"gopkg.in/yaml.v3"

	"proxyd/internal/config"
	"proxyd/internal/node"
)

// fakeConfig 构造最小可用配置。
func fakeConfig() *config.Config {
	return &config.Config{
		Listen:             "127.0.0.1",
		PortRange:          [2]int{42001, 42010},
		MixedPort:          41999,
		Mode:               "rule",
		Rules:              []string{"DOMAIN-SUFFIX,example.com,DIRECT", "MATCH,PROXY"},
		ExternalController: "127.0.0.1:19090",
		LogLevel:           "info",
	}
}

// fakeSocks5 构造一个 socks5 节点。
func fakeSocks5(name, server string, port int) *node.Node {
	return &node.Node{
		Name: name,
		Mapping: map[string]any{
			"name":     name,
			"type":     "socks5",
			"server":   server,
			"port":     port,
			"username": "u",
			"password": "p",
		},
	}
}

// parseYAML 把生成的 YAML 解回 map 便于断言。
func parseYAML(t *testing.T, buf []byte) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := yaml.Unmarshal(buf, &m); err != nil {
		t.Fatalf("生成的 YAML 无法解析: %v", err)
	}
	return m
}

func TestGenerate(t *testing.T) {
	// Generate 内部自检会用 executor.ParseWithBytes，若配置含 GEO 规则会读 geo 文件，
	// 先设置 home 目录避免污染真实目录。
	C.SetHomeDir(t.TempDir())

	cfg := fakeConfig()
	assigns := []Assignment{
		{Port: 42001, Node: fakeSocks5("节点A", "1.2.3.4", 10001)},
		{Port: 42002, Node: fakeSocks5("节点B", "5.6.7.8", 10002)},
	}

	buf, err := Generate(cfg, assigns, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}

	// mihomo 可解析（Generate 已自检，这里再独立验证一次）。
	if _, err := executor.ParseWithBytes(buf); err != nil {
		t.Fatalf("mihomo ParseWithBytes 失败: %v", err)
	}

	m := parseYAML(t, buf)

	if got := m["mixed-port"]; got != 41999 {
		t.Errorf("mixed-port = %v, want 41999", got)
	}
	if got := m["mode"]; got != "rule" {
		t.Errorf("mode = %v, want rule", got)
	}

	// listeners：每个 assignment 一条，带正确 port 与固定出口 proxy。
	listeners, ok := m["listeners"].([]any)
	if !ok || len(listeners) != 2 {
		t.Fatalf("listeners 数量不符: %v", m["listeners"])
	}
	wantListener := []struct {
		name  string
		port  int
		proxy string
	}{
		{"节点A:42001", 42001, "节点A"},
		{"节点B:42002", 42002, "节点B"},
	}
	for i, w := range wantListener {
		l, ok := listeners[i].(map[string]any)
		if !ok {
			t.Fatalf("listeners[%d] 类型异常", i)
		}
		if l["name"] != w.name || l["type"] != "mixed" || l["port"] != w.port || l["proxy"] != w.proxy {
			t.Errorf("listeners[%d] = %v, want name=%s port=%d proxy=%s", i, l, w.name, w.port, w.proxy)
		}
	}

	// proxies 原样透传。
	proxies, ok := m["proxies"].([]any)
	if !ok || len(proxies) != 2 {
		t.Fatalf("proxies 数量不符: %v", m["proxies"])
	}

	// proxy-groups 含 PROXY 组，成员为节点名 + DIRECT。
	groups, ok := m["proxy-groups"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("proxy-groups 数量不符: %v", m["proxy-groups"])
	}
	g := groups[0].(map[string]any)
	if g["name"] != "PROXY" || g["type"] != "select" {
		t.Errorf("proxy-groups[0] = %v, want PROXY/select", g)
	}
	members, ok := g["proxies"].([]any)
	if !ok || len(members) != 3 || members[0] != "节点A" || members[1] != "节点B" || members[2] != "DIRECT" {
		t.Errorf("PROXY 组成员 = %v, want [节点A 节点B DIRECT]", g["proxies"])
	}

	// rules 原样透传。
	rules, ok := m["rules"].([]any)
	if !ok || len(rules) != 2 || rules[0] != "DOMAIN-SUFFIX,example.com,DIRECT" || rules[1] != "MATCH,PROXY" {
		t.Errorf("rules = %v, 未原样透传", m["rules"])
	}
}

func TestGenerateEmptyAssigns(t *testing.T) {
	// 与上一个用例同进程，home 目录已设置，无需重复。

	cfg := fakeConfig()
	buf, err := Generate(cfg, nil, nil)
	if err != nil {
		t.Fatalf("Generate(空 assigns) 失败: %v", err)
	}
	if _, err := executor.ParseWithBytes(buf); err != nil {
		t.Fatalf("mihomo ParseWithBytes 失败: %v", err)
	}

	m := parseYAML(t, buf)
	if _, exists := m["listeners"]; exists {
		t.Errorf("空 assigns 不应输出 listeners: %v", m["listeners"])
	}
	groups := m["proxy-groups"].([]any)
	g := groups[0].(map[string]any)
	members := g["proxies"].([]any)
	if len(members) != 1 || members[0] != "DIRECT" {
		t.Errorf("空 assigns 时 PROXY 组应只含 DIRECT, got %v", members)
	}
}

func TestGenerateOptionalFields(t *testing.T) {
	cfg := fakeConfig()
	cfg.Secret = "s3cret"
	cfg.ExternalUI = "ui"
	cfg.DNS = map[string]any{"enable": true, "nameserver": []string{"223.5.5.5"}}
	cfg.RuleProviders = map[string]any{
		"reject": map[string]any{
			"type":     "http",
			"behavior": "domain",
			"url":      "https://example.com/reject.yaml",
			"path":     "./ruleset/reject.yaml",
			"interval": 86400,
		},
	}

	buf, err := Generate(cfg, nil, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	m := parseYAML(t, buf)
	if m["secret"] != "s3cret" {
		t.Errorf("secret 未透传: %v", m["secret"])
	}
	if m["external-ui"] != "ui" {
		t.Errorf("external-ui 未透传: %v", m["external-ui"])
	}
	if m["dns"] == nil {
		t.Error("dns 未透传")
	}
	if m["rule-providers"] == nil {
		t.Error("rule-providers 未透传")
	}
	// allow-lan：回环地址应为 false。
	if m["allow-lan"] != false {
		t.Errorf("回环监听时 allow-lan 应为 false, got %v", m["allow-lan"])
	}

	cfg.Listen = "0.0.0.0"
	buf, err = Generate(cfg, nil, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	m = parseYAML(t, buf)
	if m["allow-lan"] != true {
		t.Errorf("非回环监听时 allow-lan 应为 true, got %v", m["allow-lan"])
	}
}

func TestGenerateAutoPort(t *testing.T) {
	cfg := fakeConfig()
	cfg.AutoPort = 41998
	cfg.HealthURL = "http://www.gstatic.com/generate_204"
	assigns := []Assignment{
		{Port: 42001, Node: fakeSocks5("节点A", "1.2.3.4", 10001)},
		{Port: 42002, Node: fakeSocks5("节点B", "5.6.7.8", 10002)},
	}

	buf, err := Generate(cfg, assigns, nil)
	if err != nil {
		t.Fatalf("Generate(auto-port) 失败: %v", err)
	}
	if _, err := executor.ParseWithBytes(buf); err != nil {
		t.Fatalf("mihomo ParseWithBytes 失败: %v", err)
	}
	m := parseYAML(t, buf)

	// 主端口不受影响：仍是 rule 模式 mixed-port。
	if got := m["mode"]; got != "rule" {
		t.Errorf("mode = %v, want rule", got)
	}
	if got := m["mixed-port"]; got != 41999 {
		t.Errorf("mixed-port = %v, want 41999（auto-port 不影响主端口）", got)
	}

	// AUTO url-test 组：成员为全部节点，带 url/interval/tolerance。
	groups := m["proxy-groups"].([]any)
	var auto map[string]any
	for _, g := range groups {
		if g.(map[string]any)["name"] == "AUTO" {
			auto = g.(map[string]any)
		}
	}
	if auto == nil {
		t.Fatalf("缺少 AUTO 组: %v", groups)
	}
	if auto["type"] != "url-test" || auto["url"] != cfg.HealthURL || auto["interval"] != 300 || auto["tolerance"] != 50 {
		t.Errorf("AUTO 组配置异常: %v", auto)
	}
	members := auto["proxies"].([]any)
	if len(members) != 2 || members[0] != "节点A" || members[1] != "节点B" {
		t.Errorf("AUTO 组成员 = %v, want [节点A 节点B]", members)
	}

	// auto-port listener 固定走 AUTO。
	listeners := m["listeners"].([]any)
	var autoLn map[string]any
	for _, l := range listeners {
		if l.(map[string]any)["proxy"] == "AUTO" {
			autoLn = l.(map[string]any)
		}
	}
	if autoLn == nil {
		t.Fatalf("缺少 AUTO listener: %v", listeners)
	}
	if autoLn["port"] != 41998 || autoLn["type"] != "mixed" || autoLn["name"] != "AUTO:41998" {
		t.Errorf("AUTO listener 异常: %v", autoLn)
	}
}

func TestGenerateAutoPortEmptyAssigns(t *testing.T) {
	// 无可用节点时跳过 AUTO listener，主端口不受影响。
	cfg := fakeConfig()
	cfg.AutoPort = 41998
	buf, err := Generate(cfg, nil, nil)
	if err != nil {
		t.Fatalf("Generate(auto-port, 空 assigns) 失败: %v", err)
	}
	m := parseYAML(t, buf)
	if m["mode"] != "rule" || m["mixed-port"] != 41999 {
		t.Errorf("主端口异常: mode=%v mixed-port=%v", m["mode"], m["mixed-port"])
	}
	if _, exists := m["listeners"]; exists {
		t.Errorf("空节点时不应生成 listener: %v", m["listeners"])
	}
	for _, g := range m["proxy-groups"].([]any) {
		if g.(map[string]any)["name"] == "AUTO" {
			t.Error("空节点时不应生成 AUTO 组")
		}
	}
}

func TestGenerateCustomRules(t *testing.T) {
	cfg := fakeConfig()
	cfg.CustomRules = []string{
		"DOMAIN-SUFFIX,example.com,节点A",
		"IP-CIDR,10.0.0.0/8,DIRECT,no-resolve",
	}
	assigns := []Assignment{{Port: 42001, Node: fakeSocks5("节点A", "1.2.3.4", 10001)}}

	buf, err := Generate(cfg, assigns, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	m := parseYAML(t, buf)
	rules := m["rules"].([]any)
	want := []string{
		"DOMAIN-SUFFIX,example.com,节点A", // 自定义规则前置
		"IP-CIDR,10.0.0.0/8,DIRECT,no-resolve",
		"DOMAIN-SUFFIX,example.com,DIRECT", // 内置规则原样保留在后面
		"MATCH,PROXY",
	}
	if len(rules) != len(want) {
		t.Fatalf("rules = %v, want %v", rules, want)
	}
	for i, w := range want {
		if rules[i] != w {
			t.Errorf("rules[%d] = %v, want %q", i, rules[i], w)
		}
	}
}

func TestGenerateGroups(t *testing.T) {
	cfg := fakeConfig()
	cfg.Groups = []config.NodeGroup{
		{Name: "g1", Port: 43000, Nodes: []string{"节点A", "不存在的节点"}}, // 取交集
		{Name: "empty", Port: 43001, Nodes: []string{"不存在的节点"}},     // 交集为空，跳过
		{Name: "节点A", Port: 43002, Nodes: []string{"节点A"}},          // 与节点名冲突，跳过
	}
	assigns := []Assignment{
		{Port: 42001, Node: fakeSocks5("节点A", "1.2.3.4", 10001)},
		{Port: 42002, Node: fakeSocks5("节点B", "5.6.7.8", 10002)},
	}

	buf, err := Generate(cfg, assigns, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	m := parseYAML(t, buf)

	// 只有 g1 生成了 url-test 组，成员为交集 [节点A]。
	var g1 map[string]any
	for _, g := range m["proxy-groups"].([]any) {
		gm := g.(map[string]any)
		switch gm["name"] {
		case "g1":
			g1 = gm
		case "empty", "节点A":
			t.Errorf("分组 %v 不应生成", gm["name"])
		}
	}
	if g1 == nil {
		t.Fatalf("缺少 g1 组: %v", m["proxy-groups"])
	}
	if g1["type"] != "url-test" || g1["url"] != cfg.HealthURL {
		t.Errorf("g1 组配置异常: %v", g1)
	}
	members := g1["proxies"].([]any)
	if len(members) != 1 || members[0] != "节点A" {
		t.Errorf("g1 成员 = %v, want [节点A]", members)
	}

	// g1 的 listener 固定走该组，命名 组名:端口。
	var gl map[string]any
	for _, l := range m["listeners"].([]any) {
		if l.(map[string]any)["port"] == 43000 {
			gl = l.(map[string]any)
		}
	}
	if gl == nil {
		t.Fatalf("缺少 g1 listener: %v", m["listeners"])
	}
	if gl["name"] != "g1:43000" || gl["type"] != "mixed" || gl["proxy"] != "g1" {
		t.Errorf("g1 listener 异常: %v", gl)
	}
}

func TestGenerateImportedRules(t *testing.T) {
	// 合并顺序：custom-rules 最前 → 导入规则 → 内置规则。
	cfg := fakeConfig()
	cfg.CustomRules = []string{"DOMAIN-SUFFIX,custom.example,DIRECT"}
	imported := []string{"DOMAIN-SUFFIX,imported.example,PROXY", "DOMAIN-SUFFIX,imported2.example,DIRECT"}

	buf, err := Generate(cfg, nil, imported)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	m := parseYAML(t, buf)
	rules := m["rules"].([]any)
	want := []string{
		"DOMAIN-SUFFIX,custom.example,DIRECT",
		"DOMAIN-SUFFIX,imported.example,PROXY",
		"DOMAIN-SUFFIX,imported2.example,DIRECT",
		"DOMAIN-SUFFIX,example.com,DIRECT",
		"MATCH,PROXY",
	}
	if len(rules) != len(want) {
		t.Fatalf("rules = %v, want %v", rules, want)
	}
	for i, w := range want {
		if rules[i] != w {
			t.Errorf("rules[%d] = %v, want %q", i, rules[i], w)
		}
	}
}

func TestGenerateManyRules(t *testing.T) {
	// 导入规则可能数千条（gfwlist 量级）：验证自检性能可接受。
	cfg := fakeConfig()
	imported := make([]string, 0, 6000)
	for i := 0; i < 6000; i++ {
		imported = append(imported, fmt.Sprintf("DOMAIN-SUFFIX,site-%d.example,PROXY", i))
	}
	start := time.Now()
	buf, err := Generate(cfg, nil, imported)
	if err != nil {
		t.Fatalf("Generate(6000 条导入规则) 失败: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Errorf("6000 条规则自检耗时 %v，超过 10s", elapsed)
	}
	if n := len(parseYAML(t, buf)["rules"].([]any)); n != 6002 {
		t.Errorf("rules 数量 = %d, want 6002", n)
	}
}
