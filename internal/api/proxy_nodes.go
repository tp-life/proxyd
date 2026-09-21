package api

// 代理域：节点列表、手动节点与 overview 聚合视图接口。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"proxyd/internal/app"
	"proxyd/internal/autostart"
	"proxyd/internal/proxy/node"
)

// registerProxyNodeRoutes 注册 overview 聚合视图与手动节点路由。
func (s *Server) registerProxyNodeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/manual-nodes", s.handleListManualNodes)
	mux.HandleFunc("POST /api/manual-nodes", s.handleAddManualNode)
	mux.HandleFunc("DELETE /api/manual-nodes/{index}", s.handleDelManualNode)
}

// NodeEntry 是节点列表中的单条记录。
type NodeEntry struct {
	Name         string `json:"name"`
	Key          string `json:"key"`  // 稳定身份（协议+地址+凭据），去重与稳定端口分配按它识别节点
	Type         string `json:"type"` // 出站协议（ss/vmess/...）
	Subscription string `json:"subscription"`
	Delay        uint16 `json:"delay"`
	Alive        bool   `json:"alive"`
	FailReason   string `json:"fail_reason,omitempty"` // 测速失败原因
	Port         int    `json:"port"`                  // 0 表示未映射
	Tunnel       bool   `json:"tunnel,omitempty"`      // 隧道类（VPN 语义）出站：不参与端口映射，经分组出口使用
	// Testing 表示该节点本轮健康检测尚未产出结果：延迟列应显示「测速中…」，
	// delay/alive 是本轮开始前的稳定值。测速结束后逐节点变为 false。
	Testing bool `json:"testing,omitempty"`
}

// Overview 是 /api/overview 的响应。
type Overview struct {
	Mode               string                  `json:"mode"`
	Listen             string                  `json:"listen"`
	MixedPort          int                     `json:"mixed_port"`
	AutoPort           int                     `json:"auto_port"`            // 0 表示关闭
	PortMappingEnabled bool                    `json:"port_mapping_enabled"` // 一对一节点端口 listener 是否实际启用
	AdBlockEnabled     bool                    `json:"adblock_enabled"`      // 广告拦截是否开启（规则模式恒生效）
	SystemProxy        bool                    `json:"system_proxy"`         // 配置状态（是否启用系统代理）
	TUN                app.TUNStatus           `json:"tun"`                  // TUN 开关与当前进程权限状态
	DNSPreset          string                  `json:"dns_preset"`           // off|fake-ip|redir-host
	DNSCustom          bool                    `json:"dns_custom"`           // true 表示手写 dns 段优先，预设暂不生效
	Version            app.VersionCheckStatus  `json:"version_check"`        // 启动异步检查的缓存状态，不在 overview 请求中联网
	Autostart          bool                    `json:"autostart"`            // OS 级开机自启项是否存在（实时查询）
	AutostartRuntime   autostart.RuntimeStatus `json:"autostart_runtime"`    // 注册与托管进程状态分开，启动失败不会隐藏在开关后
	ServerTime         string                  `json:"server_time"`          // 服务器本地时间（RFC3339，带时区偏移），供概览"更新于"显示
	PortRange          [2]int                  `json:"port_range"`
	Testing            bool                    `json:"testing"` // 任一轮健康检测进行中；前端据此加密轮询，逐节点的延迟列以 NodeEntry.Testing 为准
	Subs               []SubEntry              `json:"subscriptions"`
	ManualNodes        []app.ManualNodeEntry   `json:"manual_nodes"`
	Ports              []PortEntry             `json:"ports"`            // 当前实际监听的一对一节点端口
	PortAssignments    []PortEntry             `json:"port_assignments"` // 稳定分配快照；关闭映射时仍保留
	Nodes              []NodeEntry             `json:"nodes"`
	CustomRules        []string                `json:"custom_rules"`
	Groups             []GroupEntry            `json:"groups"`
}

// handleOverview 聚合应用内存快照，返回控制台一次轮询所需的完整只读状态。
//
// 参数：
//   - w: http.ResponseWriter，写出 JSON 响应。
//   - 请求参数未使用；该接口不接受筛选条件。
//
// 返回值：无；成功返回 HTTP 200 与 Overview JSON。
//
// 错误情况：系统自启查询采用有超时的本机 I/O，故障通过状态消息返回；版本检查只读缓存。
func (s *Server) handleOverview(w http.ResponseWriter, _ *http.Request) {
	cfg := s.app.Config()
	subInfos := s.app.SubscriptionUserInfos()
	portOf := map[string]int{}
	if cfg.PortMappingEnabled() {
		for _, as := range s.app.Assignments() {
			// 订阅级映射关闭时节点不参与监听，Port 保持 0，与真实运行态一致。
			if as.Node != nil && cfg.PortMappingEnabledFor(as.Node.Subscription) {
				portOf[as.Node.Name] = as.Port
			}
		}
	}

	subs := map[string]int{} // 订阅名 -> ov.Subs 下标
	autostartRuntime := s.app.AutostartRuntime()
	ov := Overview{
		Mode:               s.app.Mode(),
		Listen:             cfg.Listen,
		MixedPort:          cfg.MixedPort,
		AutoPort:           cfg.AutoPort,
		PortMappingEnabled: cfg.PortMappingEnabled(),
		AdBlockEnabled:     cfg.AdBlock.Enable,
		SystemProxy:        cfg.SystemProxy && !cfg.ProxyDisabled,
		TUN:                s.app.TUNStatus(),
		DNSPreset:          cfg.DNSPreset,
		DNSCustom:          len(cfg.DNS) > 0,
		Version:            s.app.VersionStatus(),
		Autostart:          autostartRuntime.Enabled,
		AutostartRuntime:   autostartRuntime,
		ServerTime:         time.Now().Format(time.RFC3339), // 服务器本地时间（含时区偏移）
		PortRange:          cfg.PortRange,
		Testing:            s.app.Testing(),
		Subs:               []SubEntry{},
		ManualNodes:        s.app.ManualNodes(),
		Nodes:              []NodeEntry{},
		CustomRules:        s.app.CustomRules(),
		Groups:             s.groupEntries(),
	}
	for _, sub := range s.app.Subscriptions() {
		subs[sub.Name] = len(ov.Subs)
		entry := SubEntry{Name: sub.Name, URL: sub.URL, Type: sub.Type, Enabled: sub.IsEnabled(), PortMapping: sub.PortMappingEnabled()}
		if info, ok := subInfos[sub.Name]; ok {
			entry.UserInfo = &info
		}
		ov.Subs = append(ov.Subs, entry)
	}
	for _, n := range s.app.Nodes() {
		entry := newNodeEntry(n, portOf[n.Name])
		ov.Nodes = append(ov.Nodes, entry)
		if i, ok := subs[n.Subscription]; ok {
			ov.Subs[i].Total++
			// 存活计数复用同一行展示值：订阅徽标与节点列表不会出现口径不一致，
			// 也不会在测速期间因为读到半成品状态而闪红。
			if entry.Alive {
				ov.Subs[i].Alive++
			}
		}
	}
	for index := range ov.Subs {
		subscription := &ov.Subs[index]
		switch {
		case !subscription.Enabled:
			subscription.State = "disabled"
		case subscription.Total == 0:
			subscription.State = "empty"
		case subscription.Alive == 0:
			subscription.State = "error"
		case subscription.Alive < subscription.Total:
			subscription.State = "degraded"
		default:
			subscription.State = "healthy"
		}
	}
	ov.Ports = s.portEntries()
	ov.PortAssignments = s.assignmentEntries()
	writeJSON(w, ov)
}

func (s *Server) handleListManualNodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.ManualNodes())
}

// handleAddManualNode 添加手动节点：校验 + 持久化，随后后台仅重建现有节点池。
// url 字段接受代理 URL/分享链接；proxy 字段接受结构化隧道类（VPN）出站映射
// （type 限 tailscale/openvpn/zerotier/wireguard/ssh），两者二选一。
//
// 参数：w 写出新节点或请求错误；r 的 JSON 在 url 与 proxy 字段之间二选一。
//
// 返回值：无；成功返回 HTTP 201 和脱敏后的手动节点展示值。
//
// 错误情况：JSON、节点协议/必填字段、名称唯一性或持久化失败时返回 400；后续
// 后台重建只解析本地配置并测试节点，不会因为手动节点变化而下载订阅。
func (s *Server) handleAddManualNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL   string         `json:"url"`
		Name  string         `json:"name,omitempty"`
		Proxy map[string]any `json:"proxy,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var (
		entry app.ManualNodeEntry
		err   error
	)
	switch {
	case req.Proxy != nil && strings.TrimSpace(req.URL) != "":
		http.Error(w, "bad request: url 与 proxy 只能二选一", http.StatusBadRequest)
		return
	case req.Proxy != nil:
		entry, err = s.app.AddManualProxy(req.Proxy, req.Name)
	default:
		if strings.TrimSpace(req.URL) == "" {
			http.Error(w, "bad request: url required", http.StatusBadRequest)
			return
		}
		entry, err = s.app.AddManualNode(req.URL, req.Name)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.trigger(false)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, entry)
}

// handleDelManualNode 删除手动节点，并在后台仅使用剩余缓存数据更新运行态。
//
// 参数：w 写出空响应或错误；r 的路径 index 是手动节点在配置列表中的下标。
//
// 返回值：无；成功返回 HTTP 204。
//
// 错误情况：下标非法或节点不存在时返回 400/404；后台重建不会拉取任何订阅。
func (s *Server) handleDelManualNode(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		http.Error(w, "bad request: index must be an integer", http.StatusBadRequest)
		return
	}
	if err := s.app.RemoveManualNode(index); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.trigger(false)
	w.WriteHeader(http.StatusNoContent)
}

// newNodeEntry 把运行态节点转换为列表记录；隧道类节点没有端口映射（Port 恒为 0）
// 是正常状态，通过 Tunnel 标识让控制台与 CLI 区分展示，而不是当作异常。
//
// 延迟与存活状态取自节点发布的展示行（见 node.Display）：健康检测进行中时它固定为本轮
// 开始前的稳定值并带 Testing 标记，因此概览既不会读到 worker 正在写回的中间状态，
// 也不会在测速期间让状态列与计数抖动。
func newNodeEntry(n *node.Node, port int) NodeEntry {
	display := n.Display()
	return NodeEntry{
		Name:         n.Name,
		Key:          n.Key(),
		Type:         nodeType(n),
		Subscription: n.Subscription,
		Delay:        display.Delay,
		Alive:        display.Alive,
		FailReason:   display.FailReason,
		Port:         port,
		Tunnel:       n.IsTunnel(),
		Testing:      display.Testing,
	}
}

// nodeType 取节点的出站协议名。
func nodeType(n *node.Node) string {
	if t, ok := n.Mapping["type"].(string); ok {
		return t
	}
	return ""
}
