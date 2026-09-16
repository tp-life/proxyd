package api

// 本文件承载「LAN 网关」旁路由模块（docs/adr/0003）的 HTTP 端点。
// 该模块无凭据字段，设备表与运行状态可直接返回，无需打码。
// macOS helper 的安装/卸载端点（POST /api/gateway/helper/*）刻意留空：
// helper 服务端与 launchd 安装链路由后续切片落地后再挂真实实现，
// 避免在管理面暴露永远返回 501 的死路由。

import (
	"encoding/json"
	"net/http"
	"strings"

	"proxyd/internal/config"
)

// registerGatewayRoutes 注册「LAN 网关」模块路由。
// 参数说明：mux 为 *http.ServeMux，应用管理 API 认证的路由器。
// 返回值说明：无。
// 错误情况：重复注册按 ServeMux 约定触发 panic，正常启动只调用一次。
func (s *Server) registerGatewayRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/gateway", s.handleGetGateway)
	mux.HandleFunc("POST /api/gateway", s.handleSetGateway)
	mux.HandleFunc("GET /api/gateway/precheck", s.handleGatewayPrecheck)
	mux.HandleFunc("POST /api/gateway/devices", s.handleAddGatewayDevice)
	mux.HandleFunc("PUT /api/gateway/devices/{name}", s.handleUpdateGatewayDevice)
	mux.HandleFunc("DELETE /api/gateway/devices/{name}", s.handleDeleteGatewayDevice)
}

// handleGetGateway 返回网关模块完整状态：生命周期相位、平台/支持性、转发与规则
// 应用状态、生效端口（redir/tproxy/dns）、设备表与执行层自述状态。
func (s *Server) handleGetGateway(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.GatewayOverview())
}

// handleSetGateway 热切换网关模块总开关。
//
// 参数说明：
//   - w: http.ResponseWriter，返回与 GET /api/gateway 同构的最新状态。
//   - r: *http.Request，请求体包含 enabled 布尔值。
//
// 返回值说明：无；成功写入 200 JSON，失败写入 400 文本错误。
//
// 错误情况：JSON 非法、平台不支持、代理模块停用中启用、事务失败时返回 400，
// 事务保持原状态；macOS helper 未安装不视为失败（degraded 体现在状态相位中）。
func (s *Server) handleSetGateway(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetGatewayEnabled(req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetGateway(w, r)
}

// handleGatewayPrecheck 返回「启用前检查」结果（平台支持性、helper/能力位状态、修复指引）。
func (s *Server) handleGatewayPrecheck(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.GatewayPrecheck())
}

// handleAddGatewayDevice 登记一台下游设备。
//
// 参数说明：
//   - w: http.ResponseWriter，成功时返回 201 与落盘的设备条目。
//   - r: *http.Request，请求体为 GatewayDevice JSON。
//
// 返回值说明：无。
//
// 错误情况：JSON 非法、重名/IP 重复/字段或策略非法、调和或落盘失败时返回 400，
// 事务回滚到原设备表。
func (s *Server) handleAddGatewayDevice(w http.ResponseWriter, r *http.Request) {
	var device config.GatewayDevice
	if err := json.NewDecoder(r.Body).Decode(&device); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	device.Name = strings.TrimSpace(device.Name)
	device.IP = strings.TrimSpace(device.IP)
	device.Policy = strings.TrimSpace(device.Policy)
	if err := s.app.AddGatewayDevice(device); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, device)
}

// handleUpdateGatewayDevice 原位更新一台已登记设备（暂不支持改名）。
// 校验或事务失败返回 400；设备不存在同样返回 400（与更新语义一致，区别于删除的 404）。
func (s *Server) handleUpdateGatewayDevice(w http.ResponseWriter, r *http.Request) {
	var device config.GatewayDevice
	if err := json.NewDecoder(r.Body).Decode(&device); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	device.Name = strings.TrimSpace(device.Name)
	device.IP = strings.TrimSpace(device.IP)
	device.Policy = strings.TrimSpace(device.Policy)
	if err := s.app.UpdateGatewayDevice(r.PathValue("name"), device); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, device)
}

// handleDeleteGatewayDevice 删除一台已登记设备；设备不存在时返回 404。
func (s *Server) handleDeleteGatewayDevice(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RemoveGatewayDevice(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
