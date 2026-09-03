package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"proxyd/internal/config"
	"proxyd/internal/remote"
)

// 本文件承载「远程连接」周边模块（remote/tailcat 隧道）的 HTTP 端点。
// 安全约定：本机 token 与远端 token 都是连接凭据，列表/状态接口一律返回
// 打码摘要；完整本机 token 只能通过 GET /api/remote/token 显式获取。

// registerRemoteRoutes 注册「远程连接」周边模块路由。
func (s *Server) registerRemoteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/remote", s.handleGetRemote)
	mux.HandleFunc("POST /api/remote", s.handleSetRemote)
	mux.HandleFunc("GET /api/remote/token", s.handleGetRemoteToken)
	mux.HandleFunc("POST /api/remote/serve", s.handleSetRemoteServe)
	mux.HandleFunc("POST /api/remote/allow", s.handleSetRemoteAllow)
	mux.HandleFunc("POST /api/remote/keyfile", s.handleSetRemoteKeyFile)
	mux.HandleFunc("POST /api/remote/builtin-ssh", s.handleSetRemoteBuiltinSSH)
	mux.HandleFunc("GET /api/remote/tempkey", s.handleGetRemoteTempKey)
	mux.HandleFunc("POST /api/remote/tempkey/reset", s.handleResetRemoteTempKey)
	mux.HandleFunc("GET /api/remote/remotes", s.handleListRemotes)
	mux.HandleFunc("POST /api/remote/remotes", s.handleAddRemote)
	mux.HandleFunc("DELETE /api/remote/remotes/{name}", s.handleDelRemote)
	mux.HandleFunc("GET /api/remote/remotes/{name}/token", s.handleGetRemotePeerToken)
	mux.HandleFunc("POST /api/remote/forwards", s.handleAddRemoteForward)
	mux.HandleFunc("PUT /api/remote/forwards/{name}", s.handleSetRemoteForward)
	mux.HandleFunc("DELETE /api/remote/forwards/{name}", s.handleDelRemoteForward)
}

// maskToken 把 tc... token 折叠为首尾摘要（如 tcomFw…DRYQ8u），用于列表展示。
func maskToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) <= 14 {
		return "***"
	}
	return token[:6] + "…" + token[len(token)-6:]
}

// remoteStatusResponse 是 GET /api/remote 的响应：token 字段为打码摘要。
type remoteStatusResponse struct {
	remote.Status
	Token string `json:"token,omitempty"` // 覆盖内嵌 Status 的同名字段：打码摘要
}

// handleGetRemote 返回远程连接模块状态（token 打码）。
func (s *Server) handleGetRemote(w http.ResponseWriter, _ *http.Request) {
	st := s.app.RemoteStatus()
	writeJSON(w, remoteStatusResponse{Status: st, Token: maskToken(st.Token)})
}

// handleGetRemoteToken 显式返回完整本机 token（供复制分享）。
func (s *Server) handleGetRemoteToken(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"token": s.app.RemoteStatus().Token})
}

// handleSetRemote 热切换远程连接服务端开关。
func (s *Server) handleSetRemote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetRemoteEnabled(req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetRemote(w, r)
}

// handleSetRemoteServe 修改经隧道暴露的本机端口列表。
func (s *Server) handleSetRemoteServe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ports []int `json:"ports"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetRemoteServe(req.Ports); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetRemote(w, r)
}

// handleSetRemoteAllow 整体替换客户端公钥白名单（空列表恢复放行所有）。
func (s *Server) handleSetRemoteAllow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetRemoteAllow(req.Keys); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetRemote(w, r)
}

// handleSetRemoteKeyFile 修改自定义服务端密钥文件路径（空字符串恢复内置托管密钥）。
func (s *Server) handleSetRemoteKeyFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetRemoteKeyFile(req.Path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetRemote(w, r)
}

// handleSetRemoteBuiltinSSH 热切换内嵌免密 SSH 服务（隧道 22 端口进程内处理）。
func (s *Server) handleSetRemoteBuiltinSSH(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetRemoteBuiltinSSH(req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetRemote(w, r)
}

// handleGetRemoteTempKey 显式返回临时身份完整密钥对（公钥+私钥）。
// 私钥是连接凭据，与 token 一样只经此专用端点透出；未生成时返回 404。
func (s *Server) handleGetRemoteTempKey(w http.ResponseWriter, _ *http.Request) {
	pub, priv, err := s.app.RemoteTempKeyPair()
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"public": pub, "private": priv})
}

// handleResetRemoteTempKey 重置临时身份：生成新密钥对，公钥写入配置 temp-key。
// 手动添加的白名单条目不受影响；响应返回新公钥与最新状态。
func (s *Server) handleResetRemoteTempKey(w http.ResponseWriter, r *http.Request) {
	if _, err := s.app.ResetRemoteTempKey(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.handleGetRemote(w, r)
}

// remotePeerEntry 是远端列表的展示条目（token 打码）。
type remotePeerEntry struct {
	Name  string `json:"name"`
	Token string `json:"token"` // 打码摘要
}

// handleListRemotes 返回保存的远端列表（token 打码）。
func (s *Server) handleListRemotes(w http.ResponseWriter, _ *http.Request) {
	remotes := s.app.Config().Remote.Remotes
	out := make([]remotePeerEntry, 0, len(remotes))
	for _, p := range remotes {
		out = append(out, remotePeerEntry{Name: p.Name, Token: maskToken(p.Token)})
	}
	writeJSON(w, map[string]any{"remotes": out})
}

// handleAddRemote 新增保存的远端。
func (s *Server) handleAddRemote(w http.ResponseWriter, r *http.Request) {
	var peer config.RemotePeer
	if err := json.NewDecoder(r.Body).Decode(&peer); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	peer.Name = strings.TrimSpace(peer.Name)
	peer.Token = strings.TrimSpace(peer.Token)
	if err := s.app.AddRemotePeer(peer); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, remotePeerEntry{Name: peer.Name, Token: maskToken(peer.Token)})
}

// handleDelRemote 删除保存的远端。
func (s *Server) handleDelRemote(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DelRemotePeer(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGetRemotePeerToken 显式返回指定已保存远端的完整 token（供复制连接使用）。
// 与 GET /api/remote/token 同属本机控制台接口；远端名称不存在时返回 404。
func (s *Server) handleGetRemotePeerToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, p := range s.app.Config().Remote.Remotes {
		if p.Name == name {
			writeJSON(w, map[string]string{"token": p.Token})
			return
		}
	}
	http.Error(w, fmt.Sprintf("远端 %q 不存在", name), http.StatusNotFound)
}

// handleAddRemoteForward 新增本地转发。
// listen 为空串或 "auto" 时由应用层自动分配空闲回环端口（候选 10022-10121）；
// 响应总是返回实际落盘的转发对象，其中 listen 字段是已生效的具体地址，
// 前端可据此得知自动分配的端口。
func (s *Server) handleAddRemoteForward(w http.ResponseWriter, r *http.Request) {
	var f config.RemoteForward
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.Name = strings.TrimSpace(f.Name)
	f.Remote = strings.TrimSpace(f.Remote)
	created, err := s.app.AddRemoteForward(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, created)
}

// handleSetRemoteForward 启停单条转发（请求体 { "enabled": bool }）。
func (s *Server) handleSetRemoteForward(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetRemoteForwardEnabled(r.PathValue("name"), req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetRemote(w, r)
}

// handleDelRemoteForward 删除转发。
func (s *Server) handleDelRemoteForward(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DelRemoteForward(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
