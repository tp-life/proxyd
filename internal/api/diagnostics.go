package api

// 诊断 HTTP 端点只接受已保存设备名称，返回可直接脱敏导出的统一报告。
import (
	"encoding/json"
	"io"
	"net/http"
)

// registerDiagnosticRoutes 登记诊断用例；参数 mux 为路由器；无返回，重复注册会 panic。
func (s *Server) registerDiagnosticRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/diagnostics", s.handleDiagnostics)
}

// handleDiagnostics 执行有界检查；参数 w/r 为 HTTP 对象；无返回，非法输入或忙状态返回 400。
func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Peer string `json:"peer"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF {
		http.Error(w, "请提供诊断对象", 400)
		return
	}
	result, err := s.app.Diagnose(r.Context(), req.Peer)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, result)
}
