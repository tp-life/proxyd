package subscribe

import (
	"errors"
	"fmt"
	"strconv"

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
		normalizeClashProxy(p)
		nodes = append(nodes, newNode(p, name, subName))
	}
	return nodes, nil
}

// normalizeClashProxy 修正第三方订阅转换器常见的 YAML 标量类型偏差。
//
// 参数说明：
//   - mapping: map[string]any，单个 Clash 节点的原始字段；函数会原地规范化。
//
// 返回值说明：无；不相关字段和值保持原样。
//
// 错误情况：本函数不返回错误。Reality short-id 本质是十六进制字符串，但未加引号的
// 纯数字值会被 YAML 解码为 int/uint64，而 mihomo 的 RealityOptions.ShortID 只接受
// string。这里只恢复无歧义的整数词法值；浮点数、对象等异常类型继续保留，让 mihomo
// 在配置自检阶段明确拒绝，避免把真正损坏的订阅静默改成另一种含义。
func normalizeClashProxy(mapping map[string]any) {
	realityOptions, ok := mapping["reality-opts"].(map[string]any)
	if !ok {
		return
	}
	shortID, exists := realityOptions["short-id"]
	if !exists {
		return
	}
	switch value := shortID.(type) {
	case int:
		realityOptions["short-id"] = strconv.Itoa(value)
	case int64:
		realityOptions["short-id"] = strconv.FormatInt(value, 10)
	case uint64:
		realityOptions["short-id"] = strconv.FormatUint(value, 10)
	}
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
