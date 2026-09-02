// Package core 负责生成 mihomo 配置并以库方式内嵌运行 mihomo 核心。
package core

import (
	"fmt"
	"log"
	"net/netip"
	"slices"
	"strings"

	"github.com/metacubex/mihomo/hub/executor"
	"gopkg.in/yaml.v3"

	"proxyd/internal/config"
	"proxyd/internal/node"
	"proxyd/internal/pool"
)

// Assignment 是端口到节点的映射（internal/pool 定义的别名）。
type Assignment = pool.Assignment

// Generate 生成完整的 mihomo YAML 配置：
// 主端口 mixed-port 默认走常规 Clash 规则模式（rule/global/direct）；
// main-auto 开启时主端口改为 listener 固定走 AUTO url-test 组（跳过规则匹配），
// 无可用节点时回退规则模式（打日志）；
// main-node 非空（且 main-auto 未开启）时主端口 listener 固定直达该节点（跳过规则），
// 节点当前不可用时回退规则模式（打日志，配置保留，恢复后自动再生效）；
// 每个节点分配一个 listener（type mixed，proxy 固定出口到该节点）；
// auto-port 开启时额外生成一个固定走 AUTO url-test 组（全部可用节点选优）的 listener；
// 每个节点分组额外生成一个 proxy-group + 固定走该组的 mixed listener；
// 分组可以显式列节点，也可以按订阅名动态取该订阅当前可用节点。
// tun 段由 config.TUNConfig 提供常用默认值并保留高级字段，生成时完整交给 mihomo；
// TUN 设备权限由应用层在开启前校验，本函数只负责配置语义与 mihomo 解析自检。
// imported 是 rule-urls 远程导入的规则（排在 custom-rules 之后、内置 rules 之前）。
//
// 注意：listener 名称统一用端口号（"L<port>"），不带节点/分组名。
// mihomo 热更新（PatchInboundListeners）先 Listen 新 listener、后关闭旧 listener，
// 同端口改名会 bind 冲突把端口打挂；同名但配置不同才会按 关闭→监听 的正确顺序处理。
// main-auto/main-node 切换时主端口在 mixed-port 与 listener 之间转换，同属 inbound 热更新范畴，
// 名称同样保持 L<port> 规范。
func Generate(cfg *config.Config, assigns []Assignment, imported []string) ([]byte, error) {
	return generate(cfg, assigns, nil, imported)
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
	return generate(cfg, assigns, nodes, imported)
}

// generate 实现 Generate 与 GenerateWithNodes 共用的配置翻译和 mihomo 自检流程。
//
// 参数：
//   - cfg: *config.Config，proxyd 运行配置。
//   - assigns: []Assignment，需要生成固定端口入口的节点。
//   - nodes: []*node.Node，可选完整健康节点集，用于链式依赖和策略组成员。
//   - imported: []string，已经清洗、去重的远程规则。
//
// 返回值：生成后的 YAML 字节与错误。
//
// 错误情况：assignment 缺节点、YAML 序列化或 mihomo 自检失败时返回错误；GEO 数据
// 不可用时沿用既有降级逻辑，移除 GEO 规则后再自检一次。
func generate(cfg *config.Config, assigns []Assignment, nodes []*node.Node, imported []string) ([]byte, error) {
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
	if dnsConfig := resolveDNSConfig(cfg); dnsConfig != nil {
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
		proxies = append(proxies, n.Mapping)
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
		nodeNames = append(nodeNames, a.Node.Name)
		if cfg.PortMappingEnabled() {
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

	// 主端口入口形态：main-auto（AUTO 组）优先于 main-node（固定节点），
	// 两者都未生效时回退顶层 mixed-port 规则模式。
	mainTarget := resolveMainInbound(cfg, assigns)
	if cfg.MainAuto && cfg.MainNode != "" {
		log.Printf("[core] main-auto 已开启，main-node 本轮被忽略（auto 优先）")
	}
	if cfg.MainAuto && mainTarget == "" {
		log.Printf("[core] main-auto 已开启但当前无可用节点，本轮主端口回退规则模式")
	}
	if !cfg.MainAuto && cfg.MainNode != "" && mainTarget == "" {
		log.Printf("[core] main-node 指定的节点当前不可用（失效或已消失），本轮主端口回退规则模式")
	}
	if mainTarget == "" {
		m["mixed-port"] = cfg.MixedPort
	}

	// 主端口规则模式下使用的选择组：所有节点 + 内置 DIRECT（空 assigns 时仅 DIRECT）。
	names := append(slices.Clone(nodeNames), "DIRECT")
	groups := []map[string]any{
		{"name": "PROXY", "type": "select", "proxies": names},
	}

	// AUTO url-test 组（全部可用节点中延迟最低）：auto-port 与 main-auto 共用。
	autoWanted := cfg.AutoPort > 0 || mainTarget == "AUTO"
	switch {
	case autoWanted && len(nodeNames) == 0:
		log.Printf("[core] auto-port %d 已开启但当前无可用节点，本轮跳过该 listener", cfg.AutoPort)
	case autoWanted:
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
	if mainTarget != "" {
		// 主端口以 listener 形式固定走 mainTarget（AUTO 组或指定节点）：
		// 规则匹配（自定义/内置规则）对其不再生效；节点映射端口、分组端口、auto-port 不受影响。
		listeners = append(listeners, map[string]any{
			"name":   fmt.Sprintf("L%d", cfg.MixedPort),
			"type":   "mixed",
			"listen": cfg.Listen,
			"port":   cfg.MixedPort,
			"proxy":  mainTarget,
		})
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

	// 规则合并顺序：用户 custom-rules 最前 → rule-urls 导入规则 → 内置规则
	//（追加在 GEOSITE/GEOIP/MATCH 之后永远不会命中，所以自定义/导入规则必须前置）。
	rules := make([]string, 0, len(cfg.CustomRules)+len(imported)+len(cfg.Rules))
	rules = append(rules, cfg.CustomRules...)
	rules = append(rules, imported...)
	rules = append(rules, cfg.Rules...)
	m["rules"] = rules
	if cfg.RuleProviders != nil {
		m["rule-providers"] = cfg.RuleProviders
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

// resolveMainInbound 计算主端口固定 listener 的 proxy 目标：
// main-auto 开启且有可用节点时为 "AUTO"（auto 优先，main-node 被忽略）；
// 否则 main-node 非空且该节点（按 Key 匹配）当前可用时为节点名；
// 其余情况返回空串 = 主端口回退顶层 mixed-port 规则模式。
func resolveMainInbound(cfg *config.Config, assigns []Assignment) string {
	if cfg.MainAuto {
		if len(assigns) > 0 {
			return "AUTO"
		}
		return ""
	}
	if cfg.MainNode != "" {
		for _, a := range assigns {
			if a.Node != nil && a.Node.Key() == cfg.MainNode {
				return a.Node.Name
			}
		}
	}
	return ""
}

// MainInboundIsListener 报告给定配置与节点下主端口是否为固定 listener 形态
// （供 App 判断 mixed-port ↔ listener 转换，决定是否需要先释放主端口再热更新）。
func MainInboundIsListener(cfg *config.Config, assigns []Assignment) bool {
	return resolveMainInbound(cfg, assigns) != ""
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
