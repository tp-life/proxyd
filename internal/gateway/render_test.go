package gateway

import (
	"strings"
	"testing"

	"proxyd/internal/config"
)

func testGatewayConfig() config.GatewayConfig {
	return config.GatewayConfig{
		RedirPort:   17892,
		TProxyPort:  17893,
		DNSRedirect: true,
		Devices: []config.GatewayDevice{
			{Name: "电视", IP: "192.168.1.10", Policy: "proxy"},
			{Name: "手机", IP: "192.168.1.11/32", Policy: "direct"},
		},
	}
}

func TestRenderPFAnchor(t *testing.T) {
	text := RenderPFAnchor(testGatewayConfig(), 1053)
	if !strings.Contains(text, "rdr pass inet proto tcp from { 192.168.1.10, 192.168.1.11 } to any -> 127.0.0.1 port 17892") {
		t.Errorf("缺 TCP rdr 行:\n%s", text)
	}
	if !strings.Contains(text, "rdr pass inet proto udp from { 192.168.1.10, 192.168.1.11 } to any port 53 -> 127.0.0.1 port 1053") {
		t.Errorf("缺 DNS rdr 行:\n%s", text)
	}
}

func TestRenderPFAnchorDNSOff(t *testing.T) {
	cfg := testGatewayConfig()
	cfg.DNSRedirect = false
	text := RenderPFAnchor(cfg, 1053)
	if strings.Contains(text, "port 53") {
		t.Errorf("dns-redirect 关闭时不应有 53 rdr 行:\n%s", text)
	}
	if !strings.Contains(text, "rdr pass inet proto tcp") {
		t.Errorf("TCP rdr 行应保留:\n%s", text)
	}
}

func TestRenderPFAnchorEmptyDevices(t *testing.T) {
	text := RenderPFAnchor(config.GatewayConfig{RedirPort: 17892}, 1053)
	if strings.Contains(text, "rdr ") {
		t.Errorf("空设备表不应有任何 rdr 行:\n%s", text)
	}
	if !strings.Contains(text, "设备表为空") {
		t.Errorf("空设备表应有注释说明:\n%s", text)
	}
}

func TestRenderNFTRuleset(t *testing.T) {
	text := RenderNFTRuleset(testGatewayConfig(), 1053)
	for _, want := range []string{
		"table inet proxyd-gw {",
		"type ipv4_addr",
		"elements = { 192.168.1.10, 192.168.1.11 }",
		"type nat hook prerouting priority dstnat; policy accept;",
		"ip saddr @gw_devices meta l4proto tcp redirect to :17892",
		"ip saddr @gw_devices udp dport 53 redirect to :1053",
		"ip saddr @gw_devices meta l4proto udp tproxy to :17893 meta mark set 0x1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ruleset 缺 %q:\n%s", want, text)
		}
	}
}

func TestRenderNFTRulesetEmptyDevices(t *testing.T) {
	text := RenderNFTRuleset(config.GatewayConfig{RedirPort: 17892, TProxyPort: 17893}, 1053)
	if strings.Contains(text, "elements =") {
		t.Errorf("空设备表不应有 elements 行:\n%s", text)
	}
	if !strings.Contains(text, "table inet proxyd-gw {") {
		t.Errorf("空设备表仍应渲染表骨架（保持幂等替换）:\n%s", text)
	}
}

func TestRenderNFTRulesetDefaultPorts(t *testing.T) {
	// 端口未补默认值（直接构造的配置）时按默认端口渲染。
	text := RenderNFTRuleset(config.GatewayConfig{
		Devices: []config.GatewayDevice{{Name: "a", IP: "10.0.0.1"}},
	}, 1053)
	if !strings.Contains(text, "redirect to :17892") || !strings.Contains(text, "tproxy to :17893") {
		t.Errorf("默认端口渲染异常:\n%s", text)
	}
}
