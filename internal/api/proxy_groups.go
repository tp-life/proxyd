package api

// 代理域：节点分组接口。

import (
	"encoding/json"
	"net/http"

	"proxyd/internal/config"
)

// registerProxyGroupRoutes 注册节点分组路由。
func (s *Server) registerProxyGroupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/groups", s.handleListGroups)
	mux.HandleFunc("POST /api/groups", s.handleAddGroup)
	mux.HandleFunc("PUT /api/groups/{name}", s.handleUpdateGroup)
	mux.HandleFunc("DELETE /api/groups/{name}", s.handleDelGroup)
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
