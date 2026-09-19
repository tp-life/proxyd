package node

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"unicode"
)

// 隧道类（VPN 语义）出站类型集合。这些出站代表整网隧道出口而非普通代理五元组：
// 首次拨号慢（tsnet 需 DERP 协商、openvpn 需 TLS 握手）、延迟天然偏高、故障时用户
// 预期是断流而非明文回退。因此它们不参与每节点端口映射，仅通过分组出口或内置
// PROXY 组的默认出口暴露（见 docs/adr/0002-vpn-outbounds-single-binary.md）。
const (
	// TunnelTypeTailscale 是 mihomo 内置的 tsnet 出站（需 with_gvisor 构建标签）。
	TunnelTypeTailscale = "tailscale"
	// TunnelTypeOpenVPN 是 mihomo 内置的 OpenVPN 出站。
	TunnelTypeOpenVPN = "openvpn"
	// TunnelTypeZeroTier 是 mihomo 内置的 ZeroTier 出站。
	TunnelTypeZeroTier = "zerotier"
	// TunnelTypeWireGuard 虽有常规 server:port，但语义是整网隧道出口，同样归类。
	TunnelTypeWireGuard = "wireguard"
	// TunnelTypeSSH 是整网 SSH 隧道出站。
	TunnelTypeSSH = "ssh"
)

// tunnelTypes 是 IsTunnel 的判定集合；mihomo 新增隧道类出站时在此登记。
var tunnelTypes = map[string]bool{
	TunnelTypeTailscale: true,
	TunnelTypeOpenVPN:   true,
	TunnelTypeZeroTier:  true,
	TunnelTypeWireGuard: true,
	TunnelTypeSSH:       true,
}

// IsTunnel 判断一份 mihomo 出站映射是否为隧道类（VPN 语义）出站。
//
// 参数：
//   - m: map[string]any，mihomo 出站原始映射，按 type 字段判定。
//
// 返回值：bool，type 属于隧道类型集合时返回 true；映射缺失 type 或为 nil 时返回 false。
//
// 错误情况：无；未知类型一律按普通节点处理，保持向后兼容。
func IsTunnel(m map[string]any) bool {
	t, _ := m["type"].(string)
	return tunnelTypes[t]
}

// IsTunnel 报告该节点是否为隧道类出站。
//
// 参数：无；方法读取当前 Node.Mapping。
//
// 返回值：bool，nil 节点或无 type 字段时返回 false。
//
// 错误情况：无。
func (n *Node) IsTunnel() bool {
	return n != nil && IsTunnel(n.Mapping)
}

// IsTailscale 判断节点是否为交由 mihomo 管理的 Tailscale 出站。
//
// 参数：无；方法读取当前 Node.Mapping 的 type 字段。
//
// 返回值：bool，节点非 nil 且 type=tailscale 时返回 true。
//
// 错误情况：无；映射缺失、type 类型错误或 nil 节点均返回 false。该判断只表达
// 出站所有权，不引入 Tailscale SDK；登录、控制面和数据面生命周期全部由 mihomo
// 的 Tailscale Adapter 负责。
func (n *Node) IsTailscale() bool {
	if n == nil {
		return false
	}
	typ, _ := n.Mapping["type"].(string)
	return typ == TunnelTypeTailscale
}

// TailscaleExitNode 返回 Tailscale 出站配置的 exit-node 标识。
//
// 参数：无；方法读取当前 Node.Mapping 的 mihomo 标准字段 `exit-node`。
//
// 返回值：string，去除首尾空白后的节点 IP、名称或 auto:* 选择器；未配置时为空。
//
// 错误情况：无；非 Tailscale 节点、字段缺失或字段类型错误均返回空字符串。只有
// 配置了 Exit Node 的出站才能用全局公网 health-url 判断连通性；仅访问 Tailnet 或
// 子网路由的出站不能用公网目标判死。
func (n *Node) TailscaleExitNode() string {
	if !n.IsTailscale() {
		return ""
	}
	value, _ := n.Mapping["exit-node"].(string)
	return strings.TrimSpace(value)
}

// TunnelStateDir 计算隧道节点隔离的 tsnet 状态目录，防止重启后重新认证与节点身份漂移。
// 目录名 = 安全化节点名 + Key 哈希：Key 含 `|`、URL 等路径不安全字符，不能直接做目录名；
// 哈希后缀保证重名节点（不同凭据/控制面）仍获得隔离目录。
func TunnelStateDir(stateDir string, n *Node) string {
	sum := sha256.Sum256([]byte(n.Key()))
	return filepath.Join(stateDir, "tsnet", safeDirName(n.Name)+"-"+hex.EncodeToString(sum[:4]))
}

// WithTunnelStateDir 为 tailscale 节点返回写入隔离状态目录后的映射副本。
//
// 参数：
//   - stateDir: string，proxyd 状态目录；为空时不做改写。
//   - n: *Node，待处理节点。
//
// 返回值：
//   - map[string]any，改写后的映射副本；不改写时原样返回 n.Mapping（不复制）。
//   - bool，是否发生了改写。
//
// 错误情况：无；非 tailscale 节点、已显式设置 state-dir、stateDir 为空或 nil 节点
// 均原样返回。配置生成（core）与健康检测（pool）都必须经此函数创建 tsnet 出站，
// 否则 mihomo 会把状态写入默认目录 <state-dir>/tailscale，多节点互相覆盖且与运行态
// 实例身份不一致。改写必须发生在副本上：Mapping 会被原样存入节点快照并参与 Key()
// 计算，原地修改会污染稳定身份。
func WithTunnelStateDir(stateDir string, n *Node) (map[string]any, bool) {
	if n == nil {
		return nil, false
	}
	m := n.Mapping
	if stateDir == "" {
		return m, false
	}
	if t, _ := m["type"].(string); t != TunnelTypeTailscale {
		return m, false
	}
	if dir, _ := m["state-dir"].(string); strings.TrimSpace(dir) != "" {
		return m, false
	}
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out["state-dir"] = TunnelStateDir(stateDir, n)
	return out, true
}

// safeDirName 把节点名收敛为跨平台安全的目录名片段：保留字母（含中文）、数字和
// ._-，其余字符替换为下划线；空结果回退为 "node"。
func safeDirName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "node"
	}
	return b.String()
}
