package api

// 代理域：节点列表、手动节点与 overview 聚合视图接口。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"proxyd/internal/app"
	"proxyd/internal/config"
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
	Key          string `json:"key"`  // 稳定身份（协议+地址+凭据），main-node 等按它引用节点
	Type         string `json:"type"` // 出站协议（ss/vmess/...）
	Subscription string `json:"subscription"`
	Delay        uint16 `json:"delay"`
	Alive        bool   `json:"alive"`
	FailReason   string `json:"fail_reason,omitempty"` // 测速失败原因
	Port         int    `json:"port"`                  // 0 表示未映射
}

// Overview 是 /api/overview 的响应。
type Overview struct {
	Mode               string                 `json:"mode"`
	Listen             string                 `json:"listen"`
	MixedPort          int                    `json:"mixed_port"`
	MainAuto           bool                   `json:"main_auto"`            // 主端口是否固定走最优节点（跳过规则）
	MainNode           string                 `json:"main_node"`            // 主端口固定节点（node key；空串=跟随规则）
	MainNodeUp         bool                   `json:"main_node_up"`         // main-node 当前是否生效（节点可用且 main-auto 未开）
	AutoPort           int                    `json:"auto_port"`            // 0 表示关闭
	PortMappingEnabled bool                   `json:"port_mapping_enabled"` // 一对一节点端口 listener 是否实际启用
	SystemProxy        bool                   `json:"system_proxy"`         // 配置状态（是否启用系统代理）
	TUN                app.TUNStatus          `json:"tun"`                  // TUN 开关与当前进程权限状态
	DNSPreset          string                 `json:"dns_preset"`           // off|fake-ip|redir-host
	DNSCustom          bool                   `json:"dns_custom"`           // true 表示手写 dns 段优先，预设暂不生效
	Version            app.VersionCheckStatus `json:"version_check"`        // 启动异步检查的缓存状态，不在 overview 请求中联网
	Autostart          bool                   `json:"autostart"`            // OS 级开机自启项是否存在（实时查询）
	ServerTime         string                 `json:"server_time"`          // 服务器本地时间（RFC3339，带时区偏移），供概览"更新于"显示
	PortRange          [2]int                 `json:"port_range"`
	Subs               []SubEntry             `json:"subscriptions"`
	ManualNodes        []app.ManualNodeEntry  `json:"manual_nodes"`
	Ports              []PortEntry            `json:"ports"`            // 当前实际监听的一对一节点端口
	PortAssignments    []PortEntry            `json:"port_assignments"` // 稳定分配快照；关闭映射时仍保留
	Nodes              []NodeEntry            `json:"nodes"`
	CustomRules        []string               `json:"custom_rules"`
	Groups             []config.NodeGroup     `json:"groups"`
}

// handleOverview 聚合应用内存快照，返回控制台一次轮询所需的完整只读状态。
//
// 参数：
//   - w: http.ResponseWriter，写出 JSON 响应。
//   - 请求参数未使用；该接口不接受筛选条件。
//
// 返回值：无；成功返回 HTTP 200 与 Overview JSON。
//
// 错误情况：无外部 I/O；版本检查只读取 App 缓存，绝不会在十秒轮询路径同步访问 GitHub。
func (s *Server) handleOverview(w http.ResponseWriter, _ *http.Request) {
	cfg := s.app.Config()
	subInfos := s.app.SubscriptionUserInfos()
	portOf := map[string]int{}
	if cfg.PortMappingEnabled() {
		for _, as := range s.app.Assignments() {
			portOf[as.Node.Name] = as.Port
		}
	}

	subs := map[string]int{} // 订阅名 -> ov.Subs 下标
	ov := Overview{
		Mode:               s.app.Mode(),
		Listen:             cfg.Listen,
		MixedPort:          cfg.MixedPort,
		MainAuto:           cfg.MainAuto,
		MainNode:           cfg.MainNode,
		AutoPort:           cfg.AutoPort,
		PortMappingEnabled: cfg.PortMappingEnabled(),
		SystemProxy:        cfg.SystemProxy,
		TUN:                s.app.TUNStatus(),
		DNSPreset:          cfg.DNSPreset,
		DNSCustom:          len(cfg.DNS) > 0,
		Version:            s.app.VersionStatus(),
		Autostart:          s.app.AutostartStatus(),
		ServerTime:         time.Now().Format(time.RFC3339), // 服务器本地时间（含时区偏移）
		PortRange:          cfg.PortRange,
		Subs:               []SubEntry{},
		ManualNodes:        s.app.ManualNodes(),
		Nodes:              []NodeEntry{},
		CustomRules:        s.app.CustomRules(),
		Groups:             s.app.Groups(),
	}
	for _, sub := range s.app.Subscriptions() {
		subs[sub.Name] = len(ov.Subs)
		entry := SubEntry{Name: sub.Name, URL: sub.URL, Type: sub.Type, Enabled: sub.IsEnabled()}
		if info, ok := subInfos[sub.Name]; ok {
			entry.UserInfo = &info
		}
		ov.Subs = append(ov.Subs, entry)
	}
	for _, n := range s.app.Nodes() {
		ov.Nodes = append(ov.Nodes, NodeEntry{
			Name:         n.Name,
			Key:          n.Key(),
			Type:         nodeType(n),
			Subscription: n.Subscription,
			Delay:        n.Delay,
			Alive:        n.Alive,
			FailReason:   n.FailReason,
			Port:         portOf[n.Name],
		})
		if i, ok := subs[n.Subscription]; ok {
			ov.Subs[i].Total++
			if n.Alive {
				ov.Subs[i].Alive++
			}
		}
		// main-node 生效与否只取决于节点可用且 main-auto 未开（与内核 resolveMainInbound 一致）；
		// 节点是否有独立端口监听（portOf）受端口映射开关影响，与主端口固定 listener 无关，
		// 不能作为生效条件，否则关闭映射时概览会误报「固定节点不可用/已回退」。
		if cfg.MainNode != "" && !cfg.MainAuto && n.Alive && n.Key() == cfg.MainNode {
			ov.MainNodeUp = true
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

// handleAddManualNode 添加手动节点：校验 + 持久化，随后后台刷新纳入节点池。
func (s *Server) handleAddManualNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string `json:"url"`
		Name string `json:"name,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		http.Error(w, "bad request: url required", http.StatusBadRequest)
		return
	}
	entry, err := s.app.AddManualNode(req.URL, req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.trigger(true)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, entry)
}

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
	s.trigger(true)
	w.WriteHeader(http.StatusNoContent)
}

// nodeType 取节点的出站协议名。
func nodeType(n *node.Node) string {
	if t, ok := n.Mapping["type"].(string); ok {
		return t
	}
	return ""
}
