package api

// 代理域：订阅管理（增删改/单订阅刷新与测速）及全局刷新、测速接口。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"proxyd/internal/app"
	"proxyd/internal/config"
	"proxyd/internal/proxy/subscribe"
)

// registerProxySubscriptionRoutes 注册订阅管理、刷新草稿与全局刷新/测速路由。
//
// 参数：mux 为 API 主路由器，所有订阅路由只在此领域文件集中登记。
// 返回值：无。
// 错误情况：无；重复 pattern 会由 net/http 在启动阶段 panic，属于开发期路由冲突。
func (s *Server) registerProxySubscriptionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/subscriptions", s.handleAddSub)
	mux.HandleFunc("PUT /api/subscriptions/{name}", s.handleUpdateSub)
	mux.HandleFunc("DELETE /api/subscriptions/{name}", s.handleDelSub)
	mux.HandleFunc("POST /api/subscriptions/{name}/refresh", s.handleRefreshSub)
	mux.HandleFunc("POST /api/subscriptions/{name}/test", s.handleTestSub)
	mux.HandleFunc("GET /api/subscriptions/{name}/draft", s.handleGetSubscriptionDraft)
	mux.HandleFunc("POST /api/subscriptions/{name}/draft", s.handleCreateSubscriptionDraft)
	mux.HandleFunc("POST /api/subscriptions/{name}/draft/{id}/apply", s.handleApplySubscriptionDraft)
	mux.HandleFunc("DELETE /api/subscriptions/{name}/draft/{id}", s.handleDiscardSubscriptionDraft)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/test", s.handleTest)
}

// SubEntry 是订阅聚合信息。
type SubEntry struct {
	Name        string              `json:"name"`
	URL         string              `json:"url"`
	Type        string              `json:"type"`
	Enabled     bool                `json:"enabled"`
	PortMapping bool                `json:"port_mapping"`
	State       string              `json:"state"` // disabled|empty|error|degraded|healthy
	Total       int                 `json:"total"`
	Alive       int                 `json:"alive"`
	UserInfo    *subscribe.UserInfo `json:"userinfo,omitempty"`
}

// handleAddSub 保存一条订阅配置，但不隐式拉取远端内容。
//
// 参数：w 写出新订阅或请求错误；r 的 JSON 包含名称、URL、类型和启用/端口映射开关。
//
// 返回值：无；成功返回 HTTP 201 和规范化后的订阅。
//
// 错误情况：JSON、URL、类型、名称唯一性或持久化失败时返回 400。即使订阅启用，
// 也要等用户点击“同步”后才下载节点，避免新增动作变成不可见的自动同步。
func (s *Server) handleAddSub(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		Type        string `json:"type"`
		Enabled     *bool  `json:"enabled"`
		PortMapping *bool  `json:"port_mapping"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		http.Error(w, "bad request: url required", http.StatusBadRequest)
		return
	}
	sub, err := s.app.AddSubscriptionEntry(config.Subscription{
		Name: req.Name, URL: req.URL, Type: req.Type, Enabled: req.Enabled, PortMapping: req.PortMapping,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, sub)
}

// handleUpdateSub 同步编辑订阅名称、URL、类型、启用状态和订阅级端口映射开关。
//
// 参数：
//   - w: http.ResponseWriter，写出提交后的订阅或事务错误。
//   - r: *http.Request，路径 `{name}` 是当前名称，请求体包含目标 name/url/type/enabled。
//
// 返回值：无；成功返回 HTTP 200 与完整订阅值。
//
// 错误情况：JSON/字段非法、订阅不存在、热更新、持久化或回滚失败时返回 400；
// 编辑动作不会拉取远端，URL/类型变化后的节点要由用户再次手动同步。
func (s *Server) handleUpdateSub(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		Type        string `json:"type"`
		Enabled     *bool  `json:"enabled"`
		PortMapping *bool  `json:"port_mapping"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	updated, err := s.app.UpdateSubscription(ctx, r.PathValue("name"), config.Subscription{
		Name: req.Name, URL: req.URL, Type: req.Type, Enabled: req.Enabled, PortMapping: req.PortMapping,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, updated)
}

// handleDelSub 删除订阅，并在后台仅用剩余缓存节点重建运行态。
//
// 参数：w 写出空响应或错误；r 的路径 name 指定待删除订阅。
//
// 返回值：无；成功返回 HTTP 204。
//
// 错误情况：订阅不存在或删除最后来源时返回 404；后台重建失败写日志，不会借机
// 拉取其它订阅，也不会改变删除请求已经提交的 HTTP 结果。
func (s *Server) handleDelSub(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RemoveSubscription(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.trigger(false)
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

// handleGetSubscriptionDraft 返回指定订阅当前仍有效的待确认草稿。
//
// 参数：w 写出草稿 JSON 或错误；r 的路径 name 指定订阅。
// 返回值：无；成功为 HTTP 200。
// 错误情况：草稿不存在或过期返回 404，不会返回内部节点 Mapping 或订阅正文。
func (s *Server) handleGetSubscriptionDraft(w http.ResponseWriter, r *http.Request) {
	draft, err := s.app.SubscriptionDraft(r.PathValue("name"))
	if err != nil {
		writeSubscriptionDraftError(w, err)
		return
	}
	writeJSON(w, draft)
}

// handleCreateSubscriptionDraft 拉取、解析并健康检测订阅，但不修改运行态或缓存。
//
// 参数：w 写出安全差异视图；r 的路径 name 指定订阅，请求上下文支持客户端取消。
// 返回值：无；成功为 HTTP 201，响应包含 30 分钟有效的随机草稿 ID。
// 错误情况：订阅无效、拉取/解析/检测前置条件失败返回 400；处理最长 3 分钟。
func (s *Server) handleCreateSubscriptionDraft(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	draft, err := s.app.CreateSubscriptionDraft(ctx, r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, draft)
}

// handleApplySubscriptionDraft 明确确认并应用指定订阅草稿。
//
// 参数：w 写出提交状态；r 的路径 name/id 唯一标识待确认草稿。
// 返回值：无；成功为 HTTP 200。
// 错误情况：不存在/过期为 404，预览后的运行基线变化为 409，热更新失败为 400。
func (s *Server) handleApplySubscriptionDraft(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := s.app.ApplySubscriptionDraft(ctx, r.PathValue("name"), r.PathValue("id")); err != nil {
		writeSubscriptionDraftError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "applied"})
}

// handleDiscardSubscriptionDraft 按随机 ID 丢弃一份未确认草稿。
//
// 参数：w 写出空响应或错误；r 的路径 name/id 唯一标识草稿。
// 返回值：无；成功为 HTTP 204。
// 错误情况：草稿不存在或已被新预览替换时返回 404，且不会误删新草稿。
func (s *Server) handleDiscardSubscriptionDraft(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DiscardSubscriptionDraft(r.PathValue("name"), r.PathValue("id")); err != nil {
		writeSubscriptionDraftError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeSubscriptionDraftError 把应用层草稿错误映射为稳定 HTTP 状态。
//
// 参数：w 为响应写入器；err 为应用层返回的带语义错误。
// 返回值：无。
// 错误情况：not found/expired 映射 404，stale 映射 409，其余业务或热更新错误映射 400。
func writeSubscriptionDraftError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, app.ErrSubscriptionDraftNotFound), errors.Is(err, app.ErrSubscriptionDraftExpired):
		status = http.StatusNotFound
	case errors.Is(err, app.ErrSubscriptionDraftStale):
		status = http.StatusConflict
	}
	http.Error(w, err.Error(), status)
}
