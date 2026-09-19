package core

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub/executor"
	"gopkg.in/yaml.v3"

	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/subscribe"
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

// fakeSubNode 构造带订阅来源的测试节点。
func fakeSubNode(name, sub string, port int) *node.Node {
	n := fakeSocks5(name, fmt.Sprintf("10.0.0.%d", port%255), port)
	n.Subscription = sub
	return n
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
		{"L42001", 42001, "节点A"},
		{"L42002", 42002, "节点B"},
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

	// proxy-groups：PROXY 组（首位，成员为节点名 + AUTO + DIRECT）与 AUTO url-test 组。
	groups, ok := m["proxy-groups"].([]any)
	if !ok || len(groups) != 2 {
		t.Fatalf("proxy-groups 数量不符: %v", m["proxy-groups"])
	}
	g := groups[0].(map[string]any)
	if g["name"] != "PROXY" || g["type"] != "select" {
		t.Errorf("proxy-groups[0] = %v, want PROXY/select", g)
	}
	members, ok := g["proxies"].([]any)
	if !ok || len(members) != 4 || members[0] != "节点A" || members[1] != "节点B" || members[2] != "AUTO" || members[3] != "DIRECT" {
		t.Errorf("PROXY 组成员 = %v, want [节点A 节点B AUTO DIRECT]", g["proxies"])
	}
	auto := groups[1].(map[string]any)
	if auto["name"] != "AUTO" || auto["type"] != "url-test" {
		t.Errorf("proxy-groups[1] = %v, want AUTO/url-test（有可用节点即生成）", auto)
	}

	// rules 原样透传。
	rules, ok := m["rules"].([]any)
	if !ok || len(rules) != 2 || rules[0] != "DOMAIN-SUFFIX,example.com,DIRECT" || rules[1] != "MATCH,PROXY" {
		t.Errorf("rules = %v, 未原样透传", m["rules"])
	}
}

// TestGenerateAcceptsNumericRealityShortIDFromSubscription 验证第三方转换器输出未加引号
// 的 Reality short-id 时，从订阅解析到 mihomo 配置自检的完整链路仍然可用。
//
// 参数说明：
//   - t: *testing.T，提供隔离的 mihomo home 目录和断言。
//
// 返回值说明：无；ParseClash 产物能生成并通过 mihomo ParseWithBytes 时测试通过。
//
// 错误情况：订阅边界未完成类型规范化时，mihomo 会报告无法把数字解码到 string；
// Reality 公钥、short-id 或其它 VLESS 字段不满足配置约束时也会使测试失败。
func TestGenerateAcceptsNumericRealityShortIDFromSubscription(t *testing.T) {
	C.SetHomeDir(t.TempDir())
	body := []byte(`
proxies:
  - name: converter-reality
    type: vless
    server: 192.0.2.1
    port: 443
    uuid: 00000000-0000-0000-0000-000000000001
    tls: true
    flow: xtls-rprx-vision
    servername: example.com
    client-fingerprint: chrome
    reality-opts:
      public-key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
      short-id: 88
`)
	nodes, err := subscribe.ParseClash(body, "converter")
	if err != nil || len(nodes) != 1 {
		t.Fatalf("解析转换订阅失败: nodes=%d err=%v", len(nodes), err)
	}
	nodes[0].Alive = true
	assignments := []Assignment{{Port: 42001, Node: nodes[0]}}
	generated, err := GenerateWithNodes(fakeConfig(), assignments, nodes, nil)
	if err != nil {
		t.Fatalf("生成 mihomo 配置失败: %v", err)
	}
	if _, err := executor.ParseWithBytes(generated); err != nil {
		t.Fatalf("mihomo 无法解析规范化后的 Reality 节点: %v", err)
	}
}

// TestGeneratePortMappingDisabledKeepsRoutingNodesAndOtherListeners 验证关闭端口映射后，
// 仅移除“健康节点一对一端口”入口，同时继续保留节点出站、PROXY/AUTO 组、自动优选
// 端口和自定义策略组端口。这样开关不会误伤主代理链路或其它显式入口。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于隔离 mihomo 主目录并报告断言失败。
//
// 返回值：无。
//
// 错误情况：生成失败、被关闭的映射端口仍存在、其它入口被错误删除，或路由节点从
// PROXY 组中丢失时测试失败。
func TestGeneratePortMappingDisabledKeepsRoutingNodesAndOtherListeners(t *testing.T) {
	C.SetHomeDir(t.TempDir())
	cfg := fakeConfig()
	disabled := false
	cfg.PortMapping = &disabled
	cfg.AutoPort = 41998
	cfg.HealthURL = "http://www.gstatic.com/generate_204"
	cfg.Groups = []config.NodeGroup{{
		Name:  "低延迟",
		Port:  43000,
		Type:  config.GroupTypeURLTest,
		Nodes: []string{"节点A"},
	}}
	assigns := []Assignment{
		{Port: 42001, Node: fakeSocks5("节点A", "1.2.3.4", 10001)},
		{Port: 42002, Node: fakeSocks5("节点B", "5.6.7.8", 10002)},
	}

	buf, err := Generate(cfg, assigns, nil)
	if err != nil {
		t.Fatalf("关闭端口映射后生成配置失败: %v", err)
	}
	m := parseYAML(t, buf)
	listeners, ok := m["listeners"].([]any)
	if !ok {
		t.Fatalf("关闭节点映射后仍应保留自动端口和分组端口: %#v", m["listeners"])
	}
	ports := make(map[int]bool, len(listeners))
	for index, raw := range listeners {
		listener, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("listeners[%d] 类型异常: %#v", index, raw)
		}
		port, ok := listener["port"].(int)
		if !ok {
			t.Fatalf("listeners[%d].port 类型异常: %#v", index, listener["port"])
		}
		ports[port] = true
	}
	if ports[42001] || ports[42002] {
		t.Fatalf("关闭端口映射后仍生成一对一入口: %#v", ports)
	}
	if !ports[41998] || !ports[43000] {
		t.Fatalf("关闭端口映射不应删除自动端口或分组端口: %#v", ports)
	}

	groups, ok := m["proxy-groups"].([]any)
	if !ok || len(groups) < 1 {
		t.Fatalf("关闭端口映射后缺少 PROXY 组: %#v", m["proxy-groups"])
	}
	proxyGroup, ok := groups[0].(map[string]any)
	if !ok {
		t.Fatalf("PROXY 组类型异常: %#v", groups[0])
	}
	members, ok := proxyGroup["proxies"].([]any)
	if !ok || len(members) != 4 || members[0] != "节点A" || members[1] != "节点B" || members[2] != "AUTO" || members[3] != "DIRECT" {
		t.Fatalf("关闭端口映射不应移除路由节点，实际成员: %#v", proxyGroup["proxies"])
	}
}

// TestGenerateSubscriptionPortMappingDisabled 验证订阅级端口映射开关只停该订阅节点的
// 一对一 listener：节点仍留在 PROXY 路由组与稳定分配中，其他订阅与手动节点的监听不受影响。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于隔离 mihomo 主目录并报告断言失败。
//
// 返回值：无。
//
// 错误情况：被关订阅的节点仍生成监听、其他来源的监听被误删，或路由组成员丢失时测试失败。
func TestGenerateSubscriptionPortMappingDisabled(t *testing.T) {
	C.SetHomeDir(t.TempDir())
	cfg := fakeConfig()
	disabled := false
	cfg.Subscriptions = []config.Subscription{
		{Name: "sub-a", URL: "https://a.example.com/sub", PortMapping: &disabled},
		{Name: "sub-b", URL: "https://b.example.com/sub"},
	}
	assigns := []Assignment{
		{Port: 42001, Node: fakeSubNode("节点A", "sub-a", 10001)},
		{Port: 42002, Node: fakeSubNode("节点B", "sub-b", 10002)},
		{Port: 42003, Node: fakeSubNode("手动节点", "manual", 10003)},
	}

	buf, err := Generate(cfg, assigns, nil)
	if err != nil {
		t.Fatalf("订阅级关闭端口映射后生成配置失败: %v", err)
	}
	m := parseYAML(t, buf)
	ports := map[int]bool{}
	for _, raw := range m["listeners"].([]any) {
		listener := raw.(map[string]any)
		ports[listener["port"].(int)] = true
	}
	if ports[42001] {
		t.Fatalf("订阅 sub-a 已关闭映射，其节点不应再生成一对一入口: %#v", ports)
	}
	if !ports[42002] || !ports[42003] {
		t.Fatalf("其他订阅与手动节点的监听不应受 sub-a 开关影响: %#v", ports)
	}

	groups, ok := m["proxy-groups"].([]any)
	if !ok || len(groups) < 1 {
		t.Fatalf("缺少 PROXY 组: %#v", m["proxy-groups"])
	}
	proxyGroup, ok := groups[0].(map[string]any)
	if !ok {
		t.Fatalf("PROXY 组类型异常: %#v", groups[0])
	}
	members, ok := proxyGroup["proxies"].([]any)
	if !ok || len(members) != 5 || members[0] != "节点A" || members[1] != "节点B" || members[2] != "手动节点" || members[3] != "AUTO" || members[4] != "DIRECT" {
		t.Fatalf("订阅级关闭映射不应移除路由节点，实际成员: %#v", proxyGroup["proxies"])
	}
}

// TestGenerateWithDialerProxyDependencies 验证链式入口即使是唯一获得本地端口的节点，
// 其上游节点仍会作为 proxy-only 出站进入配置，并通过 mihomo 的引用与循环自检。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：依赖节点被端口容量截断、额外生成 listener、dialer-proxy 字段丢失，
// 或 mihomo 无法解析最终配置时测试失败。
func TestGenerateWithDialerProxyDependencies(t *testing.T) {
	C.SetHomeDir(t.TempDir())
	cfg := fakeConfig()
	exit := fakeSocks5("链路出口", "1.2.3.4", 10001)
	exit.Alive = true
	entry := fakeSocks5("链路入口", "5.6.7.8", 10002)
	entry.Mapping["dialer-proxy"] = exit.Name
	entry.Alive = true

	buf, err := GenerateWithNodes(cfg, []Assignment{{Port: 42001, Node: entry}}, []*node.Node{entry, exit}, nil)
	if err != nil {
		t.Fatalf("GenerateWithNodes 生成链式代理失败: %v", err)
	}
	if _, err := executor.ParseWithBytes(buf); err != nil {
		t.Fatalf("mihomo 无法解析 dialer-proxy 配置: %v", err)
	}
	m := parseYAML(t, buf)
	proxies, ok := m["proxies"].([]any)
	if !ok || len(proxies) != 2 {
		t.Fatalf("链路依赖未作为额外出站生成: %#v", m["proxies"])
	}
	if got := proxies[0].(map[string]any)["dialer-proxy"]; got != exit.Name {
		t.Fatalf("dialer-proxy 字段未透传: %v", got)
	}
	listeners, ok := m["listeners"].([]any)
	if !ok || len(listeners) != 1 {
		t.Fatalf("proxy-only 依赖不应占用本地端口: %#v", m["listeners"])
	}
}

// TestGenerateDialerProxyTargetsGroup 验证现代 mihomo 推荐的“节点 dialer-proxy 指向
// 代理组”链式方式可用，以替代已经从核心移除的 relay 组类型。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：未分配端口的组成员被丢弃、代理组未生成或 mihomo 引用校验失败时测试失败。
func TestGenerateDialerProxyTargetsGroup(t *testing.T) {
	C.SetHomeDir(t.TempDir())
	cfg := fakeConfig()
	cfg.Groups = []config.NodeGroup{{
		Name: "链路上游组", Port: 43000, Type: config.GroupTypeFallback, Nodes: []string{"上游 A", "上游 B"},
	}}
	upstreamA := fakeSocks5("上游 A", "1.2.3.4", 10001)
	upstreamA.Alive = true
	upstreamB := fakeSocks5("上游 B", "2.3.4.5", 10002)
	upstreamB.Alive = true
	entry := fakeSocks5("链路入口", "5.6.7.8", 10003)
	entry.Mapping["dialer-proxy"] = "链路上游组"
	entry.Alive = true

	buf, err := GenerateWithNodes(
		cfg,
		[]Assignment{{Port: 42001, Node: entry}},
		[]*node.Node{entry, upstreamA, upstreamB},
		nil,
	)
	if err != nil {
		t.Fatalf("生成指向代理组的 dialer-proxy 失败: %v", err)
	}
	if _, err := executor.ParseWithBytes(buf); err != nil {
		t.Fatalf("mihomo 无法解析代理组链路: %v", err)
	}
	m := parseYAML(t, buf)
	proxies, ok := m["proxies"].([]any)
	if !ok || len(proxies) != 3 {
		t.Fatalf("代理组成员未注册为 proxy-only 出站: %#v", m["proxies"])
	}
}

// TestGenerateTUN 验证 proxyd 的默认 TUN 字段和高级透传字段会进入 mihomo 配置。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：生成失败、mihomo 无法解析或字段丢失时测试失败；本测试只解析配置，
// 不启动 TUN 设备，因此不要求测试进程具备 root/CAP_NET_ADMIN 权限。
func TestGenerateTUN(t *testing.T) {
	cfg := fakeConfig()
	cfg.TUN = config.DefaultTUNConfig()
	cfg.TUN.Enable = true
	cfg.TUN.Extra = map[string]any{"strict-route": true}

	buf, err := Generate(cfg, nil, nil)
	if err != nil {
		t.Fatalf("Generate(TUN) 失败: %v", err)
	}
	tunSection, ok := parseYAML(t, buf)["tun"].(map[string]any)
	if !ok {
		t.Fatalf("生成配置缺少 tun 段: %s", buf)
	}
	if tunSection["enable"] != true || tunSection["stack"] != "system" ||
		tunSection["auto-route"] != true || tunSection["auto-detect-interface"] != true ||
		tunSection["strict-route"] != true {
		t.Errorf("tun 段字段异常: %#v", tunSection)
	}
	dnsHijack, ok := tunSection["dns-hijack"].([]any)
	if !ok || len(dnsHijack) != 1 || dnsHijack[0] != "0.0.0.0:53" {
		t.Errorf("tun.dns-hijack = %#v", tunSection["dns-hijack"])
	}
}

// TestGenerateDNSPresets 验证 fake-ip/redir-host 预设，以及手写 dns 的最高优先级。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：预设生成的 DNS 段无法被 mihomo 解析、增强模式错误，或手写配置被
// dns-preset 覆盖时测试失败。
func TestGenerateDNSPresets(t *testing.T) {
	for _, preset := range []string{config.DNSPresetFakeIP, config.DNSPresetRedirHost} {
		t.Run(preset, func(t *testing.T) {
			cfg := fakeConfig()
			cfg.DNSPreset = preset
			buf, err := Generate(cfg, nil, nil)
			if err != nil {
				t.Fatalf("Generate(%s) 失败: %v", preset, err)
			}
			dnsSection, ok := parseYAML(t, buf)["dns"].(map[string]any)
			if !ok || dnsSection["enable"] != true || dnsSection["enhanced-mode"] != preset {
				t.Fatalf("dns preset %s 生成异常: %#v", preset, dnsSection)
			}
			_, hasRange := dnsSection["fake-ip-range"]
			if hasRange != (preset == config.DNSPresetFakeIP) {
				t.Errorf("preset=%s fake-ip-range presence=%t", preset, hasRange)
			}
		})
	}

	cfg := fakeConfig()
	cfg.DNSPreset = config.DNSPresetFakeIP
	cfg.DNS = map[string]any{"enable": true, "enhanced-mode": "redir-host", "nameserver": []string{"9.9.9.9"}}
	buf, err := Generate(cfg, nil, nil)
	if err != nil {
		t.Fatalf("Generate(custom DNS) 失败: %v", err)
	}
	dnsSection := parseYAML(t, buf)["dns"].(map[string]any)
	if dnsSection["enhanced-mode"] != "redir-host" {
		t.Fatalf("手写 dns 未优先于 preset: %#v", dnsSection)
	}

	// YAML 中显式的 dns: {} 不包含任何策略，不能意外压掉用户选择的预设。
	cfg.DNS = map[string]any{}
	buf, err = Generate(cfg, nil, nil)
	if err != nil {
		t.Fatalf("Generate(empty custom DNS) 失败: %v", err)
	}
	dnsSection = parseYAML(t, buf)["dns"].(map[string]any)
	if dnsSection["enhanced-mode"] != config.DNSPresetFakeIP {
		t.Fatalf("空 dns map 不应覆盖 preset: %#v", dnsSection)
	}
}

func TestGenerateBuiltinProxyGroup(t *testing.T) {
	// 内置 PROXY 组：成员 = 全部可用节点（含隧道类）+ AUTO + DIRECT（节点在前，
	// AUTO/DIRECT 殿后）；AutoPort=0 且有可用节点时 AUTO 组也生成；主端口恒为顶层 mixed-port。
	cfg := fakeConfig()
	cfg.HealthURL = "http://www.gstatic.com/generate_204"
	tunnel := fakeSSHNode("公司 VPN", "10.0.0.1", 22)
	tunnel.Alive = true
	assigns := []Assignment{
		{Port: 42001, Node: fakeSocks5("节点A", "1.2.3.4", 10001)},
		{Port: 42002, Node: fakeSocks5("节点B", "5.6.7.8", 10002)},
	}

	buf, err := GenerateWithState(cfg, assigns, []*node.Node{tunnel}, nil, nil)
	if err != nil {
		t.Fatalf("GenerateWithState 失败: %v", err)
	}
	if _, err := executor.ParseWithBytes(buf); err != nil {
		t.Fatalf("mihomo ParseWithBytes 失败: %v", err)
	}
	m := parseYAML(t, buf)

	if m["mixed-port"] != 41999 {
		t.Errorf("主端口应恒为顶层 mixed-port: %v", m["mixed-port"])
	}
	if _, exists := m["listeners"]; exists {
		for _, l := range m["listeners"].([]any) {
			if l.(map[string]any)["port"] == 41999 {
				t.Errorf("不应存在主端口 listener: %v", l)
			}
		}
	}

	groups := map[string]map[string]any{}
	for _, raw := range m["proxy-groups"].([]any) {
		g := raw.(map[string]any)
		groups[g["name"].(string)] = g
	}
	proxyGroup := groups["PROXY"]
	if proxyGroup == nil || proxyGroup["type"] != "select" {
		t.Fatalf("缺少内置 PROXY select 组: %v", m["proxy-groups"])
	}
	members := proxyGroup["proxies"].([]any)
	want := []string{"节点A", "节点B", "公司 VPN", "AUTO", "DIRECT"}
	if len(members) != len(want) {
		t.Fatalf("PROXY 组成员 = %v, want %v", members, want)
	}
	for i, w := range want {
		if members[i] != w {
			t.Errorf("PROXY 组成员[%d] = %v, want %q（全量 %v）", i, members[i], w, members)
		}
	}
	if _, exists := proxyGroup["default-selected"]; exists {
		t.Errorf("未选择时不应写 default-selected: %v", proxyGroup)
	}

	// AutoPort == 0 但有可用节点：AUTO url-test 组仍生成（供 PROXY 组「自动最快」引用），
	// 但不生成 AUTO listener。
	auto := groups["AUTO"]
	if auto == nil || auto["type"] != "url-test" {
		t.Fatalf("有可用节点时应生成 AUTO 组: %v", m["proxy-groups"])
	}
	if members := auto["proxies"].([]any); len(members) != 2 || members[0] != "节点A" || members[1] != "节点B" {
		t.Errorf("AUTO 组成员限非隧道节点 = %v, want [节点A 节点B]", members)
	}
	for _, l := range m["listeners"].([]any) {
		if l.(map[string]any)["proxy"] == "AUTO" {
			t.Errorf("AutoPort=0 时不应生成 AUTO listener: %v", l)
		}
	}
}

func TestGenerateBuiltinProxyDefaultSelected(t *testing.T) {
	// 持久化选中项（groupstate 的 "PROXY" 键）为有效成员时写入 default-selected；
	// AUTO/DIRECT 与节点名同样可选；失效值忽略，mihomo 原生回退成员首位。
	cfg := fakeConfig()
	cfg.HealthURL = "http://www.gstatic.com/generate_204"
	assigns := []Assignment{
		{Port: 42001, Node: fakeSocks5("节点A", "1.2.3.4", 10001)},
		{Port: 42002, Node: fakeSocks5("节点B", "5.6.7.8", 10002)},
	}

	proxyGroupOf := func(t *testing.T, selected map[string]string) map[string]any {
		t.Helper()
		buf, err := GenerateWithState(cfg, assigns, nil, nil, selected)
		if err != nil {
			t.Fatalf("GenerateWithState 失败: %v", err)
		}
		for _, raw := range parseYAML(t, buf)["proxy-groups"].([]any) {
			g := raw.(map[string]any)
			if g["name"] == "PROXY" {
				return g
			}
		}
		t.Fatal("缺少 PROXY 组")
		return nil
	}

	for _, sel := range []string{"节点B", "AUTO", "DIRECT"} {
		if got := proxyGroupOf(t, map[string]string{"PROXY": sel})["default-selected"]; got != sel {
			t.Errorf("selected=%q 时 default-selected = %v", sel, got)
		}
	}
	if g := proxyGroupOf(t, map[string]string{"PROXY": "已消失"}); g["default-selected"] != nil {
		t.Errorf("失效选中值不应写入 default-selected: %v", g)
	}
	// 无可用节点（AUTO 组不存在）时选 AUTO 视为失效值忽略。
	if g := proxyGroupOfEmpty(t, cfg, map[string]string{"PROXY": "AUTO"}); g["default-selected"] != nil {
		t.Errorf("无可用节点时 AUTO 选择应被忽略: %v", g)
	}
}

// proxyGroupOfEmpty 在无 assigns/节点时生成并取出 PROXY 组。
func proxyGroupOfEmpty(t *testing.T, cfg *config.Config, selected map[string]string) map[string]any {
	t.Helper()
	buf, err := GenerateWithState(cfg, nil, nil, nil, selected)
	if err != nil {
		t.Fatalf("GenerateWithState(空 assigns) 失败: %v", err)
	}
	m := parseYAML(t, buf)
	for _, raw := range m["proxy-groups"].([]any) {
		g := raw.(map[string]any)
		if g["name"] == "PROXY" {
			if members := g["proxies"].([]any); len(members) != 1 || members[0] != "DIRECT" {
				t.Fatalf("无节点时 PROXY 组应只含 DIRECT: %v", members)
			}
			return g
		}
	}
	t.Fatal("缺少 PROXY 组")
	return nil
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
	if autoLn["port"] != 41998 || autoLn["type"] != "mixed" || autoLn["name"] != "L41998" {
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
	if gl["name"] != "L43000" || gl["type"] != "mixed" || gl["proxy"] != "g1" {
		t.Errorf("g1 listener 异常: %v", gl)
	}
}

// TestGenerateGroupTypeAndSubscriptionMembers 验证 A2 分组演进的核心生成规则。
//
// 分组 type 必须原样传给 mihomo；subscription 非空时，成员不再来自静态 nodes，
// 而是从当前可用 assignment 中按节点订阅来源动态收集，这样订阅刷新后组成员会自动跟随。
func TestGenerateGroupTypeAndSubscriptionMembers(t *testing.T) {
	cfg := fakeConfig()
	cfg.Groups = []config.NodeGroup{
		{Name: "机场A", Port: 43000, Type: config.GroupTypeFallback, Subscription: "sub-a"},
		{Name: "轮询B", Port: 43001, Type: config.GroupTypeLoadBalance, Subscription: "sub-b"},
	}
	assigns := []Assignment{
		{Port: 42001, Node: fakeSubNode("A1", "sub-a", 10001)},
		{Port: 42002, Node: fakeSubNode("A2", "sub-a", 10002)},
		{Port: 42003, Node: fakeSubNode("B1", "sub-b", 10003)},
	}

	buf, err := Generate(cfg, assigns, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	m := parseYAML(t, buf)
	groups := map[string]map[string]any{}
	for _, raw := range m["proxy-groups"].([]any) {
		g := raw.(map[string]any)
		groups[g["name"].(string)] = g
	}
	if groups["机场A"]["type"] != config.GroupTypeFallback {
		t.Fatalf("机场A type = %v", groups["机场A"]["type"])
	}
	aMembers := groups["机场A"]["proxies"].([]any)
	if len(aMembers) != 2 || aMembers[0] != "A1" || aMembers[1] != "A2" {
		t.Fatalf("机场A 成员 = %v", aMembers)
	}
	if groups["轮询B"]["type"] != config.GroupTypeLoadBalance {
		t.Fatalf("轮询B type = %v", groups["轮询B"]["type"])
	}
	bMembers := groups["轮询B"]["proxies"].([]any)
	if len(bMembers) != 1 || bMembers[0] != "B1" {
		t.Fatalf("轮询B 成员 = %v", bMembers)
	}
}

// TestGenerateSelectGroupDefaultSelected 验证 select 分组把持久化选中项写入 mihomo
// 原生 default-selected 字段，且已失效（不在当前成员中）的选中值被忽略。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：default-selected 缺失、写错或失效值被写入时测试失败。
func TestGenerateSelectGroupDefaultSelected(t *testing.T) {
	C.SetHomeDir(t.TempDir())
	cfg := fakeConfig()
	cfg.Groups = []config.NodeGroup{
		{Name: "手动出口", Port: 43000, Type: config.GroupTypeSelect, Nodes: []string{"节点A", "节点B"}},
		{Name: "失效选中", Port: 43001, Type: config.GroupTypeSelect, Nodes: []string{"节点A"}},
	}
	assigns := []Assignment{
		{Port: 42001, Node: fakeSocks5("节点A", "1.2.3.4", 10001)},
		{Port: 42002, Node: fakeSocks5("节点B", "5.6.7.8", 10002)},
	}

	buf, err := GenerateWithState(cfg, assigns, nil, nil, map[string]string{
		"手动出口": "节点B",
		"失效选中": "已消失的节点",
	})
	if err != nil {
		t.Fatalf("GenerateWithState 失败: %v", err)
	}
	m := parseYAML(t, buf)
	groups := map[string]map[string]any{}
	for _, raw := range m["proxy-groups"].([]any) {
		g := raw.(map[string]any)
		groups[g["name"].(string)] = g
	}
	if groups["手动出口"]["type"] != "select" {
		t.Fatalf("手动出口 type = %v", groups["手动出口"]["type"])
	}
	if groups["手动出口"]["default-selected"] != "节点B" {
		t.Fatalf("default-selected = %v, want 节点B", groups["手动出口"]["default-selected"])
	}
	if _, exists := groups["失效选中"]["default-selected"]; exists {
		t.Fatalf("失效选中值不应写入 default-selected: %v", groups["失效选中"])
	}
}

// fakeSSHNode 构造一个 ssh 隧道类节点（ssh 出站无构建标签门槛，可在无 with_gvisor 的
// 测试进程中通过 mihomo 解析自检）。
func fakeSSHNode(name, server string, port int) *node.Node {
	return &node.Node{
		Name: name,
		Mapping: map[string]any{
			"name": name, "type": "ssh", "server": server, "port": port,
			"username": "u", "password": "p",
		},
	}
}

// TestGenerateTunnelNodesViaGroupAndProxy 验证隧道类节点不占用本地端口，
// 但仍作为出站注册、可被分组 select 引用、也是内置 PROXY 组（默认出口）的成员。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：隧道节点出现在一对一 listeners、缺失于 proxies/组成员，或失效后仍留在
// PROXY 组成员中时测试失败。
func TestGenerateTunnelNodesViaGroupAndProxy(t *testing.T) {
	C.SetHomeDir(t.TempDir())
	cfg := fakeConfig()
	tunnel := fakeSSHNode("公司 VPN", "10.0.0.1", 22)
	tunnel.Alive = true
	cfg.Groups = []config.NodeGroup{{
		Name: "vpn 出口", Port: 43000, Type: config.GroupTypeSelect, Nodes: []string{"公司 VPN"},
	}}
	assigns := []Assignment{{Port: 42001, Node: fakeSocks5("节点A", "1.2.3.4", 10001)}}

	buf, err := GenerateWithState(cfg, assigns, []*node.Node{tunnel}, nil,
		map[string]string{"vpn 出口": "公司 VPN"})
	if err != nil {
		t.Fatalf("GenerateWithState 失败: %v", err)
	}
	if _, err := executor.ParseWithBytes(buf); err != nil {
		t.Fatalf("mihomo 无法解析含隧道节点的配置: %v", err)
	}
	m := parseYAML(t, buf)

	// 隧道节点注册为出站，但不生成任何固定到它的一对一 listener。
	foundTunnel := false
	for _, raw := range m["proxies"].([]any) {
		if raw.(map[string]any)["name"] == "公司 VPN" {
			foundTunnel = true
		}
	}
	if !foundTunnel {
		t.Fatalf("隧道节点未注册为出站: %v", m["proxies"])
	}
	for _, raw := range m["listeners"].([]any) {
		l := raw.(map[string]any)
		if l["proxy"] == "公司 VPN" {
			t.Fatalf("隧道节点不应获得一对一 listener: %v", l)
		}
	}

	// 分组引用隧道节点并恢复选中；内置 PROXY 组成员也包含隧道节点。
	groups := map[string]map[string]any{}
	for _, raw := range m["proxy-groups"].([]any) {
		g := raw.(map[string]any)
		groups[g["name"].(string)] = g
	}
	if groups["vpn 出口"]["default-selected"] != "公司 VPN" {
		t.Fatalf("vpn 出口 default-selected = %v", groups["vpn 出口"])
	}
	members := groups["PROXY"]["proxies"].([]any)
	if len(members) != 4 || members[1] != "公司 VPN" {
		t.Fatalf("PROXY 组应包含隧道节点（[节点A 公司 VPN AUTO DIRECT]）: %v", members)
	}

	// 隧道节点失效：不再出现在 PROXY 组成员，主端口保持规则模式。
	dead := fakeSSHNode("公司 VPN", "10.0.0.1", 22) // Alive=false
	buf, err = GenerateWithState(cfg, assigns, []*node.Node{dead}, nil, nil)
	if err != nil {
		t.Fatalf("GenerateWithState(失效隧道节点) 失败: %v", err)
	}
	m = parseYAML(t, buf)
	if m["mixed-port"] != 41999 {
		t.Fatalf("主端口应恒为规则模式 mixed-port: %v", m["mixed-port"])
	}
	for _, raw := range m["proxy-groups"].([]any) {
		g := raw.(map[string]any)
		if g["name"] != "PROXY" {
			continue
		}
		for _, member := range g["proxies"].([]any) {
			if member == "公司 VPN" {
				t.Fatalf("失效隧道节点不应留在 PROXY 组成员: %v", g["proxies"])
			}
		}
	}
}

// TestPrepareOutboundMappingTailscaleStateDir 验证 tailscale 出站的 tsnet state-dir
// 固定逻辑：未显式设置时改写为 proxyd state-dir 下按节点隔离的目录（不污染原 Mapping），
// 显式设置时尊重用户值。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：改写缺失、目录名不安全/冲突、原 Mapping 被污染或用户值被覆盖时测试失败。
func TestPrepareOutboundMappingTailscaleStateDir(t *testing.T) {
	cfg := fakeConfig()
	cfg.StateDir = "/tmp/proxyd-state"
	ts := &node.Node{
		Name:    "tailscale 节点/A",
		Mapping: map[string]any{"name": "tailscale 节点/A", "type": "tailscale", "auth-key": "tskey-auth-xxx"},
	}
	out := prepareOutboundMapping(cfg, ts)
	dir, _ := out["state-dir"].(string)
	if !strings.HasPrefix(dir, "/tmp/proxyd-state/tsnet/") {
		t.Fatalf("state-dir 未改写为 proxyd 隔离目录: %q", dir)
	}
	if strings.Contains(dir, "/A") || strings.ContainsAny(filepath.Base(dir), `/\:*?"<>|`) {
		t.Fatalf("目录名未安全化: %q", dir)
	}
	if _, exists := ts.Mapping["state-dir"]; exists {
		t.Fatal("改写不得污染节点原 Mapping")
	}

	// 同名不同凭据的节点获得不同目录；同节点重复生成保持稳定。
	other := &node.Node{
		Name:    "tailscale 节点/A",
		Mapping: map[string]any{"name": "tailscale 节点/A", "type": "tailscale", "auth-key": "tskey-auth-yyy"},
	}
	if prepareOutboundMapping(cfg, other)["state-dir"] == dir {
		t.Fatal("不同 Key 的同名节点不应共享 tsnet 目录")
	}
	if prepareOutboundMapping(cfg, ts)["state-dir"] != dir {
		t.Fatal("同一节点的 tsnet 目录应稳定")
	}

	// 用户显式设置的 state-dir 被尊重；非 tailscale 节点不触碰。
	custom := &node.Node{
		Name:    "custom",
		Mapping: map[string]any{"name": "custom", "type": "tailscale", "auth-key": "k", "state-dir": "/my/ts"},
	}
	if got := prepareOutboundMapping(cfg, custom); got["state-dir"] != "/my/ts" {
		t.Fatalf("显式 state-dir 被覆盖: %v", got["state-dir"])
	}
	plain := fakeSocks5("plain", "1.2.3.4", 1080)
	if got := prepareOutboundMapping(cfg, plain); got["state-dir"] != nil {
		t.Fatalf("普通节点不应被注入 state-dir: %v", got)
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

// ruleIndex 返回规则文本在生成结果中的下标；不存在返回 -1。
func ruleIndex(m map[string]any, rule string) int {
	rules, _ := m["rules"].([]any)
	for i, r := range rules {
		if r == rule {
			return i
		}
	}
	return -1
}

func TestGenerateAdBlock(t *testing.T) {
	C.SetHomeDir(t.TempDir())

	cfg := fakeConfig()
	cfg.AdBlock = config.AdBlockConfig{Enable: true, RuleURL: "https://example.com/reject.txt"}
	assigns := []Assignment{{Port: 42001, Node: fakeSocks5("节点A", "1.2.3.4", 10001)}}
	imported := []string{"DOMAIN-SUFFIX,imported.example,DIRECT"}

	buf, err := Generate(cfg, assigns, imported)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	m := parseYAML(t, buf)

	// 广告规则位置：imported 之后、内置 rules（custom 之后的 cfg.Rules）之前。
	ruleSet := ruleIndex(m, "RULE-SET,adblock,REJECT")
	if ruleSet < 0 {
		t.Fatalf("rules 中缺少 RULE-SET,adblock,REJECT: %v", m["rules"])
	}
	if imported := ruleIndex(m, "DOMAIN-SUFFIX,imported.example,DIRECT"); imported < 0 || ruleSet <= imported {
		t.Errorf("RULE-SET 应排在 imported 规则之后: imported=%d ruleset=%d", imported, ruleSet)
	}
	if builtin := ruleIndex(m, "DOMAIN-SUFFIX,example.com,DIRECT"); builtin < 0 || ruleSet >= builtin {
		t.Errorf("RULE-SET 应排在内置 rules 之前: builtin=%d ruleset=%d", builtin, ruleSet)
	}

	// rule-providers 注入 adblock 键，字段与默认规则集格式（domain 行为 + YAML payload）匹配。
	providers, ok := m["rule-providers"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 rule-providers 段: %v", m)
	}
	provider, ok := providers["adblock"].(map[string]any)
	if !ok {
		t.Fatalf("rule-providers 缺少 adblock 键: %v", providers)
	}
	if provider["type"] != "http" || provider["behavior"] != "domain" || provider["format"] != "yaml" ||
		provider["url"] != "https://example.com/reject.txt" || provider["interval"] != 86400 {
		t.Errorf("adblock provider 字段异常: %v", provider)
	}
}

func TestGenerateAdBlockDisabled(t *testing.T) {
	C.SetHomeDir(t.TempDir())

	cfg := fakeConfig()
	buf, err := Generate(cfg, nil, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	m := parseYAML(t, buf)
	if idx := ruleIndex(m, "RULE-SET,adblock,REJECT"); idx >= 0 {
		t.Errorf("关闭时不应注入广告规则: rules[%d]", idx)
	}
	if _, exists := m["rule-providers"]; exists {
		t.Errorf("关闭且未配置 rule-providers 时不应生成该段: %v", m["rule-providers"])
	}
}

func TestGenerateAdBlockProviderConflict(t *testing.T) {
	C.SetHomeDir(t.TempDir())

	cfg := fakeConfig()
	cfg.AdBlock = config.AdBlockConfig{Enable: true, RuleURL: "https://example.com/reject.txt"}
	cfg.RuleProviders = map[string]any{"adblock": map[string]any{"type": "http", "url": "https://example.com/other.txt"}}
	if _, err := Generate(cfg, nil, nil); err == nil {
		t.Fatal("用户 rule-providers 占用 adblock 保留名时应报错")
	} else if !strings.Contains(err.Error(), "adblock") {
		t.Errorf("错误应指明保留名冲突: %v", err)
	}
}
