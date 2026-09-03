package api

// 代理域：订阅管理（增删改/单订阅刷新与测速）及全局刷新、测速接口。

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/proxy/subscribe"
)

// registerProxySubscriptionRoutes 注册订阅管理与全局刷新/测速路由。
func (s *Server) registerProxySubscriptionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/subscriptions", s.handleAddSub)
	mux.HandleFunc("PUT /api/subscriptions/{name}", s.handleUpdateSub)
	mux.HandleFunc("DELETE /api/subscriptions/{name}", s.handleDelSub)
	mux.HandleFunc("POST /api/subscriptions/{name}/refresh", s.handleRefreshSub)
	mux.HandleFunc("POST /api/subscriptions/{name}/test", s.handleTestSub)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/test", s.handleTest)
}

// SubEntry 是订阅聚合信息。
type SubEntry struct {
	Name     string              `json:"name"`
	URL      string              `json:"url"`
	Type     string              `json:"type"`
	Enabled  bool                `json:"enabled"`
	State    string              `json:"state"` // disabled|empty|error|degraded|healthy
	Total    int                 `json:"total"`
	Alive    int                 `json:"alive"`
	UserInfo *subscribe.UserInfo `json:"userinfo,omitempty"`
}

func (s *Server) handleAddSub(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Type    string `json:"type"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		http.Error(w, "bad request: url required", http.StatusBadRequest)
		return
	}
	sub, err := s.app.AddSubscriptionEntry(config.Subscription{
		Name: req.Name, URL: req.URL, Type: req.Type, Enabled: req.Enabled,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if sub.IsEnabled() {
		s.trigger(true)
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, sub)
}

// handleUpdateSub 同步编辑订阅名称、URL、类型和启用状态。
//
// 参数：
//   - w: http.ResponseWriter，写出提交后的订阅或事务错误。
//   - r: *http.Request，路径 `{name}` 是当前名称，请求体包含目标 name/url/type/enabled。
//
// 返回值：无；成功返回 HTTP 200 与完整订阅值。
//
// 错误情况：JSON/字段非法、订阅不存在、启用拉取无缓存、健康检测、热更新、持久化
// 或回滚失败时返回 400；同步执行使界面不会先乐观显示再被后台失败推翻。
func (s *Server) handleUpdateSub(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Type    string `json:"type"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	updated, err := s.app.UpdateSubscription(ctx, r.PathValue("name"), config.Subscription{
		Name: req.Name, URL: req.URL, Type: req.Type, Enabled: req.Enabled,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, updated)
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

// handleRefreshSub 只刷新单个订阅（拉取该订阅 + 检测其节点 + 热更新）。
// 与全局刷新不同，这里同步执行（最长 3 分钟）以便把拉取失败等原因直接返回给前端。
func (s *Server) handleRefreshSub(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := s.app.RefreshSubscription(ctx, r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleTestSub 只对单个订阅的现有节点做健康检测/延迟测试，同步返回结果。
func (s *Server) handleTestSub(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := s.app.TestSubscription(ctx, r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}
