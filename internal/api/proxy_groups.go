package api

// 代理域：节点分组接口。

import (
	"encoding/json"
	"net/http"
	"strings"

	"proxyd/internal/config"
)

// registerProxyGroupRoutes 注册节点分组路由。
func (s *Server) registerProxyGroupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/groups", s.handleListGroups)
	mux.HandleFunc("POST /api/groups", s.handleAddGroup)
	mux.HandleFunc("PUT /api/groups/{name}", s.handleUpdateGroup)
	mux.HandleFunc("DELETE /api/groups/{name}", s.handleDelGroup)
	mux.HandleFunc("POST /api/groups/{name}/select", s.handleSelectGroupNode)
}

// GroupEntry 是分组列表的展示项：配置值之外附带 select 分组的持久化选中项。
// 内嵌 NodeGroup 使 JSON 字段与配置结构保持一致，只在响应中追加运行态字段。
type GroupEntry struct {
	config.NodeGroup
	Selected string `json:"selected,omitempty"` // select 分组当前选中的节点名；非 select 组或未选择时为空
}

// groupEntries 合并分组配置与 select 选中状态；空 type 归一化为默认的 url-test，
// 让列表响应始终暴露生效中的分组类型。
func (s *Server) groupEntries() []GroupEntry {
	groups := s.app.Groups()
	selected := s.app.GroupSelected()
	out := make([]GroupEntry, 0, len(groups))
	for _, g := range groups {
		if g.Type == "" {
			g.Type = config.GroupTypeURLTest
		}
		out = append(out, GroupEntry{NodeGroup: g, Selected: selected[g.Name]})
	}
	return out
}

func (s *Server) handleListGroups(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.groupEntries())
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

// handleUpdateGroup 修改现有策略分组的端口、类型或成员来源。
//
// 参数：
//   - w: http.ResponseWriter，写出更新后的分组或应用层事务错误。
//   - r: *http.Request，路径 name 为当前分组名，请求体为完整 NodeGroup JSON。
//
// 返回值：无；成功返回 HTTP 200 与提交后的分组。
//
// 错误情况：请求体非法、分组不存在、尝试改名、端口冲突、热更新或持久化失败时返回 400。
func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	var group config.NodeGroup
	if err := json.NewDecoder(r.Body).Decode(&group); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.UpdateGroup(r.PathValue("name"), group); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, group)
}

func (s *Server) handleDelGroup(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RemoveGroup(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSelectGroupNode 修改 select 分组的手动选中节点；选中项持久化到
// state-dir/group-selected.json 并热更新，热更新失败时整体回滚（见 App.SetGroupSelected）。
func (s *Server) handleSelectGroupNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node string `json:"node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Node) == "" {
		http.Error(w, "bad request: node required", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	node := strings.TrimSpace(req.Node)
	if err := s.app.SetGroupSelected(name, node); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"group": name, "selected": node})
}
