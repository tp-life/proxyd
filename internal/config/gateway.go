package config

// 「LAN 网关」旁路由模块配置段：GatewayConfig/GatewayDevice 及其校验。
//
// 数据面见 docs/adr/0003（2026-09-11 勘误）：不依赖 TUN，macOS 走 pf rdr →
// redir-port，Linux 走 nftables redirect/tproxy，下游 DNS 53 redirect 到 1053。
// 设备登记表属配置意图，放本段走配置事务与历史；运行态（helper 状态等）放 state-dir。

import (
	"fmt"
	"net/netip"
	"strings"
)

const (
	// DefaultGatewayRedirPort 是未指定 redir-port 时的默认透明代理入口，
	// 避开现有默认端口（19090/19091/41998/41999/42000-42100）与 remote 自动转发段（10022 起）。
	DefaultGatewayRedirPort = 17892
	// DefaultGatewayTProxyPort 是未指定 tproxy-port 时的默认 UDP tproxy 入口（仅 Linux）。
	DefaultGatewayTProxyPort = 17893
	// DefaultGatewayDNSListenPort 是网关 dns-redirect 时 mihomo dns 的非特权监听端口；
	// 下游 53 端口由 pf/nftables redirect 到这里（helper 白名单不含绑端口操作，53 属特权端口）。
	DefaultGatewayDNSListenPort = 1053
)

// GatewayDevice 是一台登记在册的下游设备；MAC 仅用于识别与展示，不参与规则匹配。
type GatewayDevice struct {
	Name   string `yaml:"name" json:"name"`
	MAC    string `yaml:"mac,omitempty" json:"mac,omitempty"`
	IP     string `yaml:"ip" json:"ip"`         // IPv4 地址或 /32 CIDR
	Policy string `yaml:"policy" json:"policy"` // direct|proxy|group:<分组名>；留空默认 proxy
}

// GatewayConfig 是「LAN 网关」旁路由模块的配置段；数据面寄生于 proxy 模块的
// mihomo（redir/tproxy listener 与设备 SRC-IP-CIDR 规则都在 mihomo 配置里），
// 启用本模块要求 proxy 模块已启用。设备表为空的零值配置不生效（不应用转发规则、
// 不生成入口），保证存量配置升级后行为不变。
type GatewayConfig struct {
	Disabled    bool            `yaml:"disabled,omitempty" json:"disabled"`
	RedirPort   int             `yaml:"redir-port,omitempty" json:"redir_port"`     // mihomo redir 入口；0=默认 17892
	TProxyPort  int             `yaml:"tproxy-port,omitempty" json:"tproxy_port"`   // mihomo tproxy 入口（仅 Linux）；0=默认 17893
	DNSRedirect bool            `yaml:"dns-redirect,omitempty" json:"dns_redirect"` // 把下游 53 端口 redirect 到 mihomo dns（1053）
	Devices     []GatewayDevice `yaml:"devices,omitempty" json:"devices"`
}

// Clone 返回可独立修改的 GatewayConfig 副本，供热更新失败时回滚或跨锁读取。
//
// 参数说明：无。
//
// 返回值说明：GatewayConfig，Devices 切片不与原值共享（元素为纯标量值结构）。
//
// 错误情况：无。
func (g GatewayConfig) Clone() GatewayConfig {
	out := g
	out.Devices = append([]GatewayDevice(nil), g.Devices...)
	return out
}

// ApplyDefaults 为缺失的网关端口补齐默认值。
//
// 参数：无；接收者为待补默认值的 GatewayConfig 指针。
//
// 返回值：无；直接修改接收者。
//
// 错误情况：无；端口冲突等结构问题由 checkGateway 统一返回。
func (g *GatewayConfig) ApplyDefaults() {
	if g.RedirPort == 0 {
		g.RedirPort = DefaultGatewayRedirPort
	}
	if g.TProxyPort == 0 {
		g.TProxyPort = DefaultGatewayTProxyPort
	}
}

// EffectiveRedirPort 返回补默认值后的 redir 端口。
//
// 参数：无；接收者为网关配置值。
//
// 返回值：int，RedirPort 为 0（未经过 applyDefaults 的直接构造配置）时返回默认端口。
//
// 错误情况：无。
func (g GatewayConfig) EffectiveRedirPort() int {
	if g.RedirPort == 0 {
		return DefaultGatewayRedirPort
	}
	return g.RedirPort
}

// EffectiveTProxyPort 返回补默认值后的 tproxy 端口。
//
// 参数：无；接收者为网关配置值。
//
// 返回值：int，TProxyPort 为 0 时返回默认端口。
//
// 错误情况：无。
func (g GatewayConfig) EffectiveTProxyPort() int {
	if g.TProxyPort == 0 {
		return DefaultGatewayTProxyPort
	}
	return g.TProxyPort
}

// DeviceRules 把设备登记表转换为 mihomo SRC-IP-CIDR 规则行，
// 供 gen 规则管线（前置于 custom-rules）与测试共用。
//
// 参数：无；接收者为网关配置值。
//
// 返回值：
//   - []string：每条设备一行 `SRC-IP-CIDR,<ip>/32,<目标>`；policy direct→DIRECT、
//     proxy 或留空→PROXY、group:<名>→组名。无设备时返回 nil。
//
// 错误情况：无；配置入口（checkGateway）已保证 IP 与 policy 合法，
// 本函数对非法值按原样透传，不在运行路径上反复报错。
func (g GatewayConfig) DeviceRules() []string {
	if len(g.Devices) == 0 {
		return nil
	}
	rules := make([]string, 0, len(g.Devices))
	for _, device := range g.Devices {
		target := "PROXY"
		switch {
		case device.Policy == "direct":
			target = "DIRECT"
		case strings.HasPrefix(device.Policy, "group:"):
			target = strings.TrimPrefix(device.Policy, "group:")
		}
		ip := device.IP
		if !strings.Contains(ip, "/") {
			ip += "/32"
		}
		rules = append(rules, "SRC-IP-CIDR,"+ip+","+target)
	}
	return rules
}

// normalizeGatewayDeviceIP 校验并规范化设备 IP：接受 IPv4 地址或 IPv4 /32 前缀。
//
// 参数：
//   - ip: string，配置中的设备地址。
//
// 返回值：
//   - string：规范化后的地址（裸 IPv4 原样返回，前缀按 netip 规范输出），用于去重比较。
//   - error：地址非法、非 IPv4 或前缀不是 /32 时返回原因。
//
// 错误情况：IPv6、非 /32 网段（如 /24，语义不属于“单台设备”）一律拒绝。
func normalizeGatewayDeviceIP(ip string) (string, error) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return "", fmt.Errorf("ip 不能为空")
	}
	if strings.Contains(ip, "/") {
		prefix, err := netip.ParsePrefix(ip)
		if err != nil {
			return "", fmt.Errorf("ip %q 不是合法的 CIDR", ip)
		}
		if !prefix.Addr().Is4() || prefix.Bits() != 32 {
			return "", fmt.Errorf("ip %q 仅支持 IPv4 或 IPv4 /32 前缀", ip)
		}
		// /32 前缀与裸地址视为同一台设备，统一归一为地址形式便于去重。
		return prefix.Addr().String(), nil
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return "", fmt.Errorf("ip %q 不是合法地址", ip)
	}
	if !addr.Is4() {
		return "", fmt.Errorf("ip %q 仅支持 IPv4 地址", ip)
	}
	return addr.String(), nil
}

// CheckGateway 是 checkGateway 的导出入口，供应用层在设备增删改事务中
// 对「变更后的整体配置」复用同一份校验（端口查重需要分组、主端口等全量上下文）。
//
// 参数说明：无，接收者为待校验的完整配置（通常是事务克隆）。
//
// 返回值说明：error，约束满足时为 nil。
//
// 错误情况：与 checkGateway 完全一致。
func (c *Config) CheckGateway() error {
	return c.checkGateway()
}

// checkGateway 校验 gateway 配置段整体：设备表唯一性与字段合法、policy 枚举、
// redir/tproxy 端口与既有全部入口端口（mixed-port、节点映射区间、auto-port、
// api-listen、external-controller、全部分组端口）查重。
//
// 参数说明：无，接收者包含待校验的完整配置。
//
// 返回值说明：error，全部结构约束满足时为 nil。
//
// 错误情况：设备 name/ip 重复、IP 非法、policy 引用不存在的分组、端口越界或与
// 既有入口冲突时返回带上下文的错误；RedirPort/TProxyPort 为 0 时按默认值查重。
func (c *Config) checkGateway() error {
	g := c.Gateway
	seenName := map[string]bool{}
	seenIP := map[string]bool{}
	for i, device := range g.Devices {
		name := strings.TrimSpace(device.Name)
		if name == "" {
			return fmt.Errorf("gateway.devices[%d]: 名称不能为空", i)
		}
		if seenName[name] {
			return fmt.Errorf("gateway.devices[%d]: 名称 %q 重复", i, name)
		}
		seenName[name] = true
		normalizedIP, err := normalizeGatewayDeviceIP(device.IP)
		if err != nil {
			return fmt.Errorf("gateway.devices[%d]: %w", i, err)
		}
		if seenIP[normalizedIP] {
			return fmt.Errorf("gateway.devices[%d]: ip %q 重复", i, device.IP)
		}
		seenIP[normalizedIP] = true
		if err := c.checkGatewayDevicePolicy(device.Policy); err != nil {
			return fmt.Errorf("gateway.devices[%d]: %w", i, err)
		}
	}
	redirPort := g.EffectiveRedirPort()
	tproxyPort := g.EffectiveTProxyPort()
	if err := c.checkGatewayPort(redirPort, "redir-port"); err != nil {
		return err
	}
	if err := c.checkGatewayPort(tproxyPort, "tproxy-port"); err != nil {
		return err
	}
	if redirPort == tproxyPort {
		return fmt.Errorf("gateway: redir-port 与 tproxy-port 不能同为 %d", redirPort)
	}
	return nil
}

// checkGatewayDevicePolicy 校验单台设备的出口策略：direct、proxy、留空（默认 proxy）
// 或 group:<已存在的分组名>。
//
// 参数：
//   - policy: string，设备策略原文。
//
// 返回值：error，策略非法或引用不存在的分组时返回原因。
//
// 错误情况：group: 后分组名为空或不存在的均拒绝；分组合法性本身由分组校验负责。
func (c *Config) checkGatewayDevicePolicy(policy string) error {
	switch policy {
	case "", "direct", "proxy":
		return nil
	}
	name, ok := strings.CutPrefix(policy, "group:")
	if !ok {
		return fmt.Errorf("policy %q 无效（direct|proxy|group:<分组名>，留空默认 proxy）", policy)
	}
	if name == "" {
		return fmt.Errorf("policy %q 缺少分组名", policy)
	}
	for _, group := range c.Groups {
		if group.Name == name {
			return nil
		}
	}
	return fmt.Errorf("policy %q 引用的分组 %q 不存在", policy, name)
}

// checkGatewayPort 校验网关入口端口不与既有本地入口冲突。
//
// 参数：
//   - port: int，补默认值后的待校验端口。
//   - field: string，字段名（redir-port / tproxy-port），用于错误文案。
//
// 返回值：error，端口越界或与任一既有入口冲突时返回原因。
//
// 错误情况：与 mixed-port、节点映射区间、auto-port、api-listen、
// external-controller 或任一分组端口冲突时返回错误。
func (c *Config) checkGatewayPort(port int, field string) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("gateway: %s %d 超出 1-65535", field, port)
	}
	if port >= c.PortRange[0] && port <= c.PortRange[1] {
		return fmt.Errorf("gateway: %s %d 与节点映射区间 [%d, %d] 冲突", field, port, c.PortRange[0], c.PortRange[1])
	}
	if port == c.MixedPort {
		return fmt.Errorf("gateway: %s %d 与主端口冲突", field, port)
	}
	if c.AutoPort != 0 && port == c.AutoPort {
		return fmt.Errorf("gateway: %s %d 与 auto-port 冲突", field, port)
	}
	if p := addrPort(c.APIListen); p == port {
		return fmt.Errorf("gateway: %s %d 与 api-listen 冲突", field, port)
	}
	if p := addrPort(c.ExternalController); p == port {
		return fmt.Errorf("gateway: %s %d 与 external-controller 冲突", field, port)
	}
	for _, group := range c.Groups {
		if group.Port == port {
			return fmt.Errorf("gateway: %s %d 与分组 %q 端口冲突", field, port, group.Name)
		}
	}
	return nil
}
