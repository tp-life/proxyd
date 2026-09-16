// Package node 定义 proxyd 各层共享的代理节点领域模型与稳定身份规则。
package node

import "strings"

// Node 表示从订阅或手动配置解析出的一个代理服务器实体。
type Node struct {
	// Name 是跨订阅唯一的展示名；名称冲突时由 subscribe.Merge 添加订阅后缀。
	Name string
	// Subscription 是节点来源订阅名；手动节点使用固定来源名 manual。
	Subscription string
	// Mapping 是 mihomo 出站代理原始映射，内部 name 始终与 Name 同步。
	Mapping map[string]any
	// Alive 表示最近一次普通节点或完整链路健康检测是否成功。
	Alive bool
	// Delay 是最近一次健康检测的毫秒延迟；未知或失败时为 0。
	Delay uint16
	// FailReason 是最近一次健康检测失败的原因（Alive=true 时为空）。
	FailReason string
}

// Key 返回用于端口快照、main-node 持久化以及通用去重基础的稳定节点身份。
//
// 参数：无；方法读取当前 Node.Mapping。
//
// 返回值：string，由协议、地址、端口、主凭据组成；链式节点额外包含
// dialer-proxy 名称，避免同一服务器经不同上游拨号时被错误去重。凭据提取优先级为
// uuid → password → auth-key → private-key（后两者覆盖 tailscale/wireguard/ssh 等
// 隧道类出站）。tailscale 类型无常规 server/port 时以 control-url 顶替 server 位置
// 参与身份。普通节点保持历史 Key 格式不变，从而兼容已经持久化的 main-node 和端口映射快照。
// 合并节点时应调用 DedupKey；它会为拥有独立 tsnet 状态的 Tailscale 出站补充名称维度。
//
// 错误情况：无；缺失或未知类型字段按空字符串参与身份计算。
func (n *Node) Key() string {
	m := n.Mapping
	get := func(k string) string {
		if v, ok := m[k]; ok {
			switch s := v.(type) {
			case string:
				return s
			case int:
				return itoa(s)
			case float64:
				return itoa(int(s))
			}
		}
		return ""
	}
	cred := get("uuid")
	if cred == "" {
		cred = get("password")
	}
	if cred == "" {
		cred = get("auth-key")
	}
	if cred == "" {
		cred = get("private-key")
	}
	server := get("server")
	// tailscale 出站没有常规 server/port，控制面地址才是节点身份的稳定部分。
	if server == "" && get("type") == TunnelTypeTailscale {
		server = get("control-url")
	}
	key := get("type") + "|" + server + "|" + get("port") + "|" + cred
	if dialer := n.DialerProxy(); dialer != "" {
		key += "|dialer=" + dialer
	}
	return key
}

// DedupKey 返回节点合并阶段使用的领域身份，不改变端口快照与 main-node 持久化所用
// 的历史 Key 格式。
//
// 参数说明：无；方法读取 Node.Name 与 Node.Mapping，不修改节点内容。
//
// 返回值说明：普通节点直接返回 Key；Tailscale 节点额外包含出站名称，因为 mihomo
// 会按名称为每个出站分配独立 tsnet 状态目录与设备身份。同一控制面下两个没有
// auth-key 的审批节点因此仍是两个聚合实体，不能按空凭据合并。
//
// 错误情况：无；nil 节点返回空字符串。名称为空的异常 Tailscale 节点仍生成稳定
// 后缀，后续配置解析会负责报告缺少名称，而不会在此阶段 panic。
func (n *Node) DedupKey() string {
	if n == nil {
		return ""
	}
	key := n.Key()
	if !n.IsTailscale() {
		return key
	}
	return key + "|identity=" + strings.TrimSpace(n.Name)
}

// DialerProxy 返回该节点配置的链式拨号目标。
//
// 参数：无；方法读取 Mapping 中 mihomo 标准字段 `dialer-proxy`。
//
// 返回值：string，去除首尾空白后的代理或代理组名称；未配置时为空字符串。
//
// 错误情况：无；nil Mapping 或非字符串字段按未配置处理，真正的配置类型错误会在
// mihomo 适配器解析阶段进入节点 FailReason。
func (n *Node) DialerProxy() string {
	if n == nil || n.Mapping == nil {
		return ""
	}
	value, _ := n.Mapping["dialer-proxy"].(string)
	return strings.TrimSpace(value)
}

// itoa 把非负整数转换为十进制文本，供稳定身份生成使用。
//
// 参数：
//   - i: int，节点端口等非负整数。
//
// 返回值：string，十进制表示；0 返回 "0"。
//
// 错误情况：该轻量实现不支持负数；调用点只传入协议端口，因此不会出现负值。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [12]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
