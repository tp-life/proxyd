package subscribe

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"

	"proxyd/internal/proxy/node"
)

// ParseClash 解析 Clash 订阅 YAML，取顶层 proxies 列表。
// 每个 proxy 项原样保留所有字段作为 Node.Mapping；
// 缺失 name 或 name 为空的项会被跳过。
func ParseClash(body []byte, subName string) ([]*node.Node, error) {
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("解析 Clash 订阅失败: %w", err)
	}
	if doc.Proxies == nil {
		return nil, errors.New("clash 订阅缺少顶层 proxies 列表")
	}
	nodes := make([]*node.Node, 0, len(doc.Proxies))
	for _, p := range doc.Proxies {
		name, _ := p["name"].(string)
		if name == "" {
			continue // 缺 name 的坏项，跳过
		}
		nodes = append(nodes, newNode(p, name, subName))
	}
	return nodes, nil
}

// newNode 用解析出的 mapping 构造节点，并同步 name 键与来源订阅。
func newNode(m map[string]any, name, subName string) *node.Node {
	m["name"] = name
	return &node.Node{
		Name:         name,
		Subscription: subName,
		Mapping:      m,
	}
}
