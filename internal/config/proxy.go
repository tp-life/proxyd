package config

// 代理域配置段：订阅、分组、规则源、TUN 等类型及其校验。

import (
	"fmt"
	"strings"
)

// Subscription is one upstream subscription source.
type Subscription struct {
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url" json:"url"`
	// Type is "auto" (default, sniff content), "clash" (Clash YAML) or "share" (base64 share links).
	Type string `yaml:"type" json:"type"`
	// Enabled 使用指针区分旧配置缺失字段和用户显式关闭；缺失时保持历史默认启用行为。
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// IsEnabled 返回该订阅是否参与拉取、合并、健康检测与端口监听生成。
//
// 参数：无；接收者为待判断的订阅值对象。
//
// 返回值：
//   - bool：enabled 未配置时返回 true，显式配置时返回对应值。
//
// 错误情况：无；值对象不存在 nil 接收者场景，字段缺失按向后兼容语义处理。
func (s Subscription) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// NodeGroup 把一组节点聚合到一个指定端口。
type NodeGroup struct {
	Name         string   `yaml:"name" json:"name"`
	Port         int      `yaml:"port" json:"port"`
	Type         string   `yaml:"type,omitempty" json:"type,omitempty"`                 // url-test|fallback|load-balance；空值迁移为 url-test
	Subscription string   `yaml:"subscription,omitempty" json:"subscription,omitempty"` // 非空时成员跟随该订阅当前可用节点
	Nodes        []string `yaml:"nodes,omitempty" json:"nodes,omitempty"`               // 节点名列表；与当前可用节点取交集
}

// RuleURL 是一个远程规则源（如 gfwlist），内容跟随订阅刷新拉取，
// 只缓存到 state-dir，不持久化进配置文件。
type RuleURL struct {
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url" json:"url"`
}

// TUNConfig 描述 mihomo 的 TUN 配置段。
//
// 常用字段使用强类型，便于 proxyd 提供稳定的开关和默认值；Extra 通过 YAML
// inline 保留 mihomo 新版本增加的高级字段，避免一次 Load/Save 丢失用户手写配置。
// AutoRoute 与 AutoDetectInterface 使用指针，是为了区分“用户显式配置 false”与
// “字段缺失需要应用默认值”两种语义。
type TUNConfig struct {
	Enable              bool           `yaml:"enable" json:"enable"`
	Stack               string         `yaml:"stack" json:"stack"`
	DNSHijack           []string       `yaml:"dns-hijack" json:"dns_hijack"`
	AutoRoute           *bool          `yaml:"auto-route" json:"auto_route"`
	AutoDetectInterface *bool          `yaml:"auto-detect-interface" json:"auto_detect_interface"`
	Extra               map[string]any `yaml:",inline" json:"-"`
}

// DefaultTUNConfig 创建一份默认关闭的 TUN 配置。
//
// 参数：无。
//
// 返回值：
//   - TUNConfig：采用 system 协议栈、自动路由、自动识别出口网卡并劫持 IPv4 DNS 的配置。
//
// 错误情况：无；该函数只构造内存值。
func DefaultTUNConfig() TUNConfig {
	autoRoute := true
	autoDetectInterface := true
	return TUNConfig{
		Enable:              false,
		Stack:               "system",
		DNSHijack:           []string{"0.0.0.0:53"},
		AutoRoute:           &autoRoute,
		AutoDetectInterface: &autoDetectInterface,
	}
}

// ApplyDefaults 为缺失的 TUN 字段补齐默认值，同时保留用户显式设置的 false 和高级字段。
//
// 参数：无；接收者为待补默认值的 TUNConfig 指针。
//
// 返回值：无；直接修改接收者。
//
// 错误情况：无；字段合法性由 Validate 统一检查。
func (t *TUNConfig) ApplyDefaults() {
	defaults := DefaultTUNConfig()
	if t.Stack == "" {
		t.Stack = defaults.Stack
	}
	if t.DNSHijack == nil {
		t.DNSHijack = defaults.DNSHijack
	}
	if t.AutoRoute == nil {
		t.AutoRoute = defaults.AutoRoute
	}
	if t.AutoDetectInterface == nil {
		t.AutoDetectInterface = defaults.AutoDetectInterface
	}
}

// Clone 返回可独立修改的 TUN 配置副本，供热更新失败时回滚。
//
// 参数：无；接收者为源 TUNConfig。
//
// 返回值：
//   - TUNConfig：切片、布尔指针和 Extra map 均已复制的副本。
//
// 错误情况：无；Extra 中嵌套的复合值不会被当前开关逻辑修改，因此只复制第一层 map。
func (t TUNConfig) Clone() TUNConfig {
	out := t
	out.DNSHijack = append([]string(nil), t.DNSHijack...)
	if t.AutoRoute != nil {
		value := *t.AutoRoute
		out.AutoRoute = &value
	}
	if t.AutoDetectInterface != nil {
		value := *t.AutoDetectInterface
		out.AutoDetectInterface = &value
	}
	if t.Extra != nil {
		out.Extra = make(map[string]any, len(t.Extra))
		for key, value := range t.Extra {
			out.Extra[key] = value
		}
	}
	return out
}

// Validate 校验 proxyd 需要理解的 TUN 字段；其余透传字段交由 mihomo 自检。
//
// 参数：无；接收者为待校验的 TUNConfig。
//
// 返回值：
//   - error：stack 非 mihomo 支持值或 dns-hijack 含空条目时返回错误，否则返回 nil。
//
// 错误情况：高级 Extra 字段的类型或组合错误会在 core.Generate 的 mihomo 解析阶段返回。
func (t TUNConfig) Validate() error {
	switch t.Stack {
	case "system", "gvisor", "mixed":
	default:
		return fmt.Errorf("tun.stack %q 无效（system|gvisor|mixed）", t.Stack)
	}
	for i, address := range t.DNSHijack {
		if strings.TrimSpace(address) == "" {
			return fmt.Errorf("tun.dns-hijack[%d] 不能为空", i)
		}
	}
	return nil
}

const (
	// DNSPresetOff 禁用 proxyd 生成的 DNS 段，沿用系统 DNS 或用户手写 dns 配置。
	DNSPresetOff = "off"
	// DNSPresetFakeIP 启用 mihomo fake-ip 增强解析，适合 TUN 场景减少 DNS 泄漏。
	DNSPresetFakeIP = "fake-ip"
	// DNSPresetRedirHost 启用 redir-host 增强解析，保留真实 IP 兼容性。
	DNSPresetRedirHost = "redir-host"
)

// DefaultRules 是快捷启动（无配置文件）时使用的默认规则。
var DefaultRules = []string{
	"GEOSITE,private,DIRECT",
	"GEOIP,private,DIRECT,no-resolve",
	"GEOSITE,cn,DIRECT",
	"GEOIP,CN,DIRECT,no-resolve",
	"MATCH,PROXY",
}

// DefaultPortRange 是未指定时的默认节点映射区间。
const DefaultPortRange = "42000-42100"

// DefaultAutoPort 是开启自动选优端口时的建议端口（主端口默认 41999 的前一位）。
const DefaultAutoPort = 41998

const (
	// GroupTypeURLTest 是组内自动测速择优，保持旧版本节点分组行为。
	GroupTypeURLTest = "url-test"
	// GroupTypeFallback 是按成员顺序故障转移的 mihomo 分组类型。
	GroupTypeFallback = "fallback"
	// GroupTypeLoadBalance 是 mihomo 的负载均衡分组类型。
	GroupTypeLoadBalance = "load-balance"
)

// DefaultGeoXUrl 是默认的 geo 数据下载地址：Loyalsoldier 规则仓库（主流数据源），
// 走 jsDelivr 的 gcore 节点，解决 GitHub 直连受限时 geo 文件下载失败的问题。
// 可在配置里用 geox-url 覆盖；用户显式配置时以用户为准。
var DefaultGeoXUrl = map[string]any{
	"geosite": "https://gcore.jsdelivr.net/gh/Loyalsoldier/v2ray-rules-dat@release/geosite.dat",
	"geoip":   "https://gcore.jsdelivr.net/gh/Loyalsoldier/v2ray-rules-dat@release/geoip.dat",
	"mmdb":    "https://gcore.jsdelivr.net/gh/Loyalsoldier/geoip@release/Country.mmdb",
}

// ValidateCustomRule 宽松校验自定义规则：至少 3 段逗号分隔且各段非空
// （如 DOMAIN-SUFFIX,example.com,DIRECT）。语义正确性由 mihomo 解析兜底。
func ValidateCustomRule(rule string) error {
	parts := strings.Split(rule, ",")
	if len(parts) < 3 {
		return fmt.Errorf("规则 %q 格式应为 类型,内容,策略（至少 3 段）", rule)
	}
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("规则 %q 存在空字段", rule)
		}
	}
	return nil
}

// 分组名不能占用的 mihomo 内置/保留策略名。
var reservedGroupNames = map[string]bool{
	"DIRECT": true, "REJECT": true, "PROXY": true, "AUTO": true, "GLOBAL": true,
}

// CheckGroup 校验新增分组：字段合法，且与现有分组不重名/不重复端口。
func (c *Config) CheckGroup(g NodeGroup) error {
	if err := c.checkGroup(g); err != nil {
		return err
	}
	for _, e := range c.Groups {
		if e.Name == g.Name {
			return fmt.Errorf("分组 %q 已存在", g.Name)
		}
		if e.Port == g.Port {
			return fmt.Errorf("端口 %d 已被分组 %q 占用", g.Port, e.Name)
		}
	}
	return nil
}

// checkGroup 校验分组自身字段及与主端口/节点区间/API 端口的冲突。
func (c *Config) checkGroup(g NodeGroup) error {
	if g.Name == "" {
		return fmt.Errorf("分组名不能为空")
	}
	if g.Type == "" {
		g.Type = GroupTypeURLTest
	}
	switch g.Type {
	case GroupTypeURLTest, GroupTypeFallback, GroupTypeLoadBalance:
	default:
		return fmt.Errorf("分组类型 %q 无效（url-test|fallback|load-balance）", g.Type)
	}
	if g.Subscription == "" && len(g.Nodes) == 0 {
		return fmt.Errorf("分组 %q 必须配置 nodes 或 subscription", g.Name)
	}
	if reservedGroupNames[strings.ToUpper(g.Name)] {
		return fmt.Errorf("分组名 %q 是保留策略名", g.Name)
	}
	if g.Port <= 0 || g.Port > 65535 {
		return fmt.Errorf("分组端口 %d 超出 1-65535", g.Port)
	}
	if g.Port >= c.PortRange[0] && g.Port <= c.PortRange[1] {
		return fmt.Errorf("分组端口 %d 与节点映射区间 [%d, %d] 冲突", g.Port, c.PortRange[0], c.PortRange[1])
	}
	if g.Port == c.MixedPort {
		return fmt.Errorf("分组端口 %d 与主端口冲突", g.Port)
	}
	if p := addrPort(c.APIListen); p == g.Port {
		return fmt.Errorf("分组端口 %d 与 api-listen 冲突", g.Port)
	}
	if p := addrPort(c.ExternalController); p == g.Port {
		return fmt.Errorf("分组端口 %d 与 external-controller 冲突", g.Port)
	}
	if c.AutoPort != 0 && g.Port == c.AutoPort {
		return fmt.Errorf("分组端口 %d 与 auto-port 冲突", g.Port)
	}
	if g.Subscription != "" && !c.hasNodeSource(g.Subscription) {
		return fmt.Errorf("分组 %q 引用的订阅 %q 不存在", g.Name, g.Subscription)
	}
	return nil
}

// hasNodeSource 判断分组 subscription 是否引用了已配置的节点来源。
//
// 参数：
//   - name: string，订阅名；特殊值 `manual` 表示手动节点来源。
//
// 返回值：
//   - bool，来源存在返回 true。
//
// 错误情况：无；仅检查内存配置。
func (c *Config) hasNodeSource(name string) bool {
	if name == "manual" && len(c.ManualNodes) > 0 {
		return true
	}
	for _, sub := range c.Subscriptions {
		if sub.Name == name {
			return true
		}
	}
	return false
}

// CheckMixedPort 校验主端口：1-65535，且不得与节点映射区间、api-listen、
// external-controller、auto-port 或任一分组端口冲突。
func (c *Config) CheckMixedPort(port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("主端口 %d 超出 1-65535", port)
	}
	if port >= c.PortRange[0] && port <= c.PortRange[1] {
		return fmt.Errorf("主端口 %d 与节点映射区间 [%d, %d] 冲突", port, c.PortRange[0], c.PortRange[1])
	}
	if p := addrPort(c.APIListen); p == port {
		return fmt.Errorf("主端口 %d 与 api-listen 冲突", port)
	}
	if p := addrPort(c.ExternalController); p == port {
		return fmt.Errorf("主端口 %d 与 external-controller 冲突", port)
	}
	if c.AutoPort != 0 && port == c.AutoPort {
		return fmt.Errorf("主端口 %d 与 auto-port 冲突", port)
	}
	for _, g := range c.Groups {
		if g.Port == port {
			return fmt.Errorf("主端口 %d 与分组 %q 端口冲突", port, g.Name)
		}
	}
	return nil
}

// CheckAutoPort 校验自动选优端口：0 表示关闭；否则不得与主端口、
// 节点区间、api/external-controller 端口或分组端口冲突。
func (c *Config) CheckAutoPort(port int) error {
	if port == 0 {
		return nil
	}
	if port < 0 || port > 65535 {
		return fmt.Errorf("auto-port %d 超出 1-65535", port)
	}
	if port >= c.PortRange[0] && port <= c.PortRange[1] {
		return fmt.Errorf("auto-port %d 与节点映射区间 [%d, %d] 冲突", port, c.PortRange[0], c.PortRange[1])
	}
	if port == c.MixedPort {
		return fmt.Errorf("auto-port %d 与主端口冲突", port)
	}
	if p := addrPort(c.APIListen); p == port {
		return fmt.Errorf("auto-port %d 与 api-listen 冲突", port)
	}
	if p := addrPort(c.ExternalController); p == port {
		return fmt.Errorf("auto-port %d 与 external-controller 冲突", port)
	}
	for _, g := range c.Groups {
		if g.Port == port {
			return fmt.Errorf("auto-port %d 与分组 %q 端口冲突", port, g.Name)
		}
	}
	return nil
}

// CheckRuleURL 校验新增规则源：name/url 必填且 name 不重复。
func (c *Config) CheckRuleURL(ru RuleURL) error {
	if ru.Name == "" || ru.URL == "" {
		return fmt.Errorf("规则源 name 和 url 均为必填")
	}
	if !strings.HasPrefix(ru.URL, "http://") && !strings.HasPrefix(ru.URL, "https://") {
		return fmt.Errorf("规则源地址必须是 http(s) URL")
	}
	for _, e := range c.RuleURLs {
		if e.Name == ru.Name {
			return fmt.Errorf("规则源 %q 已存在", ru.Name)
		}
	}
	return nil
}
