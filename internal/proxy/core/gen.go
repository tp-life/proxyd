// Package core 负责生成 mihomo 配置并以库方式内嵌运行 mihomo 核心。
package core

import (
	"fmt"
	"log"
	"net/netip"
	"runtime"
	"slices"
	"strings"

	"github.com/metacubex/mihomo/hub/executor"
	"gopkg.in/yaml.v3"

	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/pool"
)

// Assignment 是端口到节点的映射（internal/pool 定义的别名）。
type Assignment = pool.Assignment

const (
	// adBlockProviderName 是广告拦截注入的 rule-provider 保留键名。
	adBlockProviderName = "adblock"
	// adBlockRule 是开启广告拦截时插入的规则：命中的域名直接 REJECT。
	adBlockRule = "RULE-SET,adblock,REJECT"
)

// Generate 生成完整的 mihomo YAML 配置：
// 主端口恒为顶层 mixed-port，走常规 Clash 规则模式（rule/global/direct）；
// 规则兜底的 MATCH,PROXY 落到内置 PROXY select 组（全部可用节点 + AUTO + DIRECT），
// 组的持久化选中项（默认出口）经 default-selected 注入，未选择时落成员首位；
// 每个节点分配一个 listener（type mixed，proxy 固定出口到该节点）；
// 有可用节点即生成 AUTO url-test 组（全部可用节点选优）；
// auto-port 开启时额外生成一个固定走 AUTO 组的 listener；
// 每个节点分组额外生成一个 proxy-group + 固定走该组的 mixed listener；
// 分组可以显式列节点，也可以按订阅名动态取该订阅当前可用节点。
// tun 段由 config.TUNConfig 提供常用默认值并保留高级字段，生成时完整交给 mihomo；
// TUN 设备权限由应用层在开启前校验，本函数只负责配置语义与 mihomo 解析自检。
// imported 是 rule-urls 远程导入的规则（排在 custom-rules 之后、内置 rules 之前）。
//
// 注意：listener 名称统一用端口号（"L<port>"），不带节点/分组名。
// mihomo 热更新（PatchInboundListeners）先 Listen 新 listener、后关闭旧 listener，
// 同端口改名会 bind 冲突把端口打挂；同名但配置不同才会按 关闭→监听 的正确顺序处理。
func Generate(cfg *config.Config, assigns []Assignment, imported []string) ([]byte, error) {
	return generate(cfg, assigns, nil, imported, nil)
}

// GenerateWithNodes 生成 mihomo 配置，并把未分配本地端口但仍被链路或分组引用的
// 可用节点注册为 proxy-only 出站。
//
// 参数：
//   - cfg: *config.Config，已经完成默认值与合法性校验的运行配置。
//   - assigns: []Assignment，获得独立本地 listener 的节点和端口映射。
//   - nodes: []*node.Node，本轮完整节点集合；仅 Alive 节点会作为额外出站加入。
//   - imported: []string，远程规则源合并后的规则文本。
//
// 返回值：
//   - []byte，可直接交给 mihomo hub.Parse 的 YAML 配置。
//   - error，序列化失败、dialer-proxy 缺失/循环或其它 mihomo 语义错误时返回。
//
// 错误情况：额外节点只注册出站，不占用本地端口，也不自动进入 PROXY/AUTO；这样
// 端口容量限制不会截断链式代理依赖，同时保持用户可见端口集合的既有语义。
func GenerateWithNodes(cfg *config.Config, assigns []Assignment, nodes []*node.Node, imported []string) ([]byte, error) {
	return generate(cfg, assigns, nodes, imported, nil)
}

// GenerateWithState 在 GenerateWithNodes 基础上接受 select 分组的持久化选中项。
//
// 参数：
//   - selected: map[string]string，分组名 -> 选中节点名（state-dir/group-selected.json）；
//     nil 等价无持久化选中。
//
// 其余参数、返回值与错误语义同 GenerateWithNodes。选中值仅在仍是该组当前成员时写入
// mihomo select 组的 default-selected 字段；失效值被静默忽略，mihomo 按其原生语义
// 回退到组成员首位。选择 default-selected 而非 external-controller 恢复，是因为它在
// 配置加载期生效，无需等 mihomo 启动后再发 API 请求，路径更短且无并发时序问题。
func GenerateWithState(cfg *config.Config, assigns []Assignment, nodes []*node.Node, imported []string, selected map[string]string) ([]byte, error) {
	return generate(cfg, assigns, nodes, imported, selected)
}

// generate 实现 Generate 与 GenerateWithNodes 共用的配置翻译和 mihomo 自检流程。
//
// 参数：
//   - cfg: *config.Config，proxyd 运行配置。
//   - assigns: []Assignment，需要生成固定端口入口的节点。
//   - nodes: []*node.Node，可选完整健康节点集，用于链式依赖和策略组成员。
//   - imported: []string，已经清洗、去重的远程规则。
//   - selected: map[string]string，select 分组的持久化选中项（分组名 -> 节点名）。
//
// 返回值：生成后的 YAML 字节与错误。
//
// 错误情况：assignment 缺节点、YAML 序列化或 mihomo 自检失败时返回错误；GEO 数据
// 不可用时沿用既有降级逻辑，移除 GEO 规则后再自检一次。
func generate(cfg *config.Config, assigns []Assignment, nodes []*node.Node, imported []string, selected map[string]string) ([]byte, error) {
	m := map[string]any{
		"mode":                cfg.Mode,
		"log-level":           cfg.LogLevel,
		"allow-lan":           !isLoopback(cfg.Listen),
		"bind-address":        cfg.Listen,
		"external-controller": cfg.ExternalController,
		"unified-delay":       true,
		"tcp-concurrent":      true,
	}
	if cfg.Secret != "" {
		m["secret"] = cfg.Secret
	}
	if cfg.ExternalUI != "" {
		m["external-ui"] = cfg.ExternalUI
	}
	dnsConfig := resolveDNSConfig(cfg)
	if gatewayActive(cfg) {
		// gateway 数据面入口（docs/adr/0003 勘误，不依赖 TUN）：
		// redir-port 两平台都需要；tproxy-port 是 Linux 语义，macOS 上 mihomo
		// 建 listener 只会报错刷屏，因此仅 Linux 且端口有效时写入。
		m["redir-port"] = cfg.Gateway.EffectiveRedirPort()
		if runtime.GOOS == "linux" {
			if port := cfg.Gateway.EffectiveTProxyPort(); port > 0 {
				m["tproxy-port"] = port
			}
		}
		if cfg.Gateway.DNSRedirect {
			dnsConfig = injectGatewayDNSListen(dnsConfig)
		}
	}
	if dnsConfig != nil {
		m["dns"] = dnsConfig
	}
	// Generate 也可能被测试或嵌入调用方直接传入未经过 config.Load 的 Config。
	// 因此在本地副本上再次补默认值，既保证 mihomo 收到合法 stack，也不修改调用方配置。
	tunConfig := cfg.TUN.Clone()
	tunConfig.ApplyDefaults()
	m["tun"] = tunConfig
	if cfg.GeoXUrl != nil {
		m["geox-url"] = cfg.GeoXUrl
	}

	proxyNodes := availableProxyNodes(assigns, nodes)
	proxies := make([]map[string]any, 0, len(proxyNodes))
	proxyNodeSet := make(map[string]bool, len(proxyNodes))
	for _, n := range proxyNodes {
		proxies = append(proxies, prepareOutboundMapping(cfg, n))
		proxyNodeSet[n.Name] = true
	}
	nodeNames := make([]string, 0, len(assigns))
	listeners := make([]map[string]any, 0, len(assigns)+len(cfg.Groups)+1)
	for _, a := range assigns {
		if a.Node == nil {
			return nil, fmt.Errorf("端口 %d 的 assignment 缺少节点", a.Port)
		}
		// 节点仍需进入 PROXY/AUTO 等路由组；端口映射开关只控制一对一 listener，
		// 不能通过跳过整个 assignment 来实现，否则会同时破坏主端口和策略组的出口集合。
		// 订阅级 port-mapping 关闭同样只停该订阅的 listener，节点保留在路由组中。
		nodeNames = append(nodeNames, a.Node.Name)
		if cfg.PortMappingEnabled() && cfg.PortMappingEnabledFor(a.Node.Subscription) {
			listeners = append(listeners, map[string]any{
				"name":   fmt.Sprintf("L%d", a.Port), // 纯端口名：热更新端口换人时避免 bind 冲突（见函数注释）
				"type":   "mixed",
				"listen": cfg.Listen,
				"port":   a.Port,
				"proxy":  a.Node.Name,
			})
		}
	}
	m["proxies"] = proxies

	// 主端口恒为顶层 mixed-port 规则模式；兜底 MATCH,PROXY 落到内置 PROXY select 组，
	// 组的选中项（默认出口）由用户在控制台/API 选择并经 default-selected 持久化。
	m["mixed-port"] = cfg.MixedPort

	// AUTO url-test 组（全部可用节点中延迟最低）：有可用节点即生成，
	// 供内置 PROXY 组（「自动最快」选项）与 auto-port listener 引用；成员限 nodeNames，
	// 不含隧道类节点（隧道节点延迟语义不同，只经 PROXY 组直接选择）。
	autoGroupOn := len(nodeNames) > 0
	if cfg.AutoPort > 0 && !autoGroupOn {
		log.Printf("[core] auto-port %d 已开启但当前无可用节点，本轮跳过该 listener", cfg.AutoPort)
	}

	// 内置 PROXY select 组：规则模式的默认出口。成员 = 全部可用节点
	// （availableProxyNodes 顺序：assigns 在前、额外可用节点含隧道在后）+ AUTO（存在时）
	// + DIRECT 殿后；无持久化选择时 mihomo 原生落成员首位（第一可用节点），行为与旧版一致。
	// 持久化选中项（state-dir/group-selected.json 的 "PROXY" 键）仅在仍是当前成员时
	// 写入 default-selected；失效值被忽略，mihomo 按其原生语义回退成员首位。
	proxyMembers := make([]string, 0, len(proxyNodes)+2)
	for _, n := range proxyNodes {
		proxyMembers = append(proxyMembers, n.Name)
	}
	if autoGroupOn {
		proxyMembers = append(proxyMembers, "AUTO")
	}
	proxyMembers = append(proxyMembers, "DIRECT")
	proxyGroup := map[string]any{"name": "PROXY", "type": "select", "proxies": proxyMembers}
	if sel := selected["PROXY"]; sel != "" && slices.Contains(proxyMembers, sel) {
		proxyGroup["default-selected"] = sel
	}
	groups := []map[string]any{proxyGroup}
	if autoGroupOn {
		groups = append(groups, map[string]any{
			"name":      "AUTO",
			"type":      "url-test",
			"proxies":   nodeNames,
			"url":       cfg.HealthURL,
			"interval":  300,
			"tolerance": 50,
		})
		if cfg.AutoPort > 0 {
			listeners = append(listeners, map[string]any{
				"name":   fmt.Sprintf("L%d", cfg.AutoPort),
				"type":   "mixed",
				"listen": cfg.Listen,
				"port":   cfg.AutoPort,
				"proxy":  "AUTO",
			})
		}
	}

	// 节点分组：组名与节点名/保留名冲突或成员交集为空时跳过（打日志）。
	for _, g := range cfg.Groups {
		if proxyNodeSet[g.Name] || strings.EqualFold(g.Name, "AUTO") || strings.EqualFold(g.Name, "PROXY") {
			log.Printf("[core] 分组 %q 与节点名/保留名冲突，跳过", g.Name)
			continue
		}
		members := resolveGroupMembersFromNodes(g, proxyNodes, proxyNodeSet)
		if len(members) == 0 {
			log.Printf("[core] 分组 %q 与当前可用节点无交集，跳过", g.Name)
			continue
		}
		groupType := g.Type
		if groupType == "" {
			groupType = config.GroupTypeURLTest
		}
		group := map[string]any{
			"name":      g.Name,
			"type":      groupType,
			"proxies":   members,
			"url":       cfg.HealthURL,
			"interval":  300,
			"tolerance": 50,
		}
		// select 组恢复持久化选中项：仅在选中值仍是当前成员时写入 mihomo 原生
		// default-selected 字段；失效值忽略，mihomo 按原生语义回退组成员首位。
		if groupType == config.GroupTypeSelect {
			if sel := selected[g.Name]; sel != "" && slices.Contains(members, sel) {
				group["default-selected"] = sel
			}
		}
		groups = append(groups, group)
		listeners = append(listeners, map[string]any{
			"name":   fmt.Sprintf("L%d", g.Port),
			"type":   "mixed",
			"listen": cfg.Listen,
			"port":   g.Port,
			"proxy":  g.Name,
		})
	}
	m["proxy-groups"] = groups

	// 规则合并顺序：gateway 设备规则（SRC-IP-CIDR）最前 → 用户 custom-rules →
	// rule-urls 导入规则 → 广告拦截 RULE-SET → 内置规则（追加在 GEOSITE/GEOIP/MATCH
	// 之后永远不会命中，所以设备/自定义/导入规则必须前置）。广告规则排在导入规则之后，
	// 用户可用 custom-rules / rule-urls 自行加白名单例外。
	rules := make([]string, 0, len(cfg.Gateway.Devices)+len(cfg.CustomRules)+len(imported)+len(cfg.Rules)+1)
	if gatewayActive(cfg) {
		rules = append(rules, gatewayDeviceRules(cfg, groups)...)
	}
	rules = append(rules, cfg.CustomRules...)
	rules = append(rules, imported...)
	if cfg.AdBlock.Enable {
		rules = append(rules, adBlockRule)
	}
	rules = append(rules, cfg.Rules...)
	m["rules"] = rules
	ruleProviders := cfg.RuleProviders
	if cfg.AdBlock.Enable {
		// adblock 是 proxyd 保留的 rule-provider 键；用户手写 rule-providers 占用
		// 该键时直接报错（经配置事务回滚），避免静默覆盖用户配置。
		if _, taken := ruleProviders[adBlockProviderName]; taken {
			return nil, fmt.Errorf("rule-providers 中 %q 是广告拦截功能的保留名，请改用其它键名", adBlockProviderName)
		}
		merged := make(map[string]any, len(ruleProviders)+1)
		for key, value := range ruleProviders {
			merged[key] = value
		}
		// 默认规则集（Loyalsoldier clash-rules reject.txt）是 domain 行为的
		// YAML payload 文本；下载/缓存/周期刷新全部由 mihomo 负责。
		merged[adBlockProviderName] = map[string]any{
			"type":     "http",
			"behavior": "domain",
			"format":   "yaml",
			"url":      cfg.AdBlock.RuleURL,
			"interval": 86400,
		}
		ruleProviders = merged
	}
	if ruleProviders != nil {
		m["rule-providers"] = ruleProviders
	}
	if len(listeners) > 0 {
		m["listeners"] = listeners
	}

	buf, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("序列化 mihomo 配置: %w", err)
	}

	// 自检：ParseWithBytes 只是 config.Parse（UnmarshalRawConfig + ParseRawConfig），
	// 不启动 listener、不创建目录；解析期间对 general 的临时全局改动会随
	// temporaryUpdateGeneral 的 rollback 复原（见 mihomo config/config.go ParseRawConfig）。
	// 唯一注意点：若 rules 含 GEOIP/GEOSITE，解析时会尝试加载 geo 数据文件，
	// 依赖调用方在此之前通过 C.SetHomeDir 设置好目录（NewRunner 已保证）。
	if _, err := executor.ParseWithBytes(buf); err != nil {
		// geo 数据需要从 GitHub 下载；网络受限时会卡住/失败。
		// 降级：剔除 GEO 规则后重试一次，保证代理本体可用（此时 GEO 规则语义退化为 MATCH 兜底）。
		if !hasGeoRules(rules) {
			return nil, fmt.Errorf("mihomo 配置自检失败: %w", err)
		}
		log.Printf("[core] geo 数据不可用（%v），本轮降级为不含 GEO 规则运行；可配置 geox-url 镜像后恢复", firstLine(err.Error()))
		m["rules"] = stripGeoRules(rules)
		buf, err = yaml.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("序列化 mihomo 配置: %w", err)
		}
		if _, err := executor.ParseWithBytes(buf); err != nil {
			return nil, fmt.Errorf("mihomo 配置自检失败（已剔除 GEO 规则）: %w", err)
		}
	}
	return buf, nil
}

// gatewayActive 判断 gateway 数据面是否应参与本次生成：模块未停用、代理数据面
// 未停用且设备表非空。ProxyDisabled 走 Runner.Suspend 路径本就不会调用 generate，
// 这里的判断是防御直接构造 Config 的嵌入调用方；设备表为空的零值配置视为不生效，
// 避免存量配置升级后凭空出现 redir 入口与转发规则（无任何设备可被分流）。
func gatewayActive(cfg *config.Config) bool {
	return !cfg.Gateway.Disabled && !cfg.ProxyDisabled && len(cfg.Gateway.Devices) > 0
}

// gatewayDeviceRules 返回可安全写入 mihomo 的设备分流规则：引用本轮被跳过分组
// （与节点名冲突或成员交集为空）的设备规则按 ADR 0003 跳过并打日志，不阻塞生成；
// DIRECT/PROXY 内置目标与实际生成的分组名均可引用。
//
// 参数：
//   - cfg: *config.Config，运行配置。
//   - groups: []map[string]any，本轮实际写入 mihomo 的策略组（含 PROXY/AUTO）。
//
// 返回值：[]string，过滤后的 SRC-IP-CIDR 规则行（可能为 nil）。
//
// 错误情况：无；失效引用仅降级为日志。
func gatewayDeviceRules(cfg *config.Config, groups []map[string]any) []string {
	deviceRules := cfg.Gateway.DeviceRules()
	if len(deviceRules) == 0 {
		return nil
	}
	valid := map[string]bool{"DIRECT": true, "PROXY": true}
	for _, group := range groups {
		if name, ok := group["name"].(string); ok {
			valid[name] = true
		}
	}
	out := make([]string, 0, len(deviceRules))
	for _, rule := range deviceRules {
		target := rule[strings.LastIndexByte(rule, ',')+1:]
		if !valid[target] {
			log.Printf("[core] gateway 设备规则 %q 引用的出口不可用，本轮跳过", rule)
			continue
		}
		out = append(out, rule)
	}
	return out
}

// injectGatewayDNSListen 在 dns 段补网关 dns-redirect 所需的非特权监听地址。
//
// 参数：
//   - dnsConfig: map[string]any，resolveDNSConfig 的结果（nil 表示无手写/预设 dns）。
//
// 返回值：
//   - map[string]any：带 listen: 0.0.0.0:1053 的 dns 段；手写段已有 listen 时原样返回并打日志。
//
// 错误情况：无。返回的是新 map 或新建段，不原地修改 cfg.DNS 共享引用；
// 无现存段时生成带默认 nameserver 的最小可用段（mihomo 要求 dns.enable 必须配 nameserver）。
func injectGatewayDNSListen(dnsConfig map[string]any) map[string]any {
	listenAddr := fmt.Sprintf("0.0.0.0:%d", config.DefaultGatewayDNSListenPort)
	if dnsConfig == nil {
		return map[string]any{
			"enable":             true,
			"listen":             listenAddr,
			"default-nameserver": []string{"223.5.5.5", "1.1.1.1"},
			"nameserver":         []string{"223.5.5.5", "1.1.1.1"},
		}
	}
	if existing, ok := dnsConfig["listen"].(string); ok && existing != "" {
		log.Printf("[core] gateway dns-redirect 已开启，但手写 dns 段已配置 listen %q，不覆盖；请确认其与 pf/nftables redirect 目标一致", existing)
		return dnsConfig
	}
	out := make(map[string]any, len(dnsConfig)+1)
	for key, value := range dnsConfig {
		out[key] = value
	}
	out["listen"] = listenAddr
	return out
}

// resolveDNSConfig 按“手写配置优先，其次预设”的规则生成 mihomo DNS 段。
//
// 参数：
//   - cfg: *config.Config，包含可选的原始 dns map 与 dns-preset。
//
// 返回值：
//   - map[string]any：应写入 mihomo 的 dns 段；off 且无手写配置时返回 nil。
//
// 错误情况：无；dns-preset 枚举已由 config.Validate 校验。调用方直接构造出未知值时
// 保守按 off 处理，最终配置仍可运行，不在生成层重复制造第二套校验错误。
func resolveDNSConfig(cfg *config.Config) map[string]any {
	if len(cfg.DNS) > 0 {
		return cfg.DNS
	}
	if cfg.DNSPreset != config.DNSPresetFakeIP && cfg.DNSPreset != config.DNSPresetRedirHost {
		return nil
	}

	// 两种预设共用不依赖域名引导的 IP nameserver，避免 DoH 域名在 DNS 尚未可用时
	// 形成启动循环。用户有地域、隐私或分流需求时应使用手写 dns: 覆盖整段。
	dnsConfig := map[string]any{
		"enable":             true,
		"ipv6":               false,
		"use-hosts":          true,
		"use-system-hosts":   true,
		"enhanced-mode":      cfg.DNSPreset,
		"default-nameserver": []string{"223.5.5.5", "1.1.1.1"},
		"nameserver":         []string{"223.5.5.5", "1.1.1.1"},
	}
	if cfg.DNSPreset == config.DNSPresetFakeIP {
		dnsConfig["fake-ip-range"] = "198.18.0.1/16"
		dnsConfig["fake-ip-filter"] = []string{"*.lan", "*.local", "localhost"}
	}
	return dnsConfig
}

// availableProxyNodes 合并已分配端口节点与额外健康节点，形成 mihomo 出站全集。
//
// 参数：
//   - assigns: []Assignment，必须优先保留且需要独立 listener 的节点。
//   - nodes: []*node.Node，本轮完整节点集；只补充 Alive 且名称未出现的节点。
//
// 返回值：
//   - []*node.Node，按 assignment 顺序优先、随后按输入节点顺序排列的去重结果。
//
// 错误情况：无；nil assignment 由 generate 主循环返回明确错误，这里只跳过以避免
// 在构造依赖集合时提前 panic。重名额外节点保留最先出现者，与订阅合并语义一致。
func availableProxyNodes(assigns []Assignment, nodes []*node.Node) []*node.Node {
	out := make([]*node.Node, 0, len(assigns)+len(nodes))
	seen := make(map[string]bool, len(assigns)+len(nodes))
	for _, assignment := range assigns {
		if assignment.Node == nil || seen[assignment.Node.Name] {
			continue
		}
		seen[assignment.Node.Name] = true
		out = append(out, assignment.Node)
	}
	for _, n := range nodes {
		if n == nil || !n.Alive || seen[n.Name] {
			continue
		}
		seen[n.Name] = true
		out = append(out, n)
	}
	return out
}

// prepareOutboundMapping 返回写入 mihomo 配置的出站映射。
//
// 参数：
//   - cfg: *config.Config，提供 proxyd state-dir 用于 tsnet 状态目录改写。
//   - n: *node.Node，待注册节点。
//
// 返回值：map[string]any，tailscale 节点未显式设置 state-dir 时返回改写后的副本，
// 其余情况原样返回节点 Mapping（不复制）。
//
// 错误情况：无；用户显式设置的 state-dir 被尊重并打警告日志（配置生成层没有
// 校验告警通道，日志是唯一出口）。改写逻辑由 node.WithTunnelStateDir 实现，
// 与健康检测路径（pool）保持同一份状态目录语义。
func prepareOutboundMapping(cfg *config.Config, n *node.Node) map[string]any {
	m := n.Mapping
	if nodeTypeOf(m) != node.TunnelTypeTailscale {
		return m
	}
	out, rewritten := node.WithTunnelStateDir(cfg.StateDir, n)
	if !rewritten {
		log.Printf("[core] 节点 %q 显式设置了 tsnet state-dir，proxyd 不再代为固定目录；重启后的节点身份由该目录内容决定", n.Name)
		return m
	}
	return out
}

// nodeTypeOf 取出站映射的 type 字段；缺失或非字符串时返回空串。
func nodeTypeOf(m map[string]any) string {
	t, _ := m["type"].(string)
	return t
}

// resolveGroupMembersFromNodes 计算节点分组的实际成员。
//
// 参数：
//   - g: config.NodeGroup，用户配置的分组规则。
//   - nodes: []*node.Node，已注册到 mihomo 的全部健康出站节点。
//   - nodeSet: map[string]bool，健康出站节点名集合，用于显式 nodes 交集过滤。
//
// 返回值：
//   - []string，当前可用的 mihomo proxy 名称列表。
//
// 错误情况：无；引用不存在的订阅或节点会自然得到空成员，由调用方记录并跳过。
// 使用全部健康出站而非仅端口 assignment，是为了让 dialer-proxy 能可靠引用策略组，
// 即使该组成员因端口容量限制没有各自的本地入口。
func resolveGroupMembersFromNodes(g config.NodeGroup, nodes []*node.Node, nodeSet map[string]bool) []string {
	if g.Subscription != "" {
		members := make([]string, 0, len(nodes))
		for _, n := range nodes {
			if n != nil && n.Subscription == g.Subscription {
				members = append(members, n.Name)
			}
		}
		return members
	}
	members := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		if nodeSet[n] {
			members = append(members, n)
		}
	}
	return members
}

// hasGeoRules 判断规则里是否含 GEOSITE/GEOIP。
func hasGeoRules(rules []string) bool {
	for _, r := range rules {
		head, _, _ := strings.Cut(r, ",")
		if strings.EqualFold(head, "GEOSITE") || strings.EqualFold(head, "GEOIP") {
			return true
		}
	}
	return false
}

// stripGeoRules 剔除 GEOSITE/GEOIP 规则行。
func stripGeoRules(rules []string) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		head, _, _ := strings.Cut(r, ",")
		if strings.EqualFold(head, "GEOSITE") || strings.EqualFold(head, "GEOIP") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// firstLine 取错误信息首行（geo 错误常带长堆栈）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// isLoopback 判断监听地址是否为回环地址，用于推断 allow-lan。
// 域名等无法直接解析为 IP 的情况按非回环处理。
func isLoopback(addr string) bool {
	ip, err := netip.ParseAddr(addr)
	return err == nil && ip.IsLoopback()
}
