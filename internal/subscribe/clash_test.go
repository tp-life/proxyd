package subscribe

import (
	"testing"
)

const clashFixture = `
proxies:
  - name: "香港 SS"
    type: ss
    server: hk.example.com
    port: 8388
    cipher: aes-256-gcm
    password: pass-hk
    udp: true
  - name: "日本 VMess"
    type: vmess
    server: jp.example.com
    port: 443
    uuid: 11111111-2222-3333-4444-555555555555
    alterId: 0
    cipher: auto
    network: ws
    tls: true
    servername: jp.example.com
    ws-opts:
      path: /ws
      headers:
        Host: cdn.example.com
  - name: "美国 Trojan"
    type: trojan
    server: us.example.com
    port: 443
    password: tj-pass
    sni: us.example.com
    skip-cert-verify: true
  - type: ss
    server: bad.example.com
    port: 1234
    cipher: aes-128-gcm
    password: no-name
`

func TestParseClash(t *testing.T) {
	nodes, err := ParseClash([]byte(clashFixture), "机场A")
	if err != nil {
		t.Fatalf("ParseClash 失败: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("期望 3 个节点（缺 name 的坏项被跳过），得到 %d", len(nodes))
	}

	ss := nodes[0]
	if ss.Name != "香港 SS" || ss.Subscription != "机场A" {
		t.Errorf("ss 节点 Name/Subscription 不正确: %+v", ss)
	}
	if ss.Mapping["name"] != ss.Name {
		t.Errorf("Mapping[name] 未与 Name 同步: %v", ss.Mapping["name"])
	}
	if ss.Mapping["type"] != "ss" || ss.Mapping["server"] != "hk.example.com" || ss.Mapping["port"] != 8388 {
		t.Errorf("ss 节点字段不正确: %+v", ss.Mapping)
	}
	if ss.Mapping["udp"] != true {
		t.Errorf("ss 节点 udp 字段应保留: %+v", ss.Mapping)
	}

	vm := nodes[1]
	if vm.Mapping["type"] != "vmess" || vm.Mapping["uuid"] != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("vmess 节点字段不正确: %+v", vm.Mapping)
	}
	ws, ok := vm.Mapping["ws-opts"].(map[string]any)
	if !ok || ws["path"] != "/ws" {
		t.Errorf("vmess ws-opts 未原样保留: %+v", vm.Mapping["ws-opts"])
	}

	tj := nodes[2]
	if tj.Mapping["type"] != "trojan" || tj.Mapping["password"] != "tj-pass" || tj.Mapping["sni"] != "us.example.com" {
		t.Errorf("trojan 节点字段不正确: %+v", tj.Mapping)
	}
}

func TestParseClashNoProxies(t *testing.T) {
	if _, err := ParseClash([]byte("port: 7890\n"), "sub"); err == nil {
		t.Fatal("缺少 proxies 时应返回错误")
	}
	if _, err := ParseClash([]byte("not yaml at all: ["), "sub"); err == nil {
		t.Fatal("非法 YAML 应返回错误")
	}
}
