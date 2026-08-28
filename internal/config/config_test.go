package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const validYAML = `
subscriptions:
  - name: a
    url: https://example.com/sub
port-range: [42000, 42010]
rules:
  - MATCH,PROXY
`

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1" {
		t.Errorf("Listen default = %q", cfg.Listen)
	}
	if cfg.RefreshInterval.D() != 24*time.Hour {
		t.Errorf("RefreshInterval default = %v", cfg.RefreshInterval.D())
	}
	if cfg.HealthURL != defaultHealthURL {
		t.Errorf("HealthURL default = %q", cfg.HealthURL)
	}
	if cfg.Mode != "rule" {
		t.Errorf("Mode default = %q", cfg.Mode)
	}
	if cfg.MixedPort != 41999 {
		t.Errorf("MixedPort default = %d", cfg.MixedPort)
	}
	if cfg.Capacity() != 11 {
		t.Errorf("Capacity = %d", cfg.Capacity())
	}
}

func TestLoadFull(t *testing.T) {
	body := `
subscriptions:
  - name: a
    url: https://example.com/sub
    type: clash
  - name: b
    url: https://example.com/links
    type: share
listen: 0.0.0.0
port-range: [43000, 43100]
mixed-port: 17890
refresh-interval: 1h
health-interval: 10m
health-timeout: 3s
exclude: "到期|官网"
mode: global
external-controller: 127.0.0.1:9090
secret: s3cret
rules:
  - GEOIP,CN,DIRECT
  - MATCH,PROXY
`
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RefreshInterval.D() != time.Hour {
		t.Errorf("RefreshInterval = %v", cfg.RefreshInterval.D())
	}
	if cfg.MixedPort != 17890 {
		t.Errorf("MixedPort = %d", cfg.MixedPort)
	}
	if len(cfg.Subscriptions) != 2 || cfg.Subscriptions[1].Type != "share" {
		t.Errorf("subscriptions = %+v", cfg.Subscriptions)
	}
}

func TestQuick(t *testing.T) {
	cfg, err := Quick([]string{"https://a.com/sub", "https://b.com/link"}, "")
	if err != nil {
		t.Fatalf("Quick: %v", err)
	}
	if cfg.PortRange != [2]int{42000, 42100} {
		t.Errorf("default PortRange = %v", cfg.PortRange)
	}
	if cfg.MixedPort != 41999 {
		t.Errorf("MixedPort = %d", cfg.MixedPort)
	}
	if len(cfg.Subscriptions) != 2 || cfg.Subscriptions[0].Name != "sub-1" {
		t.Errorf("subscriptions = %+v", cfg.Subscriptions)
	}
	if len(cfg.Rules) == 0 {
		t.Error("default rules empty")
	}
	cfg2, err := Quick([]string{"https://a.com/sub"}, "43000-43010")
	if err != nil {
		t.Fatalf("Quick range: %v", err)
	}
	if cfg2.PortRange != [2]int{43000, 43010} {
		t.Errorf("PortRange = %v", cfg2.PortRange)
	}
	if _, err := Quick(nil, ""); err == nil {
		t.Error("Quick without urls: expected error")
	}
	if _, err := Quick([]string{"https://a.com"}, "abc"); err == nil {
		t.Error("Quick bad range: expected error")
	}
}

func TestLoadInvalid(t *testing.T) {
	cases := map[string]string{
		"no subscriptions":   "port-range: [1,2]\nrules: [MATCH,PROXY]\n",
		"empty rules":        "subscriptions: [{name: a, url: x}]\nport-range: [1,2]\n",
		"bad range":          "subscriptions: [{name: a, url: x}]\nport-range: [100,50]\nrules: [MATCH,PROXY]\n",
		"range overflow":     "subscriptions: [{name: a, url: x}]\nport-range: [1,70000]\nrules: [MATCH,PROXY]\n",
		"mixed inside range": "subscriptions: [{name: a, url: x}]\nport-range: [100,200]\nmixed-port: 150\nrules: [MATCH,PROXY]\n",
		"dup sub name":       "subscriptions: [{name: a, url: x},{name: a, url: y}]\nport-range: [1,2]\nrules: [MATCH,PROXY]\n",
		"bad sub type":       "subscriptions: [{name: a, url: x, type: wat}]\nport-range: [1,2]\nrules: [MATCH,PROXY]\n",
		"bad mode":           "subscriptions: [{name: a, url: x}]\nport-range: [1,2]\nmode: wat\nrules: [MATCH,PROXY]\n",
		"bad exclude":        "subscriptions: [{name: a, url: x}]\nport-range: [1,2]\nexclude: '['\nrules: [MATCH,PROXY]\n",
		"bad duration":       "subscriptions: [{name: a, url: x}]\nport-range: [1,2]\nrefresh-interval: someday\nrules: [MATCH,PROXY]\n",
	}
	for name, body := range cases {
		if _, err := Load(writeTemp(t, body)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestLoadNewFields(t *testing.T) {
	body := `
subscriptions:
  - name: a
    url: https://example.com/sub
port-range: [42000, 42010]
auto-port: 41998
system-proxy: true
custom-rules:
  - DOMAIN-SUFFIX,example.com,DIRECT
rule-urls:
  - name: gfwlist
    url: https://example.com/gfwlist.txt
groups:
  - name: hk
    port: 43000
    nodes: ["香港 01", "香港 02"]
rules:
  - MATCH,PROXY
`
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AutoPort != 41998 {
		t.Errorf("AutoPort = %d", cfg.AutoPort)
	}
	if !cfg.SystemProxy {
		t.Error("SystemProxy = false")
	}
	if len(cfg.CustomRules) != 1 || cfg.CustomRules[0] != "DOMAIN-SUFFIX,example.com,DIRECT" {
		t.Errorf("CustomRules = %v", cfg.CustomRules)
	}
	if len(cfg.RuleURLs) != 1 || cfg.RuleURLs[0].Name != "gfwlist" {
		t.Errorf("RuleURLs = %+v", cfg.RuleURLs)
	}
	if len(cfg.Groups) != 1 || cfg.Groups[0].Name != "hk" || cfg.Groups[0].Port != 43000 ||
		len(cfg.Groups[0].Nodes) != 2 {
		t.Errorf("Groups = %+v", cfg.Groups)
	}
}

func TestMainNodePersist(t *testing.T) {
	// main-node（节点 Key）落盘并原样读回；空值默认不写出（omitempty）。
	body := `
subscriptions:
  - name: a
    url: https://example.com/sub
port-range: [42000, 42010]
main-node: "socks5|1.2.3.4|10001|p"
rules:
  - MATCH,PROXY
`
	path := writeTemp(t, body)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MainNode != "socks5|1.2.3.4|10001|p" {
		t.Fatalf("MainNode = %q", cfg.MainNode)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	if cfg2.MainNode != cfg.MainNode {
		t.Errorf("main-node 持久化往返不一致: %q vs %q", cfg2.MainNode, cfg.MainNode)
	}

	cfg.MainNode = ""
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "main-node") {
		t.Errorf("main-node 为空时不应写出: %s", raw)
	}
}

func TestMigrateModeAuto(t *testing.T) {
	// 旧配置 mode: auto 迁移为 rule + auto-port
	body := `
subscriptions:
  - name: a
    url: https://example.com/sub
port-range: [42000, 42010]
mode: auto
rules:
  - MATCH,PROXY
`
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != "rule" {
		t.Errorf("Mode = %q, want rule（auto 已迁移）", cfg.Mode)
	}
	if cfg.AutoPort != DefaultAutoPort {
		t.Errorf("AutoPort = %d, want %d", cfg.AutoPort, DefaultAutoPort)
	}
}

func TestCheckAutoPort(t *testing.T) {
	cfg := &Config{
		Listen:             "127.0.0.1",
		PortRange:          [2]int{42000, 42100},
		MixedPort:          41999,
		APIListen:          "127.0.0.1:19091",
		ExternalController: "127.0.0.1:19090",
		Groups:             []NodeGroup{{Name: "hk", Port: 43000}},
	}
	if err := cfg.CheckAutoPort(0); err != nil {
		t.Errorf("CheckAutoPort(0 关闭): %v", err)
	}
	if err := cfg.CheckAutoPort(41998); err != nil {
		t.Errorf("CheckAutoPort(41998): %v", err)
	}
	for name, port := range map[string]int{
		"负端口": -1, "超界": 70000, "区间内": 42050, "主端口": 41999,
		"api": 19091, "external": 19090, "分组端口": 43000,
	} {
		if err := cfg.CheckAutoPort(port); err == nil {
			t.Errorf("CheckAutoPort(%s=%d): expected error", name, port)
		}
	}
}

func TestCheckMixedPort(t *testing.T) {
	cfg := &Config{
		Listen:             "127.0.0.1",
		PortRange:          [2]int{42000, 42100},
		MixedPort:          41999,
		AutoPort:           41998,
		APIListen:          "127.0.0.1:19091",
		ExternalController: "127.0.0.1:19090",
		Groups:             []NodeGroup{{Name: "hk", Port: 43000}},
	}
	if err := cfg.CheckMixedPort(42999); err != nil {
		t.Errorf("CheckMixedPort(42999): %v", err)
	}
	for name, port := range map[string]int{
		"零": 0, "负端口": -1, "超界": 70000, "区间起": 42000, "区间内": 42050, "区间止": 42100,
		"api": 19091, "external": 19090, "auto-port": 41998, "分组端口": 43000,
	} {
		if err := cfg.CheckMixedPort(port); err == nil {
			t.Errorf("CheckMixedPort(%s=%d): expected error", name, port)
		}
	}
}

func TestParsePortRange(t *testing.T) {
	if r, err := ParsePortRange("43000-43200"); err != nil || r != [2]int{43000, 43200} {
		t.Errorf("ParsePortRange = %v, %v", r, err)
	}
	for _, bad := range []string{"", "abc", "43000", "43000-", "0-100", "100-0", "1-70000", "-5-10"} {
		if _, err := ParsePortRange(bad); err == nil {
			t.Errorf("ParsePortRange(%q): expected error", bad)
		}
	}
}

func TestValidateCustomRule(t *testing.T) {
	for _, ok := range []string{
		"DOMAIN-SUFFIX,example.com,DIRECT",
		"IP-CIDR,10.0.0.0/8,REJECT,no-resolve",
		"GEOSITE,cn,香港 01",
	} {
		if err := ValidateCustomRule(ok); err != nil {
			t.Errorf("ValidateCustomRule(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "MATCH,PROXY", "DOMAIN-SUFFIX,,DIRECT", "a,b"} {
		if err := ValidateCustomRule(bad); err == nil {
			t.Errorf("ValidateCustomRule(%q): expected error", bad)
		}
	}
}

func TestCheckGroup(t *testing.T) {
	cfg := &Config{
		Listen:             "127.0.0.1",
		PortRange:          [2]int{42000, 42100},
		MixedPort:          41999,
		APIListen:          "127.0.0.1:19091",
		ExternalController: "127.0.0.1:19090",
	}
	ok := NodeGroup{Name: "hk", Port: 43000, Nodes: []string{"n1"}}
	if err := cfg.CheckGroup(ok); err != nil {
		t.Errorf("CheckGroup(valid): %v", err)
	}
	cases := map[string]NodeGroup{
		"empty name":    {Name: "", Port: 43000},
		"reserved name": {Name: "AUTO", Port: 43000},
		"bad port":      {Name: "g", Port: 0},
		"in port-range": {Name: "g", Port: 42050},
		"mixed port":    {Name: "g", Port: 41999},
		"api port":      {Name: "g", Port: 19091},
		"external port": {Name: "g", Port: 19090},
		"dup name":      {Name: "hk", Port: 43001},
		"dup port":      {Name: "g2", Port: 43000},
	}
	cfg.Groups = []NodeGroup{ok}
	for name, g := range cases {
		if err := cfg.CheckGroup(g); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
