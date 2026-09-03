// Package config loads and validates proxyd's own configuration file.
package config

import (
	"fmt"
	"log"
	"net"
	"net/url"
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

// DefaultStateDir 是配置未指定 state-dir 时使用的默认状态目录
//（~/.local/state/proxyd），供无法加载配置的纯客户端命令（remote pipe）兜底。
func DefaultStateDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "proxyd")
	}
	return ".proxyd-state"
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

// ExportYAML 把当前配置序列化为可下载的 YAML，并可对凭据与 token 打码。
//
// 参数：
//   - maskTokens: bool，true 时隐藏 secret、URL 用户信息和敏感查询参数。
//
// 返回值：
//   - []byte：完整 YAML 内容。
//   - error：YAML 序列化失败时返回错误。
//
// 错误情况：打码副本不会修改运行配置；打码文件用于排障分享，不保证可直接恢复凭据。
func (c *Config) ExportYAML(maskTokens bool) ([]byte, error) {
	target := c
	if maskTokens {
		target = c.RedactedCopy()
	}
	return yaml.Marshal(target)
}

// RedactedCopy 创建隐藏已知凭据字段的配置副本。
//
// 参数：无；接收者为源 Config。
//
// 返回值：
//   - *Config：可安全展示的副本；订阅和手动节点切片已独立复制。
//
// 错误情况：无；无法按 URL 解析的节点字符串会整体替换为 `<masked>`，
// 避免为了保持格式而意外泄露编码凭据。
func (c *Config) RedactedCopy() *Config {
	out := *c
	if out.Secret != "" {
		out.Secret = redactValue
	}
	out.Subscriptions = append([]Subscription(nil), c.Subscriptions...)
	for i := range out.Subscriptions {
		out.Subscriptions[i].URL = redactSourceURL(out.Subscriptions[i].URL)
	}
	out.ManualNodes = append([]string(nil), c.ManualNodes...)
	for i := range out.ManualNodes {
		out.ManualNodes[i] = redactURL(out.ManualNodes[i])
	}
	out.RuleURLs = append([]RuleURL(nil), c.RuleURLs...)
	for i := range out.RuleURLs {
		out.RuleURLs[i].URL = redactSourceURL(out.RuleURLs[i].URL)
	}
	out.RuleProviders = redactConfigMap(c.RuleProviders)
	out.GeoXUrl = redactConfigMap(c.GeoXUrl)
	out.DNS = redactConfigMap(c.DNS)
	out.Remote = c.Remote.Clone()
	for i := range out.Remote.Remotes {
		if out.Remote.Remotes[i].Token != "" {
			out.Remote.Remotes[i].Token = redactValue
		}
	}
	for i := range out.Remote.Forwards {
		// Remote 字段通常是 remotes 名称；用户也可能直接填 tc... token，按凭据打码。
		if strings.HasPrefix(out.Remote.Forwards[i].Remote, "tc") {
			out.Remote.Forwards[i].Remote = redactValue
		}
	}
	return &out
}

const redactValue = "***"

// redactURL 隐藏代理/订阅 URL 中的用户信息和常见敏感查询参数。
//
// 参数：
//   - rawURL: string，原始订阅地址、代理 URL 或分享链接。
//
// 返回值：
//   - string：HTTP(S)/SOCKS URL 保留地址结构但凭据打码；编码型分享链接整体打码。
//
// 错误情况：解析失败或缺少 scheme 时返回 `<masked>`，宁可降低可恢复性也不泄露 token。
func redactURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" {
		return "<masked>"
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks", "socks5":
	default:
		return parsed.Scheme + "://" + redactValue
	}
	if parsed.User != nil {
		parsed.User = url.User(redactValue)
	}
	query := parsed.Query()
	for key := range query {
		if sensitiveConfigKey(key) {
			query.Set(key, redactValue)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// redactSourceURL 隐藏订阅和规则源 URL 中可能位于路径或任意查询参数的访问令牌。
//
// 参数：
//   - rawURL: string，订阅或远程规则源地址。
//
// 返回值：
//   - string：HTTP(S) 地址保留 scheme 与 host，非根路径替换为 `/***`，全部查询值
//     和用户信息打码；其他 scheme 沿用 redactURL 的保守整体打码策略。
//
// 错误情况：解析失败时返回 `<masked>`。默认导出面向排障分享而非恢复，因此宁可隐藏
// 普通路径和非敏感查询值，也不能泄露供应商把 token 放在路径或自定义 key 中的地址。
func redactSourceURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" {
		return "<masked>"
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return redactURL(rawURL)
	}
	if parsed.User != nil {
		parsed.User = url.User(redactValue)
	}
	if parsed.EscapedPath() != "" && parsed.EscapedPath() != "/" {
		parsed.Path = "/" + redactValue
		parsed.RawPath = ""
	}
	query := parsed.Query()
	for key := range query {
		query.Set(key, redactValue)
	}
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	return parsed.String()
}

// sensitiveConfigKey 判断 URL 查询参数名是否可能承载凭据。
//
// 参数：
//   - key: string，查询参数名称。
//
// 返回值：
//   - bool：名称包含 token、secret、password、passwd、auth 或 api_key 时返回 true。
//
// 错误情况：无；判断大小写不敏感，采用保守包含匹配以覆盖供应商自定义前后缀。
func sensitiveConfigKey(key string) bool {
	key = strings.ToLower(key)
	for _, marker := range []string{"token", "secret", "password", "passwd", "auth", "api_key", "apikey"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

// redactConfigMap 深复制透传配置 map，并隐藏嵌套凭据键和 URL 查询参数。
//
// 参数：
//   - source: map[string]any，可能来自 rule-providers、geox-url 或 dns 的透传配置。
//
// 返回值：
//   - map[string]any：可独立序列化的打码副本；nil 输入仍返回 nil。
//
// 错误情况：无；未知类型按原值保留。只处理结构化键和可解析 URL，避免为了打码
// 破坏普通 mihomo 枚举、CIDR 或规则字符串。
func redactConfigMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = redactConfigValue(key, value)
	}
	return out
}

// redactConfigValue 递归处理透传配置中的 map、切片和字符串值。
//
// 参数：
//   - key: string，当前字段名，用于识别 token、secret、password 等敏感键。
//   - value: any，YAML 解码后的任意字段值。
//
// 返回值：
//   - any：敏感键替换为 `***`，HTTP/SOCKS URL 隐藏用户信息与敏感查询参数，
//     复合值返回深复制后的结构，其余标量保持原值。
//
// 错误情况：URL 解析失败时保留原字符串，因为没有证据表明普通透传值承载 URL；
// 顶层订阅、规则源和手动节点仍使用更保守的 redactURL 策略。
func redactConfigValue(key string, value any) any {
	if sensitiveConfigKey(key) {
		return redactValue
	}
	switch typed := value.(type) {
	case map[string]any:
		return redactConfigMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactConfigValue(key, item)
		}
		return out
	case []string:
		out := make([]string, len(typed))
		for i, item := range typed {
			redacted, _ := redactConfigValue(key, item).(string)
			out[i] = redacted
		}
		return out
	case string:
		parsed, err := url.Parse(typed)
		if err != nil || parsed.Scheme == "" {
			return typed
		}
		if strings.EqualFold(key, "url") {
			return redactSourceURL(typed)
		}
		if parsed.User != nil {
			return redactURL(typed)
		}
		for queryKey := range parsed.Query() {
			if sensitiveConfigKey(queryKey) {
				return redactURL(typed)
			}
		}
		return typed
	default:
		return value
	}
}

// Config is the root of proxyd's configuration.
type Config struct {
	Subscriptions []Subscription `yaml:"subscriptions"`

	// ManualNodes 手动添加的自有代理节点（http(s)/socks5 URL 或分享链接），
	// 来源标记为 manual，与订阅节点一起参与去重/测速/端口分配。
	ManualNodes []string `yaml:"manual-nodes,omitempty"`

	Listen    string `yaml:"listen"`     // listen address for mapped ports, default 127.0.0.1
	PortRange [2]int `yaml:"port-range"` // inclusive [start, end] for per-node ports
	// PortMapping 控制是否为每个健康节点创建一对一本地 listener。
	// 使用指针区分旧配置中的“字段缺失”和用户显式关闭，保证升级后默认继续开启。
	PortMapping *bool `yaml:"port-mapping,omitempty"`

	RefreshInterval Duration `yaml:"refresh-interval"` // subscription refresh period, default 30m
	HealthInterval  Duration `yaml:"health-interval"`  // health check period, default 5m
	HealthURL       string   `yaml:"health-url"`       // default https://www.gstatic.com/generate_204
	HealthTimeout   Duration `yaml:"health-timeout"`   // default 5s

	Include string `yaml:"include,omitempty"` // regexp allow-list on node names; empty means allow all
	Exclude string `yaml:"exclude,omitempty"` // regexp deny-list on node names; applied after include

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
	DNSPreset          string         `yaml:"dns-preset,omitempty"`     // off|fake-ip|redir-host；手写 DNS 非空时优先
	TUN                TUNConfig      `yaml:"tun" json:"tun"`           // mihomo TUN 段；常用字段有默认值，其余字段原样透传
	CheckUpdates       *bool          `yaml:"check-updates,omitempty"`  // 启动后异步检查 GitHub Releases；指针保留显式 false
	GeoXUrl            map[string]any `yaml:"geox-url,omitempty"`       // 可选：覆盖 geo 数据下载地址（键：mmdb/geoip/geosite/asn）
	ExternalController string         `yaml:"external-controller"`      // default 127.0.0.1:19090
	Secret             string         `yaml:"secret,omitempty"`
	ExternalUI         string         `yaml:"external-ui,omitempty"`
	LogLevel           string         `yaml:"log-level"` // silent|error|warning|info|debug, default info

	// APIListen is proxyd's own API (port mapping table). Default 127.0.0.1:19091.
	// Kept separate from external-controller because mihomo's router cannot be extended.
	APIListen string `yaml:"api-listen"`

	StateDir string `yaml:"state-dir"` // mapping snapshot + cache; default ~/.local/state/proxyd

	// Remote 「远程连接」周边模块（tailcat 隧道），与代理功能独立；默认关闭。
	Remote RemoteConfig `yaml:"remote,omitempty" json:"remote"`
}

// Defaults applied by Load.
const (
	defaultListen             = "127.0.0.1"
	defaultRefreshInterval    = 24 * time.Hour
	defaultHealthInterval     = 5 * time.Minute
	defaultHealthURL          = "https://www.gstatic.com/generate_204"
	defaultHealthTimeout      = 5 * time.Second
	defaultExternalController = "127.0.0.1:19090"
	defaultAPIListen          = "127.0.0.1:19091"
	defaultLogLevel           = "info"
)

// legacyHealthURL 是旧的默认 HTTP 探测地址。mihomo 提示部分机场会劫持 HTTP 探测
// 且重复 HEAD 请求导致 “failed to get the second response”，因此迁移到 HTTPS 默认值。
const legacyHealthURL = "http://www.gstatic.com/generate_204"

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

// Load 读取配置文件并调用 Parse 完成迁移、默认值和结构校验。
//
// 参数：
//   - path: string，YAML 配置文件路径。
//
// 返回值：
//   - *Config：可直接交给应用层的配置。
//   - error：文件读取、YAML 解析或结构校验失败时返回带阶段前缀的错误。
//
// 错误情况：读取失败以 `read config` 包装，其余错误由 Parse 返回。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse 从 YAML 字节解析配置，供文件加载和配置导入共用同一校验路径。
//
// 参数：
//   - raw: []byte，完整 YAML 文本。
//
// 返回值：
//   - *Config：已执行兼容迁移、默认值填充和 Validate 的配置。
//   - error：YAML 语法或任一配置约束不满足时返回错误。
//
// 错误情况：不会写文件或修改运行配置，调用方可在成功后再执行原子 Save。
func Parse(raw []byte) (*Config, error) {
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

// migrate 兼容旧版本配置：mode: auto（已废弃的模式）迁移为 rule + 开启 auto-port；
// 旧默认 HTTP 探测地址迁移为 HTTPS（用户显式自定义的其他地址不动）。
func (c *Config) migrate() {
	if c.Mode == "auto" {
		log.Printf("[config] mode: auto 已改为独立端口：迁移为 mode: rule + auto-port: %d", DefaultAutoPort)
		c.Mode = "rule"
		if c.AutoPort == 0 {
			c.AutoPort = DefaultAutoPort
		}
	}
	if c.HealthURL == legacyHealthURL {
		log.Printf("[config] health-url 旧默认值 %s 易被节点劫持导致探测失败，迁移为 %s", legacyHealthURL, defaultHealthURL)
		c.HealthURL = defaultHealthURL
	}
}

// applyDefaults 为缺失字段补齐运行默认值，并保留用户可显式关闭的布尔设置。
//
// 参数：无；接收者为 YAML 解析或快捷启动得到的 Config。
//
// 返回值：无；直接修改接收者，使后续校验、持久化和运行态看到相同配置。
//
// 错误情况：无；默认值之间的端口冲突等结构问题由 Validate 统一返回。
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
	if c.DNSPreset == "" {
		c.DNSPreset = DNSPresetOff
	}
	if c.CheckUpdates == nil {
		enabled := true
		c.CheckUpdates = &enabled
	}
	if c.PortMapping == nil {
		enabled := true
		c.PortMapping = &enabled
	}
	if c.MixedPort == 0 && c.PortRange[0] > 1 {
		c.MixedPort = c.PortRange[0] - 1
	}
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir()
	}
	for i := range c.Groups {
		if c.Groups[i].Type == "" {
			c.Groups[i].Type = GroupTypeURLTest
		}
	}
	c.TUN.ApplyDefaults()
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

// UpdateCheckEnabled 返回启动版本检查是否启用。
//
// 参数：无；接收者为当前配置。
//
// 返回值：
//   - bool：未显式配置时按向后兼容默认值 true 返回，否则返回用户设置。
//
// 错误情况：无；nil 接收者按关闭处理，避免异常初始化路径意外发起网络请求。
func (c *Config) UpdateCheckEnabled() bool {
	return c != nil && (c.CheckUpdates == nil || *c.CheckUpdates)
}

// PortMappingEnabled 返回健康节点一对一端口映射是否启用。
//
// 参数说明：无；接收者为当前运行配置。
//
// 返回值说明：
//   - bool：字段缺失时按向后兼容默认值 true 返回，否则返回用户显式设置。
//
// 错误情况：无；nil 接收者按关闭处理，避免异常初始化路径意外创建监听端口。
func (c *Config) PortMappingEnabled() bool {
	return c != nil && (c.PortMapping == nil || *c.PortMapping)
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
	// Validate 也供 e2e 和嵌入调用方直接构造 Config 使用；空值与 Parse 补出的
	// 默认 off 语义一致，但不在校验函数里修改调用方对象。
	dnsPreset := c.DNSPreset
	if dnsPreset == "" {
		dnsPreset = DNSPresetOff
	}
	switch dnsPreset {
	case DNSPresetOff, DNSPresetFakeIP, DNSPresetRedirHost:
	default:
		return fmt.Errorf("invalid dns-preset %q (off|fake-ip|redir-host)", dnsPreset)
	}
	if c.Exclude != "" {
		if _, err := regexp.Compile(c.Exclude); err != nil {
			return fmt.Errorf("invalid exclude regexp: %w", err)
		}
	}
	if c.Include != "" {
		if _, err := regexp.Compile(c.Include); err != nil {
			return fmt.Errorf("invalid include regexp: %w", err)
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
	// Validate 也支持调用方直接构造 Config（不经过 Load/Quick）的场景。
	// 在副本上补默认值可保持校验纯读，同时让空 TUNConfig 等价于默认关闭配置。
	tunConfig := c.TUN.Clone()
	tunConfig.ApplyDefaults()
	if err := tunConfig.Validate(); err != nil {
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
	if err := c.checkRemote(); err != nil {
		return err
	}
	return nil
}

// Capacity returns how many per-node ports fit in the range.
func (c *Config) Capacity() int { return c.PortRange[1] - c.PortRange[0] + 1 }

// addrPort 取 "127.0.0.1:19091" 形式地址的端口；解析失败返回 0。
func addrPort(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(port)
	return p
}
