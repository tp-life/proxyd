package api

// 模块控制 HTTP 适配器只负责输入约束，生命周期事务由 application 编排。
import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// registerModuleRoutes 注册可扩展的模块管理端点。
// 参数：mux 为 *http.ServeMux；返回无；重复注册会由标准库报告错误。
func (s *Server) registerModuleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/modules", s.handleModules)
	mux.HandleFunc("POST /api/modules/{module}/retry", s.handleRetryModule)
	mux.HandleFunc("POST /api/modules/{module}", s.handleSetModule)
}

// handleModules 输出不含凭据的模块状态。
// 参数：w 为响应写入器、r 为请求；返回无；无业务错误。
func (s *Server) handleModules(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.app.Modules()) }

// handleSetModule 校验显式布尔值后执行启停，未知字段与超大请求直接拒绝。
// 参数：w 为响应写入器、r 为请求；返回无；输入或事务失败返回 400。
func (s *Server) handleSetModule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || req.Enabled == nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "请提供 enabled 布尔值", 400)
		return
	}
	if err := s.app.SetModuleEnabled(r.PathValue("module"), *req.Enabled); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, s.app.Modules())
}

// handleRetryModule 触发有界重试。参数 w/r 为 HTTP 适配器；返回无；失败通过状态与 400 返回。
func (s *Server) handleRetryModule(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.app.RetryModule(ctx, r.PathValue("module")); err != nil {
		http.Error(w, "模块重试失败，请查看模块状态和诊断结果", 400)
		return
	}
	writeJSON(w, s.app.Modules())
}
