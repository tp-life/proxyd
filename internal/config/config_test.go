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
	if cfg.DNSPreset != DNSPresetOff {
		t.Errorf("DNSPreset default = %q", cfg.DNSPreset)
	}
	if !cfg.UpdateCheckEnabled() || cfg.CheckUpdates == nil {
		t.Errorf("CheckUpdates 默认值异常: %#v", cfg.CheckUpdates)
	}
	if !cfg.PortMappingEnabled() || cfg.PortMapping == nil {
		t.Errorf("PortMapping 默认值异常: %#v", cfg.PortMapping)
	}
	if cfg.TUN.Enable || cfg.TUN.Stack != "system" || cfg.TUN.AutoRoute == nil || !*cfg.TUN.AutoRoute ||
		cfg.TUN.AutoDetectInterface == nil || !*cfg.TUN.AutoDetectInterface ||
		len(cfg.TUN.DNSHijack) != 1 || cfg.TUN.DNSHijack[0] != "0.0.0.0:53" {
		t.Errorf("TUN 默认值异常: %+v", cfg.TUN)
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
check-updates: false
port-mapping: false
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
	if cfg.UpdateCheckEnabled() {
		t.Error("check-updates: false 被默认值覆盖")
	}
	if cfg.PortMappingEnabled() {
		t.Error("port-mapping: false 被默认值覆盖")
	}
}

// TestSubscriptionEnabledDefaultsAndExplicitDisable 验证订阅启用状态的向后兼容语义：
// 旧配置没有 enabled 字段时默认启用，用户显式写 false 时必须保持关闭。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于报告状态断言失败。
//
// 返回值：无。
//
// 错误情况：字段缺失被误判为关闭，或显式 false 被默认值覆盖时测试失败。
func TestSubscriptionEnabledDefaultsAndExplicitDisable(t *testing.T) {
	legacy := Subscription{Name: "legacy", URL: "https://example.com/sub", Type: "auto"}
	if !legacy.IsEnabled() {
		t.Fatal("旧订阅缺少 enabled 字段时应默认启用")
	}
	disabled := false
	explicit := Subscription{Name: "disabled", URL: "https://example.com/sub", Type: "auto", Enabled: &disabled}
	if explicit.IsEnabled() {
		t.Fatal("显式 enabled=false 的订阅不应被默认值重新开启")
	}
}

// TestPortMappingEnabledFor 验证订阅级端口映射判定：字段缺失默认开启、显式关闭生效、
// 非订阅来源（手动节点 manual）与未知来源只跟随全局开关。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于报告判定断言失败。
//
// 返回值：无。
//
// 错误情况：缺失字段被误判为关闭、显式 false 未生效，或 manual 被误判为关闭时测试失败。
func TestPortMappingEnabledFor(t *testing.T) {
	disabled := false
	cfg := &Config{Subscriptions: []Subscription{
		{Name: "legacy", URL: "https://example.com/a"},
		{Name: "off", URL: "https://example.com/b", PortMapping: &disabled},
	}}
	if !cfg.PortMappingEnabledFor("legacy") {
		t.Fatal("缺少 port-mapping 字段的订阅应默认参与端口映射")
	}
	if cfg.PortMappingEnabledFor("off") {
		t.Fatal("显式 port-mapping=false 的订阅不应参与端口映射")
	}
	if !cfg.PortMappingEnabledFor("manual") {
		t.Fatal("手动节点没有订阅级开关，应默认参与端口映射")
	}
	if !cfg.PortMappingEnabledFor("ghost") {
		t.Fatal("未知来源应按开启处理，避免异常数据意外停掉监听")
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
		"bad dns preset":     "subscriptions: [{name: a, url: x}]\nport-range: [1,2]\ndns-preset: wat\nrules: [MATCH,PROXY]\n",
		"bad exclude":        "subscriptions: [{name: a, url: x}]\nport-range: [1,2]\nexclude: '['\nrules: [MATCH,PROXY]\n",
		"bad include":        "subscriptions: [{name: a, url: x}]\nport-range: [1,2]\ninclude: '['\nrules: [MATCH,PROXY]\n",
		"bad duration":       "subscriptions: [{name: a, url: x}]\nport-range: [1,2]\nrefresh-interval: someday\nrules: [MATCH,PROXY]\n",
	}
	for name, body := range cases {
		if _, err := Load(writeTemp(t, body)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// TestValidateAllowsZeroValueOptionalDefaults 验证直接构造 Config 时可省略有默认值的可选字段。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：空 dns-preset 或空 TUN 段被当成非法值时测试失败；这会破坏嵌入调用方
// 和 e2e 直接构造配置的既有入口。
func TestValidateAllowsZeroValueOptionalDefaults(t *testing.T) {
	cfg := &Config{
		Subscriptions: []Subscription{{Name: "a", URL: "https://example.com/sub"}},
		Listen:        "127.0.0.1",
		PortRange:     [2]int{42000, 42010},
		MixedPort:     41999,
		Mode:          "rule",
		Rules:         []string{"MATCH,PROXY"},
		APIListen:     "127.0.0.1:19091",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate zero-value optional defaults: %v", err)
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
    type: fallback
    subscription: a
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
		cfg.Groups[0].Type != GroupTypeFallback || cfg.Groups[0].Subscription != "a" ||
		len(cfg.Groups[0].Nodes) != 2 {
		t.Errorf("Groups = %+v", cfg.Groups)
	}
}

// TestExportYAMLRedactsCredentials 验证默认分享用导出不会泄露已知凭据，同时完整备份保持可恢复性。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：secret、URL 用户信息、敏感查询参数或编码型分享链接出现在打码结果中，
// 或完整备份丢失原始值时测试失败。
func TestExportYAMLRedactsCredentials(t *testing.T) {
	cfg, err := Parse([]byte(`
subscriptions:
  - name: private
    url: https://alice:password@example.com/api/path-secret?token=sub-secret&region=hk
manual-nodes:
  - ss://encoded-node-secret
rule-urls:
  - name: private-rules
    url: https://rules.example.com/list?api_key=rules-secret
rule-providers:
  private:
    type: http
    url: https://provider.example.com/list?auth=provider-secret
    authorization: bearer-secret
dns:
  nameserver:
    - quic://dns.example.com:853?token=quic-secret
secret: controller-secret
port-range: [42000, 42010]
rules:
  - MATCH,PROXY
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	masked, err := cfg.ExportYAML(true)
	if err != nil {
		t.Fatalf("ExportYAML(masked): %v", err)
	}
	maskedText := string(masked)
	for _, leaked := range []string{"alice", "password", "path-secret", "sub-secret", "region=hk", "encoded-node-secret", "rules-secret", "provider-secret", "bearer-secret", "quic-secret", "controller-secret"} {
		if strings.Contains(maskedText, leaked) {
			t.Errorf("打码导出泄露 %q:\n%s", leaked, maskedText)
		}
	}
	if !strings.Contains(maskedText, redactValue) {
		t.Fatalf("打码导出未包含替代标记:\n%s", maskedText)
	}

	backup, err := cfg.ExportYAML(false)
	if err != nil {
		t.Fatalf("ExportYAML(backup): %v", err)
	}
	backupText := string(backup)
	for _, original := range []string{"alice:password", "path-secret", "sub-secret", "region=hk", "encoded-node-secret", "rules-secret", "provider-secret", "bearer-secret", "quic-secret", "controller-secret"} {
		if !strings.Contains(backupText, original) {
			t.Errorf("完整备份缺少 %q:\n%s", original, backupText)
		}
	}
}

// TestTUNConfigPassThrough 验证 TUN 常用字段、显式 false 和未知高级字段可完整落盘回读。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：默认值覆盖显式 false、inline 字段丢失或 Save/Load 改变语义时测试失败。
func TestTUNConfigPassThrough(t *testing.T) {
	body := validYAML + `
tun:
  enable: false
  stack: system
  dns-hijack: []
  auto-route: false
  auto-detect-interface: false
  strict-route: true
`
	path := writeTemp(t, body)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TUN.Stack != "system" || cfg.TUN.AutoRoute == nil || *cfg.TUN.AutoRoute ||
		cfg.TUN.AutoDetectInterface == nil || *cfg.TUN.AutoDetectInterface {
		t.Fatalf("TUN 显式配置被默认值覆盖: %+v", cfg.TUN)
	}
	if cfg.TUN.DNSHijack == nil || len(cfg.TUN.DNSHijack) != 0 {
		t.Fatalf("空 dns-hijack 应保持为空切片: %#v", cfg.TUN.DNSHijack)
	}
	if strictRoute, ok := cfg.TUN.Extra["strict-route"].(bool); !ok || !strictRoute {
		t.Fatalf("strict-route 未进入透传字段: %#v", cfg.TUN.Extra)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if strictRoute, ok := reloaded.TUN.Extra["strict-route"].(bool); !ok || !strictRoute {
		t.Fatalf("Save/Load 后 strict-route 丢失: %#v", reloaded.TUN.Extra)
	}
}

// TestGroupDefaultType 验证旧配置的节点分组会默认迁移为 url-test。
//
// 这是 A2 的向后兼容边界：旧版本只支持 url-test，缺省 type 必须保持原行为，
// 否则用户升级后分组选择策略会被静默改变。
func TestGroupDefaultType(t *testing.T) {
	body := `
subscriptions:
  - name: a
    url: https://example.com/sub
port-range: [42000, 42010]
groups:
  - name: hk
    port: 43000
    nodes: ["香港 01"]
rules:
  - MATCH,PROXY
`
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Groups[0].Type != GroupTypeURLTest {
		t.Fatalf("旧分组缺省 type 应迁移为 url-test，得到 %q", cfg.Groups[0].Type)
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

func TestMigrateLegacyHealthURL(t *testing.T) {
	// 旧默认 HTTP 探测地址迁移为 HTTPS；用户自定义的其他地址保持不变
	body := `
subscriptions:
  - name: a
    url: https://example.com/sub
port-range: [42000, 42010]
health-url: http://www.gstatic.com/generate_204
rules:
  - MATCH,PROXY
`
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HealthURL != defaultHealthURL {
		t.Errorf("HealthURL = %q, want 迁移后的默认值 %q", cfg.HealthURL, defaultHealthURL)
	}

	custom := strings.Replace(body, "health-url: http://www.gstatic.com/generate_204", "health-url: http://cp.cloudflare.com/generate_204", 1)
	cfg, err = Load(writeTemp(t, custom))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HealthURL != "http://cp.cloudflare.com/generate_204" {
		t.Errorf("自定义 health-url 不应被迁移，得到 %q", cfg.HealthURL)
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

// TestRemoteAllowLegacyScalar 验证白名单旧格式（纯字符串列表）兼容解析为条目。
func TestRemoteAllowLegacyScalar(t *testing.T) {
	cfg, err := Parse([]byte(`
manual-nodes:
  - socks5://127.0.0.1:1080#x
port-range: [42000, 42010]
rules:
  - MATCH,PROXY
remote:
  allow:
    - nodekey:aaa
    - name: 家里
      key: nodekey:bbb
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Remote.Allow) != 2 {
		t.Fatalf("allow 条目数 = %d", len(cfg.Remote.Allow))
	}
	if cfg.Remote.Allow[0].Key != "nodekey:aaa" || cfg.Remote.Allow[0].Name != "" {
		t.Fatalf("旧格式字符串条目解析错误: %+v", cfg.Remote.Allow[0])
	}
	if cfg.Remote.Allow[1].Key != "nodekey:bbb" || cfg.Remote.Allow[1].Name != "家里" {
		t.Fatalf("新格式条目解析错误: %+v", cfg.Remote.Allow[1])
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
		"no members":    {Name: "g3", Port: 43003},
		"bad type":      {Name: "g3", Port: 43003, Type: "select"},
		"bad sub":       {Name: "g4", Port: 43004, Subscription: "missing"},
	}
	cfg.Subscriptions = []Subscription{{Name: "a", URL: "https://example.com/sub"}}
	cfg.Groups = []NodeGroup{ok}
	for name, g := range cases {
		if err := cfg.CheckGroup(g); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// minimalValidConfig 返回通过 Validate 的最小配置，供 remote 段校验测试复用。
func minimalValidConfig() *Config {
	return &Config{
		Subscriptions: []Subscription{{Name: "a", URL: "https://example.com/sub"}},
		Listen:        "127.0.0.1",
		PortRange:     [2]int{42000, 42010},
		MixedPort:     41999,
		Mode:          "rule",
		Rules:         []string{"MATCH,PROXY"},
		APIListen:     "127.0.0.1:19091",
	}
}

func TestRemoteConfigValidate(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour)
	cfg := minimalValidConfig()
	cfg.Remote = RemoteConfig{
		Enabled: true,
		Serve:   []int{22},
		Allow:   []RemoteAllowEntry{{Name: "laptop", Key: "nodekey:abc", ExpiresAt: &expiresAt, Ports: []int{22}}},
		Remotes: []RemotePeer{{Name: "nas", Token: "tcAAA"}},
		Forwards: []RemoteForward{
			{Name: "nas-ssh", Listen: "127.0.0.1:2222", Remote: "nas", RemotePort: 22},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid remote section rejected: %v", err)
	}

	bad := []RemoteConfig{
		{Serve: []int{0}},      // 端口越界
		{Serve: []int{22, 22}}, // 端口重复
		{Allow: []RemoteAllowEntry{{Key: "not-a-nodekey"}}},                                       // 公钥缺 nodekey: 前缀
		{Allow: []RemoteAllowEntry{{Key: "nodekey:abc"}, {Key: "nodekey:abc"}}},                   // 公钥重复
		{Allow: []RemoteAllowEntry{{Name: "x", Key: "nodekey:a"}, {Name: "x", Key: "nodekey:b"}}}, // 别名重复
		{Allow: []RemoteAllowEntry{{Key: "nodekey:a", ExpiresAt: new(time.Time)}}},                // 到期时间零值
		{Allow: []RemoteAllowEntry{{Key: "nodekey:a", Ports: []int{0}}}},                          // 授权端口越界
		{Allow: []RemoteAllowEntry{{Key: "nodekey:a", Ports: []int{22, 22}}}},                     // 授权端口重复
		{TempKey: "not-a-nodekey"},                                                                     // 临时身份公钥缺 nodekey: 前缀
		{Remotes: []RemotePeer{{Name: "", Token: "tcA"}}},                                              // 空名称
		{Remotes: []RemotePeer{{Name: "a", Token: ""}}},                                                // 空 token
		{Remotes: []RemotePeer{{Name: "a", Token: "tcA"}, {Name: "a", Token: "tcB"}}},                  // 名称重复
		{Forwards: []RemoteForward{{Name: "", Listen: "127.0.0.1:2222", Remote: "x", RemotePort: 22}}}, // 转发空名称
		{Forwards: []RemoteForward{{Name: "f", Listen: "127.0.0.1:2222", Remote: "", RemotePort: 22}}}, // 空 remote
		{Forwards: []RemoteForward{{Name: "f", Listen: "127.0.0.1:2222", Remote: "x", RemotePort: 0}}}, // 远端端口越界
		{Forwards: []RemoteForward{{Name: "f", Listen: "bad addr", Remote: "x", RemotePort: 22}}},      // 监听地址非法
		{Forwards: []RemoteForward{ // 转发重名
			{Name: "f", Listen: "127.0.0.1:2222", Remote: "x", RemotePort: 22},
			{Name: "f", Listen: "127.0.0.1:2223", Remote: "x", RemotePort: 22},
		}},
		{Forwards: []RemoteForward{ // 监听重复（"2223" 归一化为 127.0.0.1:2223）
			{Name: "f1", Listen: "2223", Remote: "x", RemotePort: 22},
			{Name: "f2", Listen: "127.0.0.1:2223", Remote: "x", RemotePort: 22},
		}},
	}
	for i, r := range bad {
		c := minimalValidConfig()
		c.Remote = r
		if err := c.Validate(); err == nil {
			t.Errorf("bad[%d]: expected error, got nil (%+v)", i, r)
		}
	}
}

// TestRemoteAllowEntryPolicy 验证 TTL 边界、端口授权语义和配置事务深拷贝。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：永久授权被误判过期、到期边界放行、端口越权或 Clone 共享底层数据时测试失败。
func TestRemoteAllowEntryPolicy(t *testing.T) {
	expiresAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	entry := RemoteAllowEntry{Key: "nodekey:test", ExpiresAt: &expiresAt, Ports: []int{22, 443}}
	if entry.IsExpired(expiresAt.Add(-time.Nanosecond)) {
		t.Fatal("授权不应在 expires-at 之前过期")
	}
	if !entry.IsExpired(expiresAt) {
		t.Fatal("授权应在 expires-at 边界立即过期")
	}
	if !entry.AllowsPort(22) || entry.AllowsPort(80) {
		t.Fatal("非空 ports 必须仅允许明确列出的端口")
	}
	if !(RemoteAllowEntry{}).AllowsPort(65535) {
		t.Fatal("空 ports 应允许全部有效服务端端口")
	}

	clone := (RemoteConfig{Allow: []RemoteAllowEntry{entry}}).Clone()
	clone.Allow[0].Ports[0] = 80
	cloneTime := clone.Allow[0].ExpiresAt.Add(time.Hour)
	clone.Allow[0].ExpiresAt = &cloneTime
	if entry.Ports[0] != 22 || !entry.ExpiresAt.Equal(expiresAt) {
		t.Fatal("RemoteConfig.Clone 不得共享授权端口切片或到期时间指针")
	}
	if !(RemoteConfig{Allow: []RemoteAllowEntry{entry}}).ClientWhitelistEnabled() {
		t.Fatal("旧配置只要存在 allow 条目就必须保持白名单模式")
	}
	if !(RemoteConfig{AllowRestricted: true}).ClientWhitelistEnabled() {
		t.Fatal("授权清扫为空后必须用 allow-restricted 保持拒绝模式")
	}
}

// TestIsLoopbackAPIListen 验证 Web 终端安全门只信任明确的回环监听地址。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：通配地址或普通网卡地址被误判为回环会使高权限终端绕过二次确认；
// 合法 IPv4/IPv6 回环被拒则会造成不必要的操作阻塞。
func TestIsLoopbackAPIListen(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:19091": true,
		"127.20.30.40:80": true,
		"[::1]:19091":     true,
		"localhost:19091": true,
		":19091":          false,
		"0.0.0.0:19091":   false,
		"[::]:19091":      false,
		"192.0.2.8:19091": false,
		"invalid":         false,
	}
	for listen, expected := range cases {
		if actual := IsLoopbackAPIListen(listen); actual != expected {
			t.Errorf("IsLoopbackAPIListen(%q) = %v, want %v", listen, actual, expected)
		}
	}
}

func TestNormalizeRemoteListen(t *testing.T) {
	cases := map[string]string{
		"2222":           "127.0.0.1:2222",
		"127.0.0.1:2222": "127.0.0.1:2222",
		" 0.0.0.0:8022 ": "0.0.0.0:8022",
	}
	for in, want := range cases {
		got, err := NormalizeRemoteListen(in)
		if err != nil || got != want {
			t.Errorf("NormalizeRemoteListen(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "127.0.0.1:99999"} {
		if _, err := NormalizeRemoteListen(bad); err == nil {
			t.Errorf("NormalizeRemoteListen(%q): expected error", bad)
		}
	}
}

func TestRemoteRedaction(t *testing.T) {
	cfg := minimalValidConfig()
	desktopToken := "tcAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	cfg.Remote = RemoteConfig{
		Remotes:  []RemotePeer{{Name: "nas", Token: "tcSECRET"}},
		Forwards: []RemoteForward{{Name: "f", Listen: "2222", Remote: "tcRAWTOKEN", RemotePort: 22}},
	}
	cfg.Desktop = DesktopConfig{Connections: []DesktopConnection{{
		Name: "旧桌面档案", Remote: desktopToken, Protocol: DesktopProtocolRDP, RemotePort: 3389,
	}}}
	redacted := cfg.RedactedCopy()
	if redacted.Remote.Remotes[0].Token != redactValue {
		t.Errorf("remote token not redacted: %q", redacted.Remote.Remotes[0].Token)
	}
	if redacted.Remote.Forwards[0].Remote != redactValue {
		t.Errorf("raw token in forward not redacted: %q", redacted.Remote.Forwards[0].Remote)
	}
	if redacted.Desktop.Connections[0].Remote != redactValue {
		t.Errorf("桌面档案中的疑似 token 未打码: %q", redacted.Desktop.Connections[0].Remote)
	}
	// 原配置不被修改。
	if cfg.Remote.Remotes[0].Token != "tcSECRET" {
		t.Error("RedactedCopy mutated the source config")
	}
	if cfg.Desktop.Connections[0].Remote != desktopToken {
		t.Error("RedactedCopy 修改了源桌面配置")
	}
}
