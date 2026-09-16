package subscribe

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"proxyd/internal/proxy/node"
)

// ManualSubscription 是手动添加节点统一的来源标记。
const ManualSubscription = "manual"

// ParseManualNode 解析一条手动节点条目，来源标记为 manual。
// 字符串条目支持 http(s)://[user:pass@]host:port[#名称]、socks5://[user:pass@]host:port[#名称]，
// 以及全部现有分享链接格式（ss/ssr/vmess/vless/trojan/hy2/tuic）。
// 映射条目是结构化 mihomo 出站，仅接受隧道类（VPN）类型
// （tailscale/openvpn/zerotier/wireguard/ssh，见 docs/adr/0002）。
func ParseManualNode(entry any) (*node.Node, error) {
	switch typed := entry.(type) {
	case string:
		return parseManualString(typed)
	case map[string]any:
		return parseManualProxy(typed)
	default:
		return nil, fmt.Errorf("条目类型无效（应为 URL 字符串或出站映射）")
	}
}

// parseManualString 解析字符串形式的手动节点条目（代理 URL 或分享链接）。
func parseManualString(entry string) (*node.Node, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return nil, errors.New("空条目")
	}
	switch {
	case strings.HasPrefix(entry, "http://"), strings.HasPrefix(entry, "https://"),
		strings.HasPrefix(entry, "socks5://"), strings.HasPrefix(entry, "socks5h://"):
		return parseManualProxyURL(entry)
	default:
		m, name, err := parseShareLink(entry)
		if err != nil {
			return nil, err
		}
		return newNode(m, name, ManualSubscription), nil
	}
}

// parseManualProxy 校验并登记一份结构化 VPN 出站映射。只做 proxyd 关心的
// 白名单与必填字段校验，其余字段原样透传给 mihomo 解析兜底。
func parseManualProxy(mapping map[string]any) (*node.Node, error) {
	name, _ := mapping["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("结构化节点缺少 name 字段")
	}
	if !node.IsTunnel(mapping) {
		typ, _ := mapping["type"].(string)
		return nil, fmt.Errorf("结构化节点 %q 类型 %q 不支持（仅 tailscale|openvpn|zerotier|wireguard|ssh）", name, typ)
	}
	if err := validateManualProxyRequired(mapping); err != nil {
		return nil, fmt.Errorf("结构化节点 %q: %w", name, err)
	}
	// 复制映射避免 newNode 写 name 键时改动配置中的原始条目。
	m := make(map[string]any, len(mapping))
	for k, v := range mapping {
		m[k] = v
	}
	return newNode(m, name, ManualSubscription), nil
}

// validateManualProxyRequired 校验隧道类出站的必填凭据/地址字段。
// Tailscale 的 auth-key 故意保持可选：缺失时 mihomo/tsnet 会进入交互式注册并
// 输出控制面登录地址，供 proxyd 的一体化接入向导呈现给管理员审批。
func validateManualProxyRequired(mapping map[string]any) error {
	str := func(key string) string {
		value, _ := mapping[key].(string)
		return strings.TrimSpace(value)
	}
	typ, _ := mapping["type"].(string)
	switch typ {
	case node.TunnelTypeOpenVPN:
		if str("server") == "" {
			return errors.New("openvpn 出站必须提供 server")
		}
		if port, ok := mapping["port"].(int); !ok || port <= 0 {
			if portF, okF := mapping["port"].(float64); !okF || portF <= 0 {
				return errors.New("openvpn 出站必须提供有效的 port")
			}
		}
		if str("ca") == "" {
			return errors.New("openvpn 出站必须提供 ca 证书材料")
		}
	}
	return nil
}

// ParseManualNodes 解析全部手动节点条目；逐条容错，返回节点与每条的错误（nil 表示成功）。
func ParseManualNodes(entries []any) ([]*node.Node, []error) {
	nodes := make([]*node.Node, 0, len(entries))
	errs := make([]error, len(entries))
	for i, e := range entries {
		n, err := ParseManualNode(e)
		if err != nil {
			errs[i] = fmt.Errorf("manual-nodes[%d] %v: %w", i, manualEntrySummary(e), err)
			continue
		}
		nodes = append(nodes, n)
	}
	return nodes, errs
}

// parseManualProxyURL 解析 http(s)/socks5 代理 URL 为 mihomo outbound map。
func parseManualProxyURL(entry string) (*node.Node, error) {
	u, err := url.Parse(entry)
	if err != nil {
		return nil, err
	}
	host, port, err := splitHostPort(u.Host)
	if err != nil {
		return nil, err
	}
	typ := "http"
	if u.Scheme == "socks5" || u.Scheme == "socks5h" {
		typ = "socks5"
	}
	m := map[string]any{
		"type":   typ,
		"server": host,
		"port":   port,
	}
	if u.User != nil {
		if name := u.User.Username(); name != "" {
			m["username"] = name
		}
		if pw, ok := u.User.Password(); ok {
			m["password"] = pw
		}
	}
	if u.Scheme == "https" {
		m["tls"] = true
	}
	return newNode(m, fallbackName(fragmentName(u), host, port), ManualSubscription), nil
}

// ManualNodeName 从条目取节点名（字符串条目取 fragment/兜底，映射条目取 name 字段），供 API 展示。
func ManualNodeName(entry any) string {
	if m, ok := entry.(map[string]any); ok {
		name, _ := m["name"].(string)
		return name
	}
	n, err := ParseManualNode(entry)
	if err != nil {
		return ""
	}
	return n.Name
}

// manualEntrySummary 返回手动节点条目的短标识（供解析错误日志使用）：
// 字符串原样引用，映射取 name/type，避免把整份证书材料写进日志。
func manualEntrySummary(entry any) string {
	switch typed := entry.(type) {
	case string:
		return fmt.Sprintf("%q", typed)
	case map[string]any:
		name, _ := typed["name"].(string)
		typ, _ := typed["type"].(string)
		return fmt.Sprintf("%q (%s)", name, typ)
	default:
		return fmt.Sprintf("%v", typed)
	}
}
