// Package config loads and validates proxyd's own configuration file.
package config

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from strings like "30m".
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler（输出 "30m" 形式，供 Save 使用）。
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// DefaultPath 是未指定 -c 时使用的默认配置文件路径（~/.config/proxyd/config.yaml）。
func DefaultPath() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "proxyd", "config.yaml")
	}
	return "proxyd.yaml"
}

// Save 把配置写回 YAML 文件（临时文件 + rename，防止写坏）。
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Subscription is one upstream subscription source.
type Subscription struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// Type is "auto" (default, sniff content), "clash" (Clash YAML) or "share" (base64 share links).
	Type string `yaml:"type"`
}

// NodeGroup 把一组节点聚合到一个指定端口：组内 url-test 自动选优。
type NodeGroup struct {
	Name  string   `yaml:"name" json:"name"`
	Port  int      `yaml:"port" json:"port"`
	Nodes []string `yaml:"nodes" json:"nodes"` // 节点名列表；与当前可用节点取交集
}

// RuleURL 是一个远程规则源（如 gfwlist），内容跟随订阅刷新拉取，
// 只缓存到 state-dir，不持久化进配置文件。
type RuleURL struct {
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url" json:"url"`
}

// Config is the root of proxyd's configuration.
type Config struct {
	Subscriptions []Subscription `yaml:"subscriptions"`

	// ManualNodes 手动添加的自有代理节点（http(s)/socks5 URL 或分享链接），
	// 来源标记为 manual，与订阅节点一起参与去重/测速/端口分配。
	ManualNodes []string `yaml:"manual-nodes,omitempty"`

	Listen    string `yaml:"listen"`     // listen address for mapped ports, default 127.0.0.1
	PortRange [2]int `yaml:"port-range"` // inclusive [start, end] for per-node ports

	RefreshInterval Duration `yaml:"refresh-interval"` // subscription refresh period, default 30m
	HealthInterval  Duration `yaml:"health-interval"`  // health check period, default 5m
	HealthURL       string   `yaml:"health-url"`       // default http://www.gstatic.com/generate_204
	HealthTimeout   Duration `yaml:"health-timeout"`   // default 5s

	Exclude string `yaml:"exclude,omitempty"` // regexp filter on node names (airport info nodes)

	// Main mixed port (rule-mode entry). Default: PortRange[0]-1.
	MixedPort int `yaml:"mixed-port"`

	// MainAuto 为 true 时主端口跳过规则匹配，固定走 AUTO url-test 组
	// （全部可用节点中延迟最低者）；与独立的 auto-port 并存、互不影响。
	// 无可用节点时本轮跳过该设置（主端口回退规则模式），日志有提示。
	MainAuto bool `yaml:"main-auto,omitempty"`

	// MainNode 主端口固定节点：不开优选时让主端口跳过规则、直达指定节点。
	// 存节点 Key（协议+地址+凭据，重命名/重名时稳定），空串 = 不固定（现有行为）。
	// MainAuto 开启时本字段被忽略（auto 优先）；节点当前不可用（失效/订阅刷新后
	// 消失）时本轮回退规则模式并打日志，配置保留不删，节点恢复后自动再生效。
	MainNode string `yaml:"main-node,omitempty"`

	// AutoPort 自动选优端口（type mixed，固定走全部可用节点中延迟最低者）。0=关闭。
	AutoPort int `yaml:"auto-port,omitempty"`

	// SystemProxy 为 true 时 serve 启动后把系统代理指向主端口。
	SystemProxy bool `yaml:"system-proxy,omitempty"`

	// Clash-semantics globals, passed through to mihomo.
	Mode               string         `yaml:"mode"`                     // rule | global | direct
	Rules              []string       `yaml:"rules"`                    // clash rule lines
	CustomRules        []string       `yaml:"custom-rules,omitempty"`   // 追加式自定义规则，生成时前置到 rules 之前
	RuleURLs           []RuleURL      `yaml:"rule-urls,omitempty"`      // 远程规则源（gfwlist/mihomo 规则文本），导入规则排在 custom-rules 之后
	Groups             []NodeGroup    `yaml:"groups,omitempty"`         // 节点分组端口：一组节点 → 指定端口（自动选优）
	RuleProviders      map[string]any `yaml:"rule-providers,omitempty"` // clash rule-providers
	DNS                map[string]any `yaml:"dns,omitempty"`            // mihomo dns section (optional)
	GeoXUrl            map[string]any `yaml:"geox-url,omitempty"`       // 可选：覆盖 geo 数据下载地址（键：mmdb/geoip/geosite/asn）
	ExternalController string         `yaml:"external-controller"`      // default 127.0.0.1:19090
	Secret             string         `yaml:"secret,omitempty"`
	ExternalUI         string         `yaml:"external-ui,omitempty"`
	LogLevel           string         `yaml:"log-level"` // silent|error|warning|info|debug, default info

	// APIListen is proxyd's own API (port mapping table). Default 127.0.0.1:19091.
	// Kept separate from external-controller because mihomo's router cannot be extended.
	APIListen string `yaml:"api-listen"`

	StateDir string `yaml:"state-dir"` // mapping snapshot + cache; default ~/.local/state/proxyd
}

// Defaults applied by Load.
const (
	defaultListen             = "127.0.0.1"
	defaultRefreshInterval    = 24 * time.Hour
	defaultHealthInterval     = 5 * time.Minute
	defaultHealthURL          = "http://www.gstatic.com/generate_204"
	defaultHealthTimeout      = 5 * time.Second
	defaultExternalController = "127.0.0.1:19090"
	defaultAPIListen          = "127.0.0.1:19091"
	defaultLogLevel           = "info"
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

// DefaultGeoXUrl 是默认的 geo 数据下载地址：Loyalsoldier 规则仓库（主流数据源），
// 走 jsDelivr 的 gcore 节点，解决 GitHub 直连受限时 geo 文件下载失败的问题。
// 可在配置里用 geox-url 覆盖；用户显式配置时以用户为准。
var DefaultGeoXUrl = map[string]any{
	"geosite": "https://gcore.jsdelivr.net/gh/Loyalsoldier/v2ray-rules-dat@release/geosite.dat",
	"geoip":   "https://gcore.jsdelivr.net/gh/Loyalsoldier/v2ray-rules-dat@release/geoip.dat",
	"mmdb":    "https://gcore.jsdelivr.net/gh/Loyalsoldier/geoip@release/Country.mmdb",
}

// ParsePortRange 解析 "42000-42100" 形式的端口区间（闭区间）。
func ParsePortRange(s string) ([2]int, error) {
	var r [2]int
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return r, fmt.Errorf("invalid port range %q, expect like 42000-42100", s)
	}
	l, err1 := strconv.Atoi(strings.TrimSpace(lo))
	h, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if err1 != nil || err2 != nil || l <= 0 || h > 65535 || l > h {
		return r, fmt.Errorf("invalid port range %q, expect like 42000-42100", s)
	}
	return [2]int{l, h}, nil
}

// Quick 从命令行直接给出的订阅地址构建配置（免配置文件快捷启动）。
// portRange 形如 "42000-42100"，为空时用 DefaultPortRange。
func Quick(urls []string, portRange string) (*Config, error) {
	if portRange == "" {
		portRange = DefaultPortRange
	}
	pr, err := ParsePortRange(portRange)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		PortRange: pr,
		Rules:     DefaultRules,
	}
	for i, u := range urls {
		cfg.Subscriptions = append(cfg.Subscriptions, Subscription{
			Name: fmt.Sprintf("sub-%d", i+1),
			URL:  u,
			Type: "auto",
		})
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Load reads, defaults and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.migrate()
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// migrate 兼容旧版本配置：mode: auto（已废弃的模式）迁移为 rule + 开启 auto-port。
func (c *Config) migrate() {
	if c.Mode == "auto" {
		log.Printf("[config] mode: auto 已改为独立端口：迁移为 mode: rule + auto-port: %d", DefaultAutoPort)
		c.Mode = "rule"
		if c.AutoPort == 0 {
			c.AutoPort = DefaultAutoPort
		}
	}
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.RefreshInterval == 0 {
		c.RefreshInterval = Duration(defaultRefreshInterval)
	}
	if c.HealthInterval == 0 {
		c.HealthInterval = Duration(defaultHealthInterval)
	}
	if c.HealthURL == "" {
		c.HealthURL = defaultHealthURL
	}
	if c.HealthTimeout == 0 {
		c.HealthTimeout = Duration(defaultHealthTimeout)
	}
	if c.ExternalController == "" {
		c.ExternalController = defaultExternalController
	}
	if c.APIListen == "" {
		c.APIListen = defaultAPIListen
	}
	if c.LogLevel == "" {
		c.LogLevel = defaultLogLevel
	}
	if c.Mode == "" {
		c.Mode = "rule"
	}
	if c.MixedPort == 0 && c.PortRange[0] > 1 {
		c.MixedPort = c.PortRange[0] - 1
	}
	if c.StateDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			c.StateDir = filepath.Join(home, ".local", "state", "proxyd")
		}
	}
	// geo 下载地址：默认镜像 + 用户按键覆盖
	merged := make(map[string]any, len(DefaultGeoXUrl))
	for k, v := range DefaultGeoXUrl {
		merged[k] = v
	}
	for k, v := range c.GeoXUrl {
		merged[k] = v
	}
	c.GeoXUrl = merged
}

// Validate checks the config for structural errors.
func (c *Config) Validate() error {
	if len(c.Subscriptions) == 0 && len(c.ManualNodes) == 0 {
		return fmt.Errorf("at least one subscription or manual node is required")
	}
	for i, m := range c.ManualNodes {
		if strings.TrimSpace(m) == "" {
			return fmt.Errorf("manual-nodes[%d]: 空条目", i)
		}
	}
	seen := map[string]bool{}
	for i, s := range c.Subscriptions {
		if s.Name == "" {
			return fmt.Errorf("subscriptions[%d]: name is required", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("subscriptions[%d]: duplicate name %q", i, s.Name)
		}
		seen[s.Name] = true
		if s.URL == "" {
			return fmt.Errorf("subscription %q: url is required", s.Name)
		}
		switch s.Type {
		case "", "auto", "clash", "share":
		default:
			return fmt.Errorf("subscription %q: invalid type %q (auto|clash|share)", s.Name, s.Type)
		}
	}
	lo, hi := c.PortRange[0], c.PortRange[1]
	if lo <= 0 || hi > 65535 || lo > hi {
		return fmt.Errorf("invalid port-range [%d, %d]", lo, hi)
	}
	if c.MixedPort < 0 || c.MixedPort > 65535 || (c.MixedPort >= lo && c.MixedPort <= hi) {
		return fmt.Errorf("mixed-port %d must be outside port-range", c.MixedPort)
	}
	if p := addrPort(c.APIListen); p == c.MixedPort {
		return fmt.Errorf("mixed-port %d 与 api-listen 冲突", c.MixedPort)
	}
	if net.ParseIP(c.Listen) == nil {
		if _, err := net.ResolveIPAddr("ip", c.Listen); err != nil {
			return fmt.Errorf("invalid listen address %q", c.Listen)
		}
	}
	switch c.Mode {
	case "rule", "global", "direct":
	default:
		return fmt.Errorf("invalid mode %q (rule|global|direct)", c.Mode)
	}
	if c.Exclude != "" {
		if _, err := regexp.Compile(c.Exclude); err != nil {
			return fmt.Errorf("invalid exclude regexp: %w", err)
		}
	}
	if len(c.Rules) == 0 {
		return fmt.Errorf("rules must not be empty (at minimum: MATCH,PROXY)")
	}
	for i := range c.CustomRules {
		if err := ValidateCustomRule(c.CustomRules[i]); err != nil {
			return fmt.Errorf("custom-rules[%d]: %w", i, err)
		}
	}
	if err := c.CheckAutoPort(c.AutoPort); err != nil {
		return err
	}
	seenRU := map[string]bool{}
	for i, ru := range c.RuleURLs {
		if ru.Name == "" || ru.URL == "" {
			return fmt.Errorf("rule-urls[%d]: name 和 url 均为必填", i)
		}
		if seenRU[ru.Name] {
			return fmt.Errorf("rule-urls[%d]: duplicate name %q", i, ru.Name)
		}
		seenRU[ru.Name] = true
	}
	seenGrp := map[string]bool{}
	seenGrpPort := map[int]bool{}
	for i, g := range c.Groups {
		if err := c.checkGroup(g); err != nil {
			return fmt.Errorf("groups[%d]: %w", i, err)
		}
		if seenGrp[g.Name] {
			return fmt.Errorf("groups[%d]: duplicate name %q", i, g.Name)
		}
		if seenGrpPort[g.Port] {
			return fmt.Errorf("groups[%d]: duplicate port %d", i, g.Port)
		}
		seenGrp[g.Name] = true
		seenGrpPort[g.Port] = true
	}
	return nil
}

// Capacity returns how many per-node ports fit in the range.
func (c *Config) Capacity() int { return c.PortRange[1] - c.PortRange[0] + 1 }

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
	return nil
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

// addrPort 取 "127.0.0.1:19091" 形式地址的端口；解析失败返回 0。
func addrPort(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(port)
	return p
}
