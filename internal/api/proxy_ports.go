package api

// 代理域：端口区间、端口映射、auto-port、主端口与代理模式接口。

import (
	"encoding/json"
	"net/http"

	"proxyd/internal/config"
)

// registerProxyPortRoutes 注册端口区间/映射、auto-port、主端口与代理模式路由。
func (s *Server) registerProxyPortRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ports", s.handlePorts) // 兼容旧接口
	mux.HandleFunc("POST /api/mode", s.handleSetMode)
	mux.HandleFunc("POST /api/port-range", s.handleSetPortRange)
	mux.HandleFunc("POST /api/port-mapping", s.handleSetPortMapping)
	mux.HandleFunc("POST /api/auto-port", s.handleSetAutoPort)
	mux.HandleFunc("POST /api/main-auto", s.handleSetMainAuto)
	mux.HandleFunc("POST /api/main-node", s.handleSetMainNode)
	mux.HandleFunc("POST /api/main-port", s.handleSetMainPort)
}

// PortEntry 是映射表中的单条端口记录。
type PortEntry struct {
	Port         int    `json:"port"`
	Node         string `json:"node"`
	Subscription string `json:"subscription"`
	Delay        uint16 `json:"delay"`
	Alive        bool   `json:"alive"`
}

func (s *Server) handlePorts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.portEntries())
}

// assignmentEntries 把应用层稳定 assignments 转换为 API 只读记录。
//
// 参数：无。
//
// 返回值：[]PortEntry，包含当前稳定分配；调用方可以自由筛选而不修改应用状态。
//
// 错误情况：无；assignment 在应用层生成时已经保证 Node 非空。
func (s *Server) assignmentEntries() []PortEntry {
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

// portEntries 返回当前真正启用的一对一节点监听端口。
//
// 参数：无。
//
// 返回值：[]PortEntry；端口映射关闭时返回空切片，开启时返回稳定 assignments。
//
// 错误情况：无；该方法刻意区分“已分配但停用”和“正在监听”，避免 API 误报端口可用。
func (s *Server) portEntries() []PortEntry {
	if !s.app.Config().PortMappingEnabled() {
		return []PortEntry{}
	}
	return s.assignmentEntries()
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

// handleSetPortMapping 开关健康节点的一对一端口 listener，并返回提交后的有效状态。
//
// 参数：
//   - w: http.ResponseWriter，写出 JSON 响应或事务失败信息。
//   - r: *http.Request，请求体必须是 `{ "enabled": boolean }`。
//
// 返回值：无；成功返回 HTTP 200 与 port_mapping_enabled 字段。
//
// 错误情况：JSON 无效返回 400；热更新、持久化或回滚失败也返回 400，正文保留
// 应用层合并错误，便于 UI 明确提示用户检查运行态。
func (s *Server) handleSetPortMapping(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetPortMapping(req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"port_mapping_enabled": s.app.Config().PortMappingEnabled()})
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

// handleSetMainAuto 开关「主端口使用最优节点」（跳过规则匹配），持久化 + 热更新。
func (s *Server) handleSetMainAuto(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetMainAuto(req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"main_auto": req.Enabled})
}

// handleSetMainNode 设置主端口固定节点（node key；空串=恢复规则模式），持久化 + 热更新。
// main-auto 开启时该设置被忽略（auto 优先）。
func (s *Server) handleSetMainNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node string `json:"node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetMainNode(req.Node); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"main_node": req.Node})
}

// handleSetMainPort 修改主端口：校验冲突 + 持久化 + 热更新；
// 系统代理已开启时自动重新绑定到新端口。
func (s *Server) handleSetMainPort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetMainPort(req.Port); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]int{"mixed_port": req.Port})
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
