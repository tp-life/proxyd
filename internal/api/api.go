// Package api 提供 proxyd 自有的 HTTP API 与内嵌 Web 控制台。
// mihomo 的 external-controller 路由不可扩展，因此使用独立监听地址。
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"proxyd/internal/app"
	"proxyd/internal/config"
	"proxyd/internal/node"
)

//go:embed ui.html
var uiHTML []byte

// PortEntry 是映射表中的单条端口记录。
type PortEntry struct {
	Port         int    `json:"port"`
	Node         string `json:"node"`
	Subscription string `json:"subscription"`
	Delay        uint16 `json:"delay"`
	Alive        bool   `json:"alive"`
}

// NodeEntry 是节点列表中的单条记录。
type NodeEntry struct {
	Name         string `json:"name"`
	Type         string `json:"type"` // 出站协议（ss/vmess/...）
	Subscription string `json:"subscription"`
	Delay        uint16 `json:"delay"`
	Alive        bool   `json:"alive"`
	FailReason   string `json:"fail_reason,omitempty"` // 测速失败原因
	Port         int    `json:"port"`                  // 0 表示未映射
}

// SubEntry 是订阅聚合信息。
type SubEntry struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Total int    `json:"total"`
	Alive int    `json:"alive"`
}

// Overview 是 /api/overview 的响应。
type Overview struct {
	Mode        string             `json:"mode"`
	Listen      string             `json:"listen"`
	MixedPort   int                `json:"mixed_port"`
	AutoPort    int                `json:"auto_port"`    // 0 表示关闭
	SystemProxy bool               `json:"system_proxy"` // 配置状态（是否启用系统代理）
	PortRange   [2]int             `json:"port_range"`
	Subs        []SubEntry         `json:"subscriptions"`
	Ports       []PortEntry        `json:"ports"`
	Nodes       []NodeEntry        `json:"nodes"`
	CustomRules []string           `json:"custom_rules"`
	Groups      []config.NodeGroup `json:"groups"`
}

// Server 包装自有 API 与 Web 控制台的 HTTP 服务。
type Server struct {
	addr string
	app  *app.App
	srv  *http.Server
	ln   net.Listener
}

// New 创建 API 服务（addr 如 127.0.0.1:19091）。
func New(addr string, a *app.App) *Server {
	return &Server{addr: addr, app: a}
}

// Start 在后台启动监听；地址端口冲突立即返回错误。
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleUI)
	mux.HandleFunc("GET /ports", s.handlePorts) // 兼容旧接口
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("POST /api/mode", s.handleSetMode)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/test", s.handleTest)
	mux.HandleFunc("POST /api/subscriptions", s.handleAddSub)
	mux.HandleFunc("DELETE /api/subscriptions/{name}", s.handleDelSub)
	mux.HandleFunc("POST /api/port-range", s.handleSetPortRange)
	mux.HandleFunc("GET /api/rules", s.handleListRules)
	mux.HandleFunc("POST /api/rules", s.handleAddRule)
	mux.HandleFunc("DELETE /api/rules/{index}", s.handleDelRule)
	mux.HandleFunc("GET /api/groups", s.handleListGroups)
	mux.HandleFunc("POST /api/groups", s.handleAddGroup)
	mux.HandleFunc("DELETE /api/groups/{name}", s.handleDelGroup)
	mux.HandleFunc("POST /api/auto-port", s.handleSetAutoPort)
	mux.HandleFunc("POST /api/system-proxy", s.handleSetSystemProxy)
	mux.HandleFunc("GET /api/rule-urls", s.handleListRuleURLs)
	mux.HandleFunc("POST /api/rule-urls", s.handleAddRuleURL)
	mux.HandleFunc("DELETE /api/rule-urls/{name}", s.handleDelRuleURL)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

// Addr 返回实际监听地址（Start 传入 ":0" 时用于取回端口）。
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.addr
	}
	return s.ln.Addr().String()
}

// Shutdown 优雅关闭。
func (s *Server) Shutdown(ctx context.Context) {
	if s.srv != nil {
		_ = s.srv.Shutdown(ctx)
	}
}

func (s *Server) handleUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(uiHTML)
}

func (s *Server) handlePorts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.portEntries())
}

func (s *Server) portEntries() []PortEntry {
	assigns := s.app.Assignments()
	entries := make([]PortEntry, 0, len(assigns))
	for _, as := range assigns {
		entries = append(entries, PortEntry{
			Port:         as.Port,
			Node:         as.Node.Name,
			Subscription: as.Node.Subscription,
			Delay:        as.Node.Delay,
			Alive:        as.Node.Alive,
		})
	}
	return entries
}

func (s *Server) handleOverview(w http.ResponseWriter, _ *http.Request) {
	cfg := s.app.Config()
	portOf := map[string]int{}
	for _, as := range s.app.Assignments() {
		portOf[as.Node.Name] = as.Port
	}

	subs := map[string]int{} // 订阅名 -> ov.Subs 下标
	ov := Overview{
		Mode:        s.app.Mode(),
		Listen:      cfg.Listen,
		MixedPort:   cfg.MixedPort,
		AutoPort:    cfg.AutoPort,
		SystemProxy: cfg.SystemProxy,
		PortRange:   cfg.PortRange,
		Subs:        []SubEntry{},
		Nodes:       []NodeEntry{},
		CustomRules: s.app.CustomRules(),
		Groups:      s.app.Groups(),
	}
	for _, sub := range s.app.Subscriptions() {
		subs[sub.Name] = len(ov.Subs)
		ov.Subs = append(ov.Subs, SubEntry{Name: sub.Name, URL: sub.URL})
	}
	for _, n := range s.app.Nodes() {
		ov.Nodes = append(ov.Nodes, NodeEntry{
			Name:         n.Name,
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
	}
	ov.Ports = s.portEntries()
	writeJSON(w, ov)
}

func (s *Server) handleSetMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetMode(req.Mode); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"mode": s.app.Mode()})
}

func (s *Server) handleAddSub(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		http.Error(w, "bad request: url required", http.StatusBadRequest)
		return
	}
	sub, err := s.app.AddSubscription(req.Name, req.URL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.trigger(true)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, sub)
}

func (s *Server) handleDelSub(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RemoveSubscription(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.trigger(true)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRefresh(w http.ResponseWriter, _ *http.Request) {
	s.trigger(true)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"refreshing"}`))
}

// handleTest 手动测速：只对现有节点做健康检测/延迟测试，不重新拉订阅。
func (s *Server) handleTest(w http.ResponseWriter, _ *http.Request) {
	s.trigger(false)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"testing"}`))
}

// trigger 异步执行一轮流水线；fetch=true 拉订阅+测速，false 仅测速。
// 完整一轮可能耗时数十秒（订阅拉取 + 全部节点测速）。
func (s *Server) trigger(fetch bool) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := s.app.Refresh(ctx, fetch); err != nil {
			log.Printf("[api] trigger(fetch=%v): %v", fetch, err)
		}
	}()
}

// handleSetPortRange 修改节点映射端口区间：持久化 + 重新分配端口 + 热更新（不同步测速）。
func (s *Server) handleSetPortRange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Range string `json:"range"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pr, err := config.ParsePortRange(req.Range)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.app.SetPortRange(pr[0], pr[1]); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string][2]int{"port_range": pr})
}

func (s *Server) handleListRules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.CustomRules())
}

// handleAddRule 追加自定义规则（前置到内置规则之前），持久化 + 热更新。
func (s *Server) handleAddRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rule string `json:"rule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Rule) == "" {
		http.Error(w, "bad request: rule required", http.StatusBadRequest)
		return
	}
	if err := s.app.AddRule(req.Rule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, s.app.CustomRules())
}

func (s *Server) handleDelRule(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		http.Error(w, "bad request: index must be an integer", http.StatusBadRequest)
		return
	}
	if err := s.app.RemoveRule(index); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListGroups(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.Groups())
}

// handleAddGroup 新增节点分组（组内 url-test 自动选优），持久化 + 热更新。
func (s *Server) handleAddGroup(w http.ResponseWriter, r *http.Request) {
	var g config.NodeGroup
	if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.AddGroup(g); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, g)
}

func (s *Server) handleDelGroup(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RemoveGroup(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetAutoPort 设置自动选优端口（0=关闭），持久化 + 热更新。
func (s *Server) handleSetAutoPort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetAutoPort(req.Port); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]int{"auto_port": req.Port})
}

// handleSetSystemProxy 开关系统代理（指向主端口）。
func (s *Server) handleSetSystemProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetSystemProxy(req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"system_proxy": req.Enabled})
}

func (s *Server) handleListRuleURLs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.RuleURLs())
}

// handleAddRuleURL 新增规则源：持久化 + 立即拉取该源 + 热更新。
func (s *Server) handleAddRuleURL(w http.ResponseWriter, r *http.Request) {
	var ru config.RuleURL
	if err := json.NewDecoder(r.Body).Decode(&ru); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.AddRuleURL(ru); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, s.app.RuleURLs())
}

func (s *Server) handleDelRuleURL(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RemoveRuleURL(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// nodeType 取节点的出站协议名。
func nodeType(n *node.Node) string {
	if t, ok := n.Mapping["type"].(string); ok {
		return t
	}
	return ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
