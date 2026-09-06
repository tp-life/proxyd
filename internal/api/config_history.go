package api

// 配置历史适配器只公开元数据和脱敏导出，恢复必须携带预检时的双重摘要。
import (
	"encoding/json"
	"io"
	"net/http"
)

// registerConfigHistoryRoutes 登记历史用例；参数 mux 为路由器；无返回值，路由重复会 panic。
func (s *Server) registerConfigHistoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/config/history", s.handleConfigHistory)
	mux.HandleFunc("GET /api/config/history/{id}/export", s.handleConfigHistoryExport)
	mux.HandleFunc("POST /api/config/history/{id}/preview", s.handleConfigHistoryPreview)
	mux.HandleFunc("POST /api/config/history/{id}/restore", s.handleConfigHistoryRestore)
}

// handleConfigHistory 列出版本及待重启状态；参数 w/r 为 HTTP 对象；无返回，读取失败返回 500。
func (s *Server) handleConfigHistory(w http.ResponseWriter, r *http.Request) {
	versions, err := s.app.ConfigHistory()
	if err != nil {
		http.Error(w, "无法读取配置历史", 500)
		return
	}
	writeJSON(w, map[string]any{"versions": versions, "pending_restart": s.app.ConfigRestartPending()})
}

// handleConfigHistoryExport 下载脱敏 YAML；参数 w/r 为 HTTP 对象；无返回，无效版本返回 404。
func (s *Server) handleConfigHistoryExport(w http.ResponseWriter, r *http.Request) {
	data, err := s.app.ExportConfigHistory(r.PathValue("id"))
	if err != nil {
		http.Error(w, "历史版本不可用", 404)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="proxyd-history-redacted.yaml"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// handleConfigHistoryPreview 校验目标与当前配置；参数 w/r 为 HTTP 对象；无返回，失败返回 400。
func (s *Server) handleConfigHistoryPreview(w http.ResponseWriter, r *http.Request) {
	result, err := s.app.PreviewConfigRestore(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, result)
}

// handleConfigHistoryRestore 执行经过预检的恢复；参数 w/r 为 HTTP 对象；无返回，冲突返回 409。
// 请求限制为 4 KiB 且拒绝未知字段，避免拼写错误让客户端误以为额外条件已生效。
func (s *Server) handleConfigHistoryRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Digest     string `json:"digest"`
		BaseDigest string `json:"base_digest"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || req.Digest == "" || req.BaseDigest == "" || dec.Decode(new(any)) != io.EOF {
		http.Error(w, "请先预检并提供配置摘要", 400)
		return
	}
	if err := s.app.RestoreConfigVersion(r.PathValue("id"), req.Digest, req.BaseDigest); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	writeJSON(w, map[string]bool{"pending_restart": true})
}
