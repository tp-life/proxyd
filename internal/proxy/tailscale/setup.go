// Package tailscale 定义代理域内 Tailscale 接入向导的值对象与业务约束。
//
// 本包只描述“如何把一个 mihomo Tailscale 出站组织成可使用的接入点”，不依赖
// HTTP、配置文件、mihomo 或 Tailscale SDK。应用层负责把规范化结果转换为节点、
// 策略组和可选的 TUN 规则，并以事务方式提交。
package tailscale

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// AuthMode 表示 Tailscale 设备加入控制面的认证方式。
type AuthMode string

const (
	// AuthModeApproval 表示不携带预认证密钥，由控制面返回注册链接并等待管理员批准。
	AuthModeApproval AuthMode = "approval"
	// AuthModeKey 表示使用管理员预先生成的 auth-key 自动注册设备。
	AuthModeKey AuthMode = "auth-key"
)

// AccessMode 表示向本机应用暴露 Tailnet 的方式。
type AccessMode string

const (
	// AccessModeProxy 只创建固定 mixed 代理端口，不修改系统路由。
	AccessModeProxy AccessMode = "proxy"
	// AccessModeTUN 启用 TUN 并为目标网段生成路由规则。
	AccessModeTUN AccessMode = "tun"
	// AccessModeBoth 同时提供固定代理端口和 TUN 透明路由。
	AccessModeBoth AccessMode = "both"
)

// Setup 是一次 Tailscale 一体化接入请求。
//
// Name 是 mihomo 出站名称；GroupName 是该出站对应的单成员 select 分组；Port
// 始终由分组占用，即使选择 TUN-only 也保留一个诊断入口。Target 只在 TUN 模式
// 使用，既接受单个 IP，也接受 CIDR。
type Setup struct {
	Name         string     `json:"name"`
	ControlURL   string     `json:"control_url"`
	Hostname     string     `json:"hostname"`
	AuthMode     AuthMode   `json:"auth_mode"`
	AuthKey      string     `json:"auth_key,omitempty"`
	GroupName    string     `json:"group_name"`
	Port         int        `json:"port,omitempty"`
	AccessMode   AccessMode `json:"access_mode"`
	Target       string     `json:"target,omitempty"`
	AcceptRoutes bool       `json:"accept_routes"`
	UDP          bool       `json:"udp"`
	Ephemeral    bool       `json:"ephemeral"`
	ExitNode     string     `json:"exit_node,omitempty"`
	ExitNodeLAN  bool       `json:"exit_node_allow_lan_access"`
	DialerProxy  string     `json:"dialer_proxy,omitempty"`
	IPVersion    string     `json:"ip_version,omitempty"`
}

// Normalize 清理用户输入、补齐安全默认值并验证接入业务规则。
//
// 参数说明：无；接收者包含 HTTP 或 CLI 传入的原始字段。
//
// 返回值说明：
//   - Setup：字段已裁剪空白，名称、认证方式、访问方式和目标网段均已规范化。
//   - error：必填字段缺失、URL/IP/端口非法或认证方式互相矛盾时返回。
//
// 错误情况：approval 模式不允许同时提交 auth-key，避免用户误以为审批流程仍会
// 生效；TUN 模式必须提供明确目标，防止一键配置意外接管全部系统流量。
func (s Setup) Normalize() (Setup, error) {
	s.Name = strings.TrimSpace(s.Name)
	s.ControlURL = strings.TrimSpace(s.ControlURL)
	s.Hostname = strings.TrimSpace(s.Hostname)
	s.AuthKey = strings.TrimSpace(s.AuthKey)
	s.GroupName = strings.TrimSpace(s.GroupName)
	s.Target = strings.TrimSpace(s.Target)
	s.ExitNode = strings.TrimSpace(s.ExitNode)
	s.DialerProxy = strings.TrimSpace(s.DialerProxy)
	s.IPVersion = strings.TrimSpace(s.IPVersion)
	if s.Name == "" {
		return Setup{}, fmt.Errorf("Tailscale 节点名称不能为空")
	}
	if s.ControlURL != "" {
		parsed, err := url.Parse(s.ControlURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return Setup{}, fmt.Errorf("控制服务地址必须是有效的 http/https URL")
		}
	}
	if s.Hostname == "" {
		s.Hostname = s.Name
	}
	if s.GroupName == "" {
		s.GroupName = s.Name + "-access"
	}
	if s.GroupName == s.Name {
		return Setup{}, fmt.Errorf("策略组名称不能与 Tailscale 节点名称相同")
	}
	if s.Port < 0 || s.Port > 65535 {
		return Setup{}, fmt.Errorf("代理端口必须为 1-65535，留空时由 proxyd 自动分配")
	}
	if s.AuthMode == "" {
		s.AuthMode = AuthModeApproval
	}
	switch s.AuthMode {
	case AuthModeApproval:
		if s.AuthKey != "" {
			return Setup{}, fmt.Errorf("管理员审批模式不应填写 auth-key")
		}
	case AuthModeKey:
		if s.AuthKey == "" {
			return Setup{}, fmt.Errorf("Auth Key 模式必须填写 auth-key")
		}
	default:
		return Setup{}, fmt.Errorf("认证方式 %q 无效（approval|auth-key）", s.AuthMode)
	}
	if s.AccessMode == "" {
		s.AccessMode = AccessModeProxy
	}
	switch s.AccessMode {
	case AccessModeProxy:
		s.Target = ""
	case AccessModeTUN, AccessModeBoth:
		target, err := normalizeTarget(s.Target)
		if err != nil {
			return Setup{}, err
		}
		s.Target = target
	default:
		return Setup{}, fmt.Errorf("访问方式 %q 无效（proxy|tun|both）", s.AccessMode)
	}
	if s.IPVersion == "" {
		s.IPVersion = "ipv4-prefer"
	}
	switch s.IPVersion {
	case "dual", "ipv4", "ipv6", "ipv4-prefer", "ipv6-prefer":
	default:
		return Setup{}, fmt.Errorf("底层 IP 偏好 %q 无效", s.IPVersion)
	}
	return s, nil
}

// UsesTUN 判断本次接入是否需要透明路由。
//
// 参数说明：无。
//
// 返回值说明：AccessMode 为 tun 或 both 时返回 true。
//
// 错误情况：无；非法模式会在 Normalize 中提前拒绝。
func (s Setup) UsesTUN() bool {
	return s.AccessMode == AccessModeTUN || s.AccessMode == AccessModeBoth
}

// RouteRule 生成把目标地址送往接入策略组的 mihomo 规则。
//
// 参数说明：无；调用前应先通过 Normalize。
//
// 返回值说明：IPv4 返回 IP-CIDR，IPv6 返回 IP-CIDR6；非 TUN 模式返回空字符串。
//
// 错误情况：无；Normalize 已保证 TUN 模式的 Target 是合法前缀。
func (s Setup) RouteRule() string {
	if !s.UsesTUN() || s.Target == "" {
		return ""
	}
	prefix, _ := netip.ParsePrefix(s.Target)
	ruleType := "IP-CIDR"
	if prefix.Addr().Is6() {
		ruleType = "IP-CIDR6"
	}
	return fmt.Sprintf("%s,%s,%s,no-resolve", ruleType, s.Target, s.GroupName)
}

// IsManagedRouteForGroup 判断一条自定义规则是否是一体化 Tailscale 接入为指定
// 策略组生成的受管 IP 路由。
//
// 参数说明：
//   - rule: string，配置中的单条 mihomo 自定义规则。
//   - groupName: string，即将随 Tailscale 接入删除的策略组名称。
//
// 返回值说明：仅当规则严格符合 `IP-CIDR|IP-CIDR6,前缀,组名,no-resolve`
// 且地址前缀合法时返回 true。
//
// 错误情况：无；格式不完整、非 IP 路由、目标组不同或前缀非法均返回 false。严格
// 匹配是为了只清理由 Setup.RouteRule 创建的资源，避免误删用户手写的其它分流规则。
func IsManagedRouteForGroup(rule, groupName string) bool {
	parts := strings.Split(rule, ",")
	if len(parts) != 4 || strings.TrimSpace(parts[2]) != strings.TrimSpace(groupName) || strings.TrimSpace(parts[3]) != "no-resolve" {
		return false
	}
	ruleType := strings.TrimSpace(parts[0])
	if ruleType != "IP-CIDR" && ruleType != "IP-CIDR6" {
		return false
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(parts[1]))
	if err != nil {
		return false
	}
	return (ruleType == "IP-CIDR" && prefix.Addr().Is4()) || (ruleType == "IP-CIDR6" && prefix.Addr().Is6())
}

// normalizeTarget 把单个 Tailnet 地址或 CIDR 规范化为带掩码的前缀。
//
// 参数说明：raw 为用户填写的 IPv4/IPv6 地址或 CIDR。
//
// 返回值说明：string 为 netip 的标准 CIDR 文本；error 表示目标为空或格式非法。
//
// 错误情况：单个 IP 自动转换为 /32 或 /128；不接受域名，因为透明规则必须具有
// 稳定、可审计的地址边界，域名访问应继续使用代理端口或独立域名规则。
func normalizeTarget(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("TUN 访问方式必须填写目标 IP 或 CIDR")
	}
	if prefix, err := netip.ParsePrefix(raw); err == nil {
		return prefix.Masked().String(), nil
	}
	address, err := netip.ParseAddr(raw)
	if err != nil {
		return "", fmt.Errorf("TUN 目标 %q 不是有效的 IP 或 CIDR", raw)
	}
	bits := 32
	if address.Is6() {
		bits = 128
	}
	return netip.PrefixFrom(address, bits).String(), nil
}
