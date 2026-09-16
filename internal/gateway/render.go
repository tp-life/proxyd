package gateway

// 转发规则文本的纯函数生成：pf anchor（macOS）与 nftables ruleset（Linux）。
// 生成结果只依赖入参，不做任何 I/O，便于单测与配置预览。

import (
	"fmt"
	"strings"

	"proxyd/internal/config"
)

const (
	// linuxTProxyMark 是 proxyd 为 Linux UDP 透明代理保留的报文标记。
	// 采用非通用的固定值而不是示例里常见的 0x1，降低与宿主机现有策略路由冲突的概率；
	// 执行层必须用相同值安装带掩码的 ip rule，二者共同构成一个不可拆分的数据面约定。
	linuxTProxyMark = 0x7078
	// linuxTProxyMarkMask 限定策略路由只匹配 proxyd 写入的低 16 位完整标记，
	// 避免只按“非零 mark”匹配而接管其他防火墙或 QoS 模块标记的流量。
	linuxTProxyMarkMask = 0xffff
)

// RenderPFAnchor 生成 macOS pf anchor 文本：设备表内下游地址的 TCP 流量 rdr 到
// 127.0.0.1:redir-port（mihomo redir 入口）；开启 dns-redirect 时把下游 UDP 53
// rdr 到 127.0.0.1:1053（mihomo dns 非特权监听）。设备表之外的 LAN 地址不匹配
// from 列表，天然不被转发。
//
// 参数：
//   - cfg: config.GatewayConfig，网关配置（端口未补默认值时按默认值渲染）。
//   - dnsListenPort: int，mihomo dns 实际监听端口。
//
// 返回值：
//   - string：完整 anchor 文本；设备表为空时只含注释（pf 载入空 anchor 等价于清除）。
//
// 错误情况：无；配置合法性由 config 校验保证。
func RenderPFAnchor(cfg config.GatewayConfig, dnsListenPort int) string {
	var b strings.Builder
	b.WriteString("# proxyd LAN 网关 anchor（自动生成，勿编辑）\n")
	b.WriteString("# 仅转发设备表内下游地址；表外 LAN 流量不受影响\n")
	ips := deviceIPList(cfg)
	if len(ips) == 0 {
		b.WriteString("# 设备表为空：无任何转发规则\n")
		return b.String()
	}
	from := "{ " + strings.Join(ips, ", ") + " }"
	fmt.Fprintf(&b, "rdr pass inet proto tcp from %s to any -> 127.0.0.1 port %d\n", from, cfg.EffectiveRedirPort())
	if cfg.DNSRedirect {
		fmt.Fprintf(&b, "rdr pass inet proto udp from %s to any port 53 -> 127.0.0.1 port %d\n", from, dnsListenPort)
	}
	return b.String()
}

// RenderNFTRuleset 生成 Linux nftables ruleset 文本（table inet proxyd-gw，
// 供 `nft -f -` 整体替换）：设备表内存为 ip 集合，下游 TCP redirect 到
// redir-port；开启 dns-redirect 时 UDP 53 redirect 到 dns 监听端口；
// tproxy-port 非零时附加 UDP tproxy 链；配套 ip rule 与本地路由由 Linux
// 执行层在同一次 Apply 中安装，调用方不需要额外执行系统命令。
//
// 参数：
//   - cfg: config.GatewayConfig，网关配置（端口未补默认值时按默认值渲染）。
//   - dnsListenPort: int，mihomo dns 实际监听端口。
//
// 返回值：
//   - string：完整 ruleset 文本；设备表为空时渲染空集合与不匹配任何流量的链，
//     应用后等价于无转发，便于 Clear 前保持幂等。
//
// 错误情况：无；配置合法性由 config 校验保证。
func RenderNFTRuleset(cfg config.GatewayConfig, dnsListenPort int) string {
	var b strings.Builder
	b.WriteString("table inet proxyd-gw {\n")
	b.WriteString("    set gw_devices {\n")
	b.WriteString("        type ipv4_addr\n")
	if ips := deviceIPList(cfg); len(ips) > 0 {
		fmt.Fprintf(&b, "        elements = { %s }\n", strings.Join(ips, ", "))
	}
	b.WriteString("    }\n")
	b.WriteString("    chain prerouting {\n")
	b.WriteString("        type nat hook prerouting priority dstnat; policy accept;\n")
	fmt.Fprintf(&b, "        ip saddr @gw_devices meta l4proto tcp redirect to :%d\n", cfg.EffectiveRedirPort())
	if cfg.DNSRedirect {
		fmt.Fprintf(&b, "        ip saddr @gw_devices udp dport 53 redirect to :%d\n", dnsListenPort)
	}
	b.WriteString("    }\n")
	if cfg.EffectiveTProxyPort() > 0 {
		// TPROXY 不修改原始目的地址，因此必须给报文打上专用 mark，再由执行层的
		// policy routing 把它交回 lo。显式 accept 终止本链，避免以后追加规则时
		// 已完成 TPROXY 的报文继续被同一条链重复处理。
		b.WriteString("    chain tproxy {\n")
		b.WriteString("        type filter hook prerouting priority mangle; policy accept;\n")
		if cfg.DNSRedirect {
			// DNS 劫持由后续 nat prerouting 链 redirect 到 mihomo DNS 监听器；若这里
			// 先把 UDP/53 送进通用 tproxy，DNS 专用入口与 fake-ip 语义将被绕过。
			fmt.Fprintf(&b, "        ip saddr @gw_devices meta l4proto udp udp dport != 53 tproxy to :%d meta mark set 0x%x accept\n", cfg.EffectiveTProxyPort(), linuxTProxyMark)
		} else {
			fmt.Fprintf(&b, "        ip saddr @gw_devices meta l4proto udp tproxy to :%d meta mark set 0x%x accept\n", cfg.EffectiveTProxyPort(), linuxTProxyMark)
		}
		b.WriteString("    }\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// deviceIPList 返回设备表的规范化 IPv4 地址列表（去 /32 后缀，保持登记顺序）。
func deviceIPList(cfg config.GatewayConfig) []string {
	ips := make([]string, 0, len(cfg.Devices))
	for _, device := range cfg.Devices {
		ip, _, _ := strings.Cut(device.IP, "/")
		ips = append(ips, ip)
	}
	return ips
}
