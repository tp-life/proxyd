package api

// 代理域：Tailscale 一体化接入与管理员审批状态接口。

import (
	"encoding/json"
	"net/http"
	"strings"

	proxytailscale "proxyd/internal/proxy/tailscale"
)

// registerProxyTailscaleRoutes 注册一体化创建、终止和只读注册状态路由。
//
// 参数说明：mux 是 API Start 创建的私有 ServeMux。
//
// 返回值说明：无。
//
// 错误情况：无；路由冲突会由 net/http 在开发阶段直接暴露，实际业务错误由 handler 返回。
func (s *Server) registerProxyTailscaleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/tailscale/setups", s.handleSetupTailscale)
	mux.HandleFunc("DELETE /api/tailscale/setups/{name}", s.handleTerminateTailscaleSetup)
	mux.HandleFunc("GET /api/tailscale/enrollments", s.handleListTailscaleEnrollments)
	mux.HandleFunc("GET /api/tailscale/enrollments/{name}", s.handleGetTailscaleEnrollment)
}

// handleSetupTailscale 接收单页向导配置，并委托应用层以事务方式创建节点、分组、
// 固定代理入口和可选 TUN 规则。
//
// 参数说明：w 写出创建结果；r.Body 是 tailscale.Setup JSON。
//
// 返回值说明：成功返回 HTTP 201 和实际分配端口；失败返回 HTTP 400 与可操作错误。
//
// 错误情况：JSON 非法、业务字段缺失、端口/名称冲突、TUN 权限、mihomo 热更新或
// 持久化失败均不会留下半套配置。
func (s *Server) handleSetupTailscale(w http.ResponseWriter, r *http.Request) {
	var request proxytailscale.Setup
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	result, err := s.app.SetupTailscale(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, result)
}

// handleTerminateTailscaleSetup 终止一条失败、启动中或等待审批的 Tailscale 接入，
// 并由应用层以事务方式删除其节点、引用组和受管路由。
//
// 参数说明：w 写出终止结果或错误；r.PathValue("name") 是 URL 解码后的出站名称，
// r.Context 控制事务开始前的请求取消。
//
// 返回值说明：成功返回 HTTP 200 与实际清理摘要，便于管理面确认已删除的聚合资源。
//
// 错误情况：名称为空返回 400；节点不存在返回 404；目标类型错误、运行态热更新、
// 落盘或快照失败返回 409，且应用层已经尝试整体回滚。
func (s *Server) handleTerminateTailscaleSetup(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "Tailscale 节点名称不能为空", http.StatusBadRequest)
		return
	}
	result, err := s.app.TerminateTailscaleSetup(r.Context(), name)
	if err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "不存在") {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, result)
}

// handleListTailscaleEnrollments 返回当前进程观察到的全部管理员审批/Auth Key 注册状态。
//
// 参数说明：w 写出 JSON；请求参数未使用。
//
// 返回值说明：成功返回 HTTP 200 和按节点名排序的状态数组。
//
// 错误情况：无；进程重启且尚未触发注册时返回空数组，注册链接不会从磁盘恢复。
func (s *Server) handleListTailscaleEnrollments(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.TailscaleEnrollments())
}

// handleGetTailscaleEnrollment 返回一个 Tailscale 接入节点的内存注册状态。
//
// 参数说明：w 写出 JSON 或 404；r.PathValue("name") 是 URL 解码后的节点名。
//
// 返回值说明：存在时返回 HTTP 200；未知节点返回 HTTP 404。
//
// 错误情况：空名称和不存在状态均视为未找到，避免返回其它会话的一次性注册链接。
func (s *Server) handleGetTailscaleEnrollment(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	status, ok := s.app.TailscaleEnrollment(name)
	if !ok {
		http.Error(w, "Tailscale 注册状态不存在", http.StatusNotFound)
		return
	}
	writeJSON(w, status)
}
