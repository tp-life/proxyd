package core

// gateway 生成集成用例：设备规则顺序、redir-port 写入、dns listen 注入与 proxy 停用门。

import (
	"strings"
	"testing"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub/executor"

	"proxyd/internal/config"
)

// fakeGatewayConfig 在 fakeConfig 基础上开启 gateway 与设备表。
func fakeGatewayConfig() *config.Config {
	cfg := fakeConfig()
	cfg.CustomRules = []string{"DOMAIN-SUFFIX,custom.example,DIRECT"}
	cfg.Gateway = config.GatewayConfig{
		RedirPort:   17892,
		DNSRedirect: true,
		Devices: []config.GatewayDevice{
			{Name: "电视", IP: "192.168.1.10", Policy: "proxy"},
			{Name: "打印机", IP: "192.168.1.12", Policy: "direct"},
		},
	}
	return cfg
}

func TestGenerateGateway(t *testing.T) {
	C.SetHomeDir(t.TempDir())
	cfg := fakeGatewayConfig()
	assigns := []Assignment{{Port: 42001, Node: fakeSocks5("节点A", "1.2.3.4", 10001)}}

	buf, err := Generate(cfg, assigns, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	// 独立再过一次 mihomo 自检（redir-port/dns listen 语义合法）。
	if _, err := executor.ParseWithBytes(buf); err != nil {
		t.Fatalf("mihomo 自检失败: %v", err)
	}
	m := parseYAML(t, buf)

	if m["redir-port"] != 17892 {
		t.Errorf("redir-port = %v", m["redir-port"])
	}

	// 设备规则前置于 custom-rules。
	rulesAny, _ := m["rules"].([]any)
	var rules []string
	for _, r := range rulesAny {
		rules = append(rules, r.(string))
	}
	idx := func(prefix string) int {
		for i, r := range rules {
			if strings.HasPrefix(r, prefix) {
				return i
			}
		}
		return -1
	}
	devIdx := idx("SRC-IP-CIDR,192.168.1.10/32,PROXY")
	if devIdx != 0 {
		t.Errorf("首条设备规则位置 = %d, rules = %v", devIdx, rules)
	}
	if rules[1] != "SRC-IP-CIDR,192.168.1.12/32,DIRECT" {
		t.Errorf("rules[1] = %q", rules[1])
	}
	if customIdx := idx("DOMAIN-SUFFIX,custom.example"); customIdx <= devIdx {
		t.Errorf("custom-rules 未排在设备规则之后: dev=%d custom=%d", devIdx, customIdx)
	}

	// dns-redirect：无手写 dns 段时生成带 listen 与默认 nameserver 的段。
	dns, ok := m["dns"].(map[string]any)
	if !ok {
		t.Fatalf("dns 段缺失: %v", m["dns"])
	}
	if dns["listen"] != "0.0.0.0:1053" {
		t.Errorf("dns.listen = %v", dns["listen"])
	}
	if dns["enable"] != true {
		t.Errorf("dns.enable = %v", dns["enable"])
	}
	if ns, _ := dns["nameserver"].([]any); len(ns) == 0 {
		t.Error("dns.nameserver 为空会导致 mihomo 自检失败")
	}
}

func TestGenerateGatewayKeepsUserDNSListen(t *testing.T) {
	C.SetHomeDir(t.TempDir())
	cfg := fakeGatewayConfig()
	cfg.DNS = map[string]any{"enable": true, "listen": "0.0.0.0:5335", "nameserver": []string{"223.5.5.5"}}
	buf, err := Generate(cfg, nil, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	dns := parseYAML(t, buf)["dns"].(map[string]any)
	if dns["listen"] != "0.0.0.0:5335" {
		t.Errorf("手写 dns listen 被覆盖: %v", dns["listen"])
	}
}

func TestGenerateGatewayDisabledOrProxySuspended(t *testing.T) {
	C.SetHomeDir(t.TempDir())
	assigns := []Assignment{{Port: 42001, Node: fakeSocks5("节点A", "1.2.3.4", 10001)}}

	// gateway 停用：不出现 gateway 字段与设备规则。
	cfg := fakeGatewayConfig()
	cfg.Gateway.Disabled = true
	buf, err := Generate(cfg, assigns, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	m := parseYAML(t, buf)
	if _, ok := m["redir-port"]; ok {
		t.Error("gateway 停用时不应写 redir-port")
	}
	if _, ok := m["dns"]; ok {
		t.Error("gateway 停用且无手写/预设 dns 时不应生成 dns 段")
	}
	for _, r := range m["rules"].([]any) {
		if strings.HasPrefix(r.(string), "SRC-IP-CIDR") {
			t.Errorf("gateway 停用时不应有设备规则: %v", r)
		}
	}

	// proxy 停用（直接构造的 Config 也会经 generate 时防御）：gateway 整体不生效。
	cfg = fakeGatewayConfig()
	cfg.ProxyDisabled = true
	buf, err = Generate(cfg, assigns, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	m = parseYAML(t, buf)
	if _, ok := m["redir-port"]; ok {
		t.Error("proxy 停用时不应写 redir-port")
	}
	for _, r := range m["rules"].([]any) {
		if strings.HasPrefix(r.(string), "SRC-IP-CIDR") {
			t.Errorf("proxy 停用时不应有设备规则: %v", r)
		}
	}
}

func TestGenerateGatewaySkipsRuleWithUnavailableGroup(t *testing.T) {
	C.SetHomeDir(t.TempDir())
	cfg := fakeGatewayConfig()
	// 分组存在但成员与可用节点无交集 → gen 跳过该组 → 引用它的设备规则也跳过。
	cfg.Groups = []config.NodeGroup{{Name: "影视", Port: 17880, Nodes: []string{"不存在的节点"}}}
	cfg.Gateway.Devices = append(cfg.Gateway.Devices,
		config.GatewayDevice{Name: "手机", IP: "192.168.1.11", Policy: "group:影视"})
	buf, err := Generate(cfg, nil, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	for _, r := range parseYAML(t, buf)["rules"].([]any) {
		if strings.Contains(r.(string), "影视") {
			t.Errorf("引用失效分组的设备规则应被跳过: %v", r)
		}
	}
}
