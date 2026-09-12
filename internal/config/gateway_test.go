package config

import (
	"strings"
	"testing"
)

const gatewayValidYAML = `
subscriptions:
  - name: a
    url: https://example.com/sub
port-range: [42000, 42010]
rules:
  - MATCH,PROXY
groups:
  - name: 影视
    port: 17880
    nodes: [n1]
gateway:
  redir-port: 17892
  tproxy-port: 17893
  dns-redirect: true
  devices:
    - name: 电视
      mac: aa:bb:cc:dd:ee:ff
      ip: 192.168.1.10
      policy: proxy
    - name: 手机
      ip: 192.168.1.11/32
      policy: group:影视
    - name: 打印机
      ip: 192.168.1.12
      policy: direct
`

func TestGatewayLoadValid(t *testing.T) {
	cfg, err := Load(writeTemp(t, gatewayValidYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Gateway.Disabled {
		t.Error("Disabled 默认应为 false")
	}
	if cfg.Gateway.RedirPort != 17892 || cfg.Gateway.TProxyPort != 17893 {
		t.Errorf("端口 = %d/%d", cfg.Gateway.RedirPort, cfg.Gateway.TProxyPort)
	}
	if !cfg.Gateway.DNSRedirect {
		t.Error("DNSRedirect 应为 true")
	}
	if len(cfg.Gateway.Devices) != 3 {
		t.Fatalf("devices = %+v", cfg.Gateway.Devices)
	}
}

func TestGatewayDefaultPorts(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML+`
gateway:
  devices:
    - name: 电视
      ip: 192.168.1.10
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Gateway.RedirPort != DefaultGatewayRedirPort {
		t.Errorf("RedirPort 默认 = %d", cfg.Gateway.RedirPort)
	}
	if cfg.Gateway.TProxyPort != DefaultGatewayTProxyPort {
		t.Errorf("TProxyPort 默认 = %d", cfg.Gateway.TProxyPort)
	}
}

func TestGatewayValidation(t *testing.T) {
	cases := []struct {
		name    string
		gateway string
		wantErr string
	}{
		{"设备名空", "devices: [{name: '', ip: 192.168.1.10}]", "名称不能为空"},
		{"设备名重复", "devices: [{name: a, ip: 192.168.1.10}, {name: a, ip: 192.168.1.11}]", "重复"},
		{"IP 重复（裸地址与 /32 视为同一）", "devices: [{name: a, ip: 192.168.1.10}, {name: b, ip: 192.168.1.10/32}]", "重复"},
		{"IP 非法", "devices: [{name: a, ip: 999.1.1.1}]", "不是合法地址"},
		{"IPv6 拒绝", "devices: [{name: a, ip: '::1'}]", "仅支持 IPv4"},
		{"非 /32 前缀拒绝", "devices: [{name: a, ip: 192.168.1.0/24}]", "/32"},
		{"policy 枚举", "devices: [{name: a, ip: 192.168.1.10, policy: fast}]", "policy"},
		{"group 空名", "devices: [{name: a, ip: 192.168.1.10, policy: 'group:'}]", "缺少分组名"},
		{"group 不存在", "devices: [{name: a, ip: 192.168.1.10, policy: 'group:不存在'}]", "不存在"},
		{"redir 越界", "redir-port: 70000", "超出 1-65535"},
		{"redir 撞主端口", "redir-port: 41999", "与主端口冲突"},
		{"redir 撞节点区间", "redir-port: 42005", "节点映射区间"},
		{"redir 撞 api-listen", "redir-port: 19091", "api-listen"},
		{"redir 撞 external-controller", "redir-port: 19090", "external-controller"},
		{"tproxy 撞分组端口", "tproxy-port: 17880", "分组"},
		{"redir 与 tproxy 相同", "redir-port: 17892\ntproxy-port: 17892", "不能同为"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `
subscriptions:
  - name: a
    url: https://example.com/sub
port-range: [42000, 42010]
rules:
  - MATCH,PROXY
groups:
  - name: 影视
    port: 17880
    nodes: [n1]
gateway:
  ` + strings.ReplaceAll(tc.gateway, "\n", "\n  ") + "\n"
			_, err := Load(writeTemp(t, body))
			if err == nil {
				t.Fatalf("期望错误 %q，实际通过", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 %q 不含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestGatewayDeviceRules(t *testing.T) {
	cfg, err := Load(writeTemp(t, gatewayValidYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rules := cfg.Gateway.DeviceRules()
	want := []string{
		"SRC-IP-CIDR,192.168.1.10/32,PROXY",
		"SRC-IP-CIDR,192.168.1.11/32,影视",
		"SRC-IP-CIDR,192.168.1.12/32,DIRECT",
	}
	if len(rules) != len(want) {
		t.Fatalf("DeviceRules = %v", rules)
	}
	for i := range want {
		if rules[i] != want[i] {
			t.Errorf("rules[%d] = %q, want %q", i, rules[i], want[i])
		}
	}
	// 留空 policy 默认 proxy
	empty := GatewayConfig{Devices: []GatewayDevice{{Name: "a", IP: "10.0.0.1"}}}
	if got := empty.DeviceRules(); len(got) != 1 || got[0] != "SRC-IP-CIDR,10.0.0.1/32,PROXY" {
		t.Errorf("留空 policy = %v", got)
	}
	// 空设备表
	if got := (GatewayConfig{}).DeviceRules(); got != nil {
		t.Errorf("空设备表 = %v", got)
	}
}

func TestGatewayCloneIndependent(t *testing.T) {
	cfg, err := Load(writeTemp(t, gatewayValidYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	clone := cfg.Clone()
	clone.Gateway.Devices[0].IP = "10.9.9.9"
	if cfg.Gateway.Devices[0].IP != "192.168.1.10" {
		t.Error("Clone 的 Devices 与原配置共享切片")
	}
	// 无凭据字段：RedactedCopy 不应改动 gateway 段内容
	redacted := cfg.RedactedCopy()
	if redacted.Gateway.Devices[0].IP != "192.168.1.10" || redacted.Gateway.RedirPort != 17892 {
		t.Errorf("RedactedCopy 不应打码 gateway 段: %+v", redacted.Gateway)
	}
}
