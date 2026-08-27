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
	"proxyd/internal/pool"
)

// Assignment 是端口到节点的映射（internal/pool 定义的别名）。
type Assignment = pool.Assignment

// Generate 生成完整的 mihomo YAML 配置字节：
// 主端口 mixed-port 走常规 Clash 规则模式（rule/global/direct）；
// 每个节点分配一个 listener（type mixed，proxy 固定出口到该节点）；
// auto-port 开启时额外生成一个固定走 AUTO url-test 组（全部可用节点选优）的 listener；
// 每个节点分组额外生成一个 url-test proxy-group + 固定走该组的 mixed listener。
// imported 是 rule-urls 远程导入的规则（排在 custom-rules 之后、内置 rules 之前）。
func Generate(cfg *config.Config, assigns []Assignment, imported []string) ([]byte, error) {
	m := map[string]any{
		"mixed-port":          cfg.MixedPort,
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
	if cfg.DNS != nil {
		m["dns"] = cfg.DNS
	}
	if cfg.GeoXUrl != nil {
		m["geox-url"] = cfg.GeoXUrl
	}

	proxies := make([]map[string]any, 0, len(assigns))
	nodeNames := make([]string, 0, len(assigns))
	nodeSet := make(map[string]bool, len(assigns))
	listeners := make([]map[string]any, 0, len(assigns)+len(cfg.Groups)+1)
	for _, a := range assigns {
		if a.Node == nil {
			return nil, fmt.Errorf("端口 %d 的 assignment 缺少节点", a.Port)
		}
		proxies = append(proxies, a.Node.Mapping)
		nodeNames = append(nodeNames, a.Node.Name)
		nodeSet[a.Node.Name] = true
		listeners = append(listeners, map[string]any{
			"name":   fmt.Sprintf("%s:%d", a.Node.Name, a.Port), // 节点名:端口，日志/面板里一眼可辨
			"type":   "mixed",
			"listen": cfg.Listen,
			"port":   a.Port,
			"proxy":  a.Node.Name,
		})
	}
	m["proxies"] = proxies

	// 主端口规则模式下使用的选择组：所有节点 + 内置 DIRECT（空 assigns 时仅 DIRECT）。
	names := append(slices.Clone(nodeNames), "DIRECT")
	groups := []map[string]any{
		{"name": "PROXY", "type": "select", "proxies": names},
	}

	if cfg.AutoPort > 0 {
		// 自动选优端口：固定走 AUTO url-test 组（全部可用节点中延迟最低）。
		if len(nodeNames) == 0 {
			log.Printf("[core] auto-port %d 已开启但当前无可用节点，本轮跳过该 listener", cfg.AutoPort)
		} else {
			groups = append(groups, map[string]any{
				"name":      "AUTO",
				"type":      "url-test",
				"proxies":   nodeNames,
				"url":       cfg.HealthURL,
				"interval":  300,
				"tolerance": 50,
			})
			listeners = append(listeners, map[string]any{
				"name":   fmt.Sprintf("AUTO:%d", cfg.AutoPort),
				"type":   "mixed",
				"listen": cfg.Listen,
				"port":   cfg.AutoPort,
				"proxy":  "AUTO",
			})
		}
	}

	// 节点分组：组名与节点名/保留名冲突或成员交集为空时跳过（打日志）。
	for _, g := range cfg.Groups {
		if nodeSet[g.Name] || strings.EqualFold(g.Name, "AUTO") || strings.EqualFold(g.Name, "PROXY") {
			log.Printf("[core] 分组 %q 与节点名/保留名冲突，跳过", g.Name)
			continue
		}
		members := make([]string, 0, len(g.Nodes))
		for _, n := range g.Nodes {
			if nodeSet[n] {
				members = append(members, n)
			}
		}
		if len(members) == 0 {
			log.Printf("[core] 分组 %q 与当前可用节点无交集，跳过", g.Name)
			continue
		}
		groups = append(groups, map[string]any{
			"name":      g.Name,
			"type":      "url-test",
			"proxies":   members,
			"url":       cfg.HealthURL,
			"interval":  300,
			"tolerance": 50,
		})
		listeners = append(listeners, map[string]any{
			"name":   fmt.Sprintf("%s:%d", g.Name, g.Port),
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
