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
	HealthURL       string   `yaml:"health-url"`       // default http://www.gstatic.com/generate_204
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
		if home, err := os.UserHomeDir(); err == nil {
			c.StateDir = filepath.Join(home, ".local", "state", "proxyd")
		}
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

// addrPort 取 "127.0.0.1:19091" 形式地址的端口；解析失败返回 0。
func addrPort(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(port)
	return p
}
