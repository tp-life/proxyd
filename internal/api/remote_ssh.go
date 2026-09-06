package api

// 本文件提供 SSH 公钥管理的 HTTP 入口；私钥不属于此 API 的输入或输出模型。

import (
	"encoding/json"
	"io"
	"net/http"
)

// registerRemoteSSHRoutes 注册 remote 上下文的 SSH 公钥管理端点。
// 参数说明：mux 为 *http.ServeMux，应用已有管理 API 认证中间件的路由器。
// 返回值说明：无。
// 错误情况：重复路由按 ServeMux 约定触发 panic，正常启动仅注册一次。
func (s *Server) registerRemoteSSHRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/remote/ssh-keys", s.handleGetRemoteSSHKeys)
	mux.HandleFunc("POST /api/remote/ssh-keys", s.handleAddRemoteSSHKeys)
	mux.HandleFunc("DELETE /api/remote/ssh-keys", s.handleDeleteRemoteSSHKey)
	mux.HandleFunc("POST /api/remote/ssh-auth", s.handleSetRemoteSSHAuth)
	mux.HandleFunc("PATCH /api/remote/ssh-keys", s.handleUpdateRemoteSSHKey)
	mux.HandleFunc("POST /api/remote/ssh-keys/disconnect", s.handleDisconnectRemoteSSHKey)
}

// decodeRemoteSSHRequest 有界解析 SSH 管理请求，并拒绝尾随或未知 JSON 字段。
// 参数说明：w 为 http.ResponseWriter；r 为 *http.Request；target 为 any，目标结构指针。
// 返回值说明：bool，解析成功时为 true，失败时已写入 400 响应。
// 错误情况：超过 128 KiB、JSON 无效或含多份 JSON 时拒绝；错误不回显可能误传的私钥。
func decodeRemoteSSHRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		http.Error(w, "SSH 管理请求无效或超过大小限制", http.StatusBadRequest)
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "请求只能包含一个 JSON 对象", http.StatusBadRequest)
		return false
	}
	return true
}

// handleGetRemoteSSHKeys 返回附加认证开关和可公开的公钥列表。
// 参数说明：w 为 http.ResponseWriter；r 为 *http.Request，不读取请求参数。
// 返回值说明：无，输出 {required, keys} JSON。
// 错误情况：无业务错误；仅输出公钥和指纹，不包含隧道 token 或私钥。
func (s *Server) handleGetRemoteSSHKeys(w http.ResponseWriter, _ *http.Request) {
	status := s.app.RemoteStatus()
	writeJSON(w, map[string]any{"required": status.SSHAuthRequired, "keys": status.SSHKeys})
}

// handleAddRemoteSSHKeys 接收公钥文本或公钥文件内容并原子导入。
// 参数说明：w 为 http.ResponseWriter；r 为 *http.Request，JSON 字段为 public_key、可选 name。
// 返回值说明：无，成功返回最新 remote 状态。
// 错误情况：解析、校验或配置事务失败时返回 400，不保存部分公钥。
func (s *Server) handleAddRemoteSSHKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if !decodeRemoteSSHRequest(w, r, &req) {
		return
	}
	if err := s.app.AddRemoteSSHKeys(req.PublicKey, req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetRemote(w, r)
}

// handleDeleteRemoteSSHKey 按请求体中的指纹或唯一名称撤销授权。
// 参数说明：w 为 http.ResponseWriter；r 为 *http.Request，JSON 字段为 identifier。
// 返回值说明：无，成功返回最新 remote 状态。
// 错误情况：未找到、歧义或事务失败时返回 400；认证开关保持原值。
func (s *Server) handleDeleteRemoteSSHKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Identifier string `json:"identifier"`
	}
	if !decodeRemoteSSHRequest(w, r, &req) {
		return
	}
	if err := s.app.DeleteRemoteSSHKey(req.Identifier); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetRemote(w, r)
}

// handleSetRemoteSSHAuth 修改附加公钥认证开关，缺少字段时拒绝而非默认为关闭。
// 参数说明：w 为 http.ResponseWriter；r 为 *http.Request，JSON 必须包含 required 布尔值。
// 返回值说明：无，成功返回最新 remote 状态。
// 错误情况：参数非法或事务失败时返回 400，不改变原认证方式。
func (s *Server) handleSetRemoteSSHAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Required *bool `json:"required"`
	}
	if !decodeRemoteSSHRequest(w, r, &req) {
		return
	}
	if req.Required == nil {
		http.Error(w, "required 必须是布尔值", http.StatusBadRequest)
		return
	}
	if err := s.app.SetRemoteSSHAuth(*req.Required); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetRemote(w, r)
}

// handleUpdateRemoteSSHKey 更新公钥生命周期，不要求客户端整体覆盖列表。
// 参数说明：w 为 http.ResponseWriter；r 为 *http.Request，包含 identifier 及可选 disabled、expires_at。
// 返回值说明：无，成功返回完整状态。
// 错误情况：参数或事务失败返回 400，落盘失败恢复旧策略。
func (s *Server) handleUpdateRemoteSSHKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Identifier string  `json:"identifier"`
		Disabled   *bool   `json:"disabled"`
		ExpiresAt  *string `json:"expires_at"`
	}
	if !decodeRemoteSSHRequest(w, r, &req) {
		return
	}
	if err := s.app.UpdateRemoteSSHKey(req.Identifier, req.Disabled, req.ExpiresAt); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.handleGetRemote(w, r)
}

// handleDisconnectRemoteSSHKey 显式结束指定公钥已有会话，不改变授权配置。
// 参数说明：w 为 http.ResponseWriter；r 为 *http.Request，包含 fingerprint。
// 返回值说明：无，输出 disconnected 计数。
// 错误情况：格式不正确时返回 400；网络连接已关闭不视为操作失败。
func (s *Server) handleDisconnectRemoteSSHKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fingerprint string `json:"fingerprint"`
	}
	if !decodeRemoteSSHRequest(w, r, &req) {
		return
	}
	count, err := s.app.DisconnectRemoteSSHKey(req.Fingerprint)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, map[string]int{"disconnected": count})
}
