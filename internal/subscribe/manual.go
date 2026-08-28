package subscribe

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"proxyd/internal/node"
)

// ManualSubscription 是手动添加节点统一的来源标记。
const ManualSubscription = "manual"

// ParseManualNode 解析一条手动节点条目，来源标记为 manual。
// 支持 http(s)://[user:pass@]host:port[#名称]、socks5://[user:pass@]host:port[#名称]，
// 以及全部现有分享链接格式（ss/ssr/vmess/vless/trojan/hy2/tuic）。
func ParseManualNode(entry string) (*node.Node, error) {
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

// ParseManualNodes 解析全部手动节点条目；逐条容错，返回节点与每条的错误（nil 表示成功）。
func ParseManualNodes(entries []string) ([]*node.Node, []error) {
	nodes := make([]*node.Node, 0, len(entries))
	errs := make([]error, len(entries))
	for i, e := range entries {
		n, err := ParseManualNode(e)
		if err != nil {
			errs[i] = fmt.Errorf("manual-nodes[%d] %q: %w", i, e, err)
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

// ManualNodeName 从条目 URL 取节点名（fragment 或 host:port 兜底），供 API 展示。
func ManualNodeName(entry string) string {
	n, err := ParseManualNode(entry)
	if err != nil {
		return ""
	}
	return n.Name
}
