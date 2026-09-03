package api

// 系统与共享接口：system-proxy、TUN、dns-preset、update-check、配置导出/导入、重启、自启、日志。

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"proxyd/internal/logbuf"
)

// registerSystemRoutes 注册系统与共享路由（含健康检查）。
func (s *Server) registerSystemRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/system-proxy", s.handleSetSystemProxy)
	mux.HandleFunc("GET /api/tun", s.handleTUNStatus)
	mux.HandleFunc("POST /api/tun", s.handleSetTUN)
	mux.HandleFunc("POST /api/dns-preset", s.handleSetDNSPreset)
	mux.HandleFunc("POST /api/update-check", s.handleSetUpdateCheck)
	mux.HandleFunc("GET /api/config/export", s.handleExportConfig)
	mux.HandleFunc("POST /api/config/import/preview", s.handlePreviewImportConfig)
	mux.HandleFunc("POST /api/config/import", s.handleImportConfig)
	mux.HandleFunc("POST /api/restart", s.handleRestart)
	mux.HandleFunc("POST /api/autostart", s.handleSetAutostart)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}

// LogsResponse 是 /api/logs 的响应。
type LogsResponse struct {
	Entries []logbuf.Entry `json:"entries"`
}

// maxImportedConfigBytes 限制配置导入请求体为 1 MiB。
// 配置文件通常只有数 KiB；上限用于避免本地 API 被误传大文件后占用过多内存。
const maxImportedConfigBytes int64 = 1 << 20

// handleLogs 返回进程内日志环形缓冲尾部。
//
// 参数：
//   - w: http.ResponseWriter，用于写 JSON 响应。
//   - r: *http.Request，读取 `tail` 和 `level` 查询参数。
//
// 返回值：无；响应体形如 `{"entries":[...]}`。
//
// 错误情况：
//   - tail 非整数时按默认 200 处理；level 未知时会自然过滤为空，不报错。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	tail := 200
	if raw := r.URL.Query().Get("tail"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			tail = n
		}
	}
	writeJSON(w, LogsResponse{Entries: logbuf.Default.Tail(tail, r.URL.Query().Get("level"))})
}

// handleSetSystemProxy 开关系统代理（指向主端口）。
func (s *Server) handleSetSystemProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetSystemProxy(req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"system_proxy": req.Enabled})
}

// handleTUNStatus 返回 TUN 开关和当前进程权限状态。
//
// 参数：
//   - w: http.ResponseWriter，写出 JSON 响应。
//   - r: *http.Request，本接口不读取请求体，仅保留统一 handler 签名。
//
// 返回值：无；状态通过 HTTP 200 JSON 写出。
//
// 错误情况：无；权限探测失败会以 allowed=false 和 permission 指引表达，
// 不把可诊断的环境状态转换成 HTTP 500。
func (s *Server) handleTUNStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.TUNStatus())
}

// handleSetTUN 开关 TUN 模式，应用层负责权限检查、热更新、失败回滚与持久化。
//
// 参数：
//   - w: http.ResponseWriter，写出新的 TUN 状态或错误。
//   - r: *http.Request，请求体必须是 `{ "enabled": boolean }`。
//
// 返回值：无；成功返回 HTTP 200 和 TUNStatus。
//
// 错误情况：JSON 无效返回 400；权限不足或 mihomo 热更新失败也返回 400，
// 响应正文保留平台修复指引，供 Web toast 与 CLI 原样展示。
func (s *Server) handleSetTUN(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetTUN(req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.app.TUNStatus())
}

// handleSetDNSPreset 切换 DNS 预设并返回当前有效的预设标识。
//
// 参数：
//   - w: http.ResponseWriter，写出 JSON 响应或 400 错误。
//   - r: *http.Request，请求体必须是 `{ "preset": "off|fake-ip|redir-host" }`。
//
// 返回值：无；成功返回 HTTP 200 与 dns_preset 字段。
//
// 错误情况：JSON 无效、预设枚举无效或 mihomo 热更新失败时返回 400；
// 手写 dns 段存在不是错误，它会继续按更高优先级生效。
func (s *Server) handleSetDNSPreset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Preset string `json:"preset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetDNSPreset(req.Preset); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"dns_preset": s.app.Config().DNSPreset})
}

// handleSetUpdateCheck 持久化版本检查开关，并在启用时立即触发一次后台检查。
//
// 参数：
//   - w: http.ResponseWriter，写出 JSON 响应或 400 错误。
//   - r: *http.Request，请求体必须是 `{ "enabled": true|false }`。
//
// 返回值：无；成功返回当前 VersionCheckStatus。
//
// 错误情况：JSON 无效或配置文件持久化失败时返回 400；GitHub 网络失败异步降级为
// failed 状态，不把设置请求变成长连接，也不影响代理服务。
func (s *Server) handleSetUpdateCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetUpdateCheck(req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.app.VersionStatus())
}

// handleExportConfig 下载当前配置；默认打码，可显式请求完整备份。
//
// 参数：
//   - w: http.ResponseWriter，写出 YAML 附件。
//   - r: *http.Request，可用 `mask_tokens=false` 导出包含真实凭据的完整配置。
//
// 返回值：无；成功返回 HTTP 200、application/yaml 和附件文件名。
//
// 错误情况：YAML 序列化失败返回 500。完整备份包含敏感信息，接口只在 proxyd
// 本地管理 API 上提供，不写入日志或中间文件。
func (s *Server) handleExportConfig(w http.ResponseWriter, r *http.Request) {
	maskTokens := !strings.EqualFold(r.URL.Query().Get("mask_tokens"), "false")
	body, err := s.app.ExportConfig(maskTokens)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	filename := "proxyd-config.masked.yaml"
	if !maskTokens {
		filename = "proxyd-config.backup.yaml"
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}

// handleImportConfig 校验并原子写入上传的 YAML 配置，返回重启要求。
//
// 参数：
//   - w: http.ResponseWriter，写出导入结果。
//   - r: *http.Request，请求体为原始 YAML，最大 1 MiB。
//
// 返回值：无；成功返回 `{ "restart_required": true }`。
//
// 错误情况：非 YAML Content-Type 返回 415；请求体过大/读取失败、YAML 或配置校验失败、
// 实例没有配置路径、写盘失败均返回 400；失败前不会替换现有配置文件。要求非浏览器
// 简单请求类型还能阻止跨站页面绕过 CORS，用 text/plain 静默改写本机配置。
func (s *Server) handleImportConfig(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/yaml" && mediaType != "application/x-yaml" && mediaType != "text/yaml") {
		http.Error(w, "Content-Type 必须是 application/yaml", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportedConfigBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("读取导入配置失败: %v", err), http.StatusBadRequest)
		return
	}
	if err := s.app.ImportConfigConfirmed(body, r.Header.Get("X-Proxyd-Config-Digest")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{
		"restart_required": true,
		"message":          "配置已导入，请重启 proxyd 后生效",
	})
}

// handlePreviewImportConfig 对上传 YAML 执行无写入预检，并返回确认摘要与影响范围。
//
// 参数：
//   - w: http.ResponseWriter，写出 ConfigImportPreview 或校验错误。
//   - r: *http.Request，请求体为原始 YAML，Content-Type 与大小限制和正式导入一致。
//
// 返回值：无；成功返回 HTTP 200，且不会修改内存配置、运行态或磁盘文件。
//
// 错误情况：非 YAML 返回 415；过大、读取、解析或配置校验失败返回 400。前端必须把
// 返回 digest 放入正式导入的 X-Proxyd-Config-Digest 请求头，才能完成确认提交。
func (s *Server) handlePreviewImportConfig(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/yaml" && mediaType != "application/x-yaml" && mediaType != "text/yaml") {
		http.Error(w, "Content-Type 必须是 application/yaml", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportedConfigBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("读取导入配置失败: %v", err), http.StatusBadRequest)
		return
	}
	preview, err := s.app.PreviewImport(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, preview)
}

// handleRestart 触发进程级重启：先返回响应，再异步派生 restart 子进程完成 stop→start。
//
// 参数：
//   - w: http.ResponseWriter，写出重启受理结果。
//   - r: *http.Request，无请求体。
//
// 返回值：无；成功返回 `{ "restarting": true }`，已触发过的重复请求附带 `"already": true`。
//
// 错误情况：未注入 restarter（例如测试实例）返回 503。restarter 的错误发生在响应之后，
// 无法回传给客户端，只记入日志；调用方应通过轮询 /healthz 判断重启是否成功。
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if s.restartFn == nil {
		http.Error(w, "当前运行方式不支持 API 重启", http.StatusServiceUnavailable)
		return
	}
	if !s.restartInFlight.CompareAndSwap(false, true) {
		writeJSON(w, map[string]any{"restarting": true, "already": true})
		return
	}
	writeJSON(w, map[string]any{
		"restarting": true,
		"message":    "proxyd 正在重启，请稍候",
	})
	// 延迟触发，确保上面的响应完整送达客户端后进程才开始退出。
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := s.restartFn(); err != nil {
			log.Printf("[api] 重启失败: %v", err)
		}
	}()
}

// handleSetAutostart 注册/移除开机自启项。
func (s *Server) handleSetAutostart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetAutostart(req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"autostart": req.Enabled})
}
