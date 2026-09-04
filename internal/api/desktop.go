package api

// 本文件提供「远程桌面」页面的 HTTP 合约。handler 只负责 JSON/路径转换和状态码，
// 配置事务、跨 remote 调和及会话生命周期全部委托给应用层。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"proxyd/internal/app"
	"proxyd/internal/config"
	"proxyd/internal/desktop"
)

// desktopSessionResponse 是领域会话到 Web 合约的显式映射。
//
// LaunchKind 为 download 时浏览器下载 .rdp 文件；为 uri 时打开系统 vnc:// 处理器。
type desktopSessionResponse struct {
	ID                string `json:"id"`
	ConnectionName    string `json:"connection_name"`
	Protocol          string `json:"protocol"`
	RemotePort        int    `json:"remote_port"`
	Username          string `json:"username,omitempty"`
	LocalAddress      string `json:"local_address"`
	StartedAt         string `json:"started_at"`
	ActiveConnections int64  `json:"active_connections"`
	LaunchKind        string `json:"launch_kind"`
	LaunchURL         string `json:"launch_url"`
}

// desktopStatusResponse 是 GET /api/desktop 的稳定响应结构。
type desktopStatusResponse struct {
	Services      []app.DesktopServiceStatus `json:"services"`
	Connections   []config.DesktopConnection `json:"connections"`
	Sessions      []desktopSessionResponse   `json:"sessions"`
	RemoteEnabled bool                       `json:"remote_enabled"`
	APILoopback   bool                       `json:"api_loopback"`
}

// registerDesktopRoutes 注册「远程桌面」模块的全部路由。
//
// 参数说明：mux 是 API Server 在 Start 中创建的专用 ServeMux。
//
// 返回值说明：无；路由同步登记，真正的网络与配置操作延迟到请求阶段。
//
// 错误情况：重复注册相同 method/path 会由 net/http panic，调用清单必须保持唯一。
func (s *Server) registerDesktopRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/desktop", s.handleGetDesktop)
	mux.HandleFunc("POST /api/desktop/services/{protocol}", s.handleSetDesktopService)
	mux.HandleFunc("POST /api/desktop/connections", s.handleAddDesktopConnection)
	mux.HandleFunc("PUT /api/desktop/connections/{name}", s.handleUpdateDesktopConnection)
	mux.HandleFunc("DELETE /api/desktop/connections/{name}", s.handleDeleteDesktopConnection)
	mux.HandleFunc("POST /api/desktop/sessions", s.handleStartDesktopSession)
	mux.HandleFunc("DELETE /api/desktop/sessions/{id}", s.handleStopDesktopSession)
	mux.HandleFunc("GET /api/desktop/sessions/{id}/rdp", s.handleDownloadDesktopRDP)
}

// handleGetDesktop 返回本机桌面服务、保存档案与当前会话状态。
//
// 参数说明：w 写出 JSON；r 提供取消信号给本机端口探测。
//
// 返回值说明：无；成功返回 200 JSON。
//
// 错误情况：本机服务未监听只在对应项返回 listening=false，不产生 HTTP 错误。
func (s *Server) handleGetDesktop(w http.ResponseWriter, r *http.Request) {
	s.writeDesktopStatus(w, s.app.DesktopStatus(r.Context()))
}

// handleSetDesktopService 更新协议服务端口及 tailcat 开放状态。
//
// 参数说明：路径 protocol 为 rdp/vnc；请求体为 {port, exposed}。
//
// 返回值说明：无；成功返回最新桌面状态。
//
// 错误情况：JSON、协议、端口、remote 调和或配置持久化失败时返回 400，应用层保证回滚。
func (s *Server) handleSetDesktopService(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Port    int  `json:"port"`
		Exposed bool `json:"exposed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetDesktopService(r.PathValue("protocol"), request.Port, request.Exposed); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.writeDesktopStatus(w, s.app.DesktopStatus(r.Context()))
}

// handleAddDesktopConnection 新增一条桌面连接档案。
//
// 参数说明：请求体是完整 DesktopConnection；w 返回最新状态。
//
// 返回值说明：无；保存成功返回 200 JSON。
//
// 错误情况：JSON 非法、档案重名、字段校验或落盘失败时返回 400。
func (s *Server) handleAddDesktopConnection(w http.ResponseWriter, r *http.Request) {
	var connection config.DesktopConnection
	if err := json.NewDecoder(r.Body).Decode(&connection); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.AddDesktopConnection(connection); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.writeDesktopStatus(w, s.app.DesktopStatus(r.Context()))
}

// handleUpdateDesktopConnection 更新或重命名桌面连接档案。
//
// 参数说明：路径 name 是原名称；请求体是完整目标状态。
//
// 返回值说明：无；成功返回最新状态。
//
// 错误情况：JSON 非法、原档案不存在、新名称冲突、字段非法或落盘失败时返回 400。
func (s *Server) handleUpdateDesktopConnection(w http.ResponseWriter, r *http.Request) {
	var connection config.DesktopConnection
	if err := json.NewDecoder(r.Body).Decode(&connection); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.UpdateDesktopConnection(r.PathValue("name"), connection); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.writeDesktopStatus(w, s.app.DesktopStatus(r.Context()))
}

// handleDeleteDesktopConnection 删除保存的桌面连接档案。
//
// 参数说明：路径 name 是待删除名称；请求体不使用。
//
// 返回值说明：无；成功返回最新状态。
//
// 错误情况：档案不存在或配置落盘失败时返回 400；活动会话按应用层规则继续存在。
func (s *Server) handleDeleteDesktopConnection(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DeleteDesktopConnection(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.writeDesktopStatus(w, s.app.DesktopStatus(r.Context()))
}

// handleStartDesktopSession 创建或复用一条连接档案的临时转发。
//
// 参数说明：请求体为 {connection: 档案名称}。
//
// 返回值说明：无；成功返回包含 launch_kind 与 launch_url 的会话 JSON。
//
// 错误情况：JSON、档案、远端、token、权限或本地监听失败时返回 400，不泄露完整 token。
func (s *Server) handleStartDesktopSession(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Connection string `json:"connection"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	session, err := s.app.StartDesktopSession(request.Connection)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, desktopSessionToResponse(session))
}

// handleStopDesktopSession 显式结束会话并返回 204。
//
// 参数说明：路径 id 是会话标识。
//
// 返回值说明：无；成功不返回正文。
//
// 错误情况：会话不存在返回 404，底层关闭失败返回 500。
func (s *Server) handleStopDesktopSession(w http.ResponseWriter, r *http.Request) {
	err := s.app.StopDesktopSession(r.PathValue("id"))
	if errors.Is(err, desktop.ErrSessionNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDownloadDesktopRDP 生成不含密码的临时 .rdp 配置文件。
//
// 参数说明：路径 id 指向当前 RDP 会话；w 返回 attachment 文件。
//
// 返回值说明：无；文件中的 full address 指向本机临时回环转发，用户名为可选字段。
//
// 错误情况：会话不存在返回 404，协议不是 RDP 返回 400。用户名已经过配置层换行校验，
// 这里仍移除 CR/LF 做纵深防御，避免构造额外 RDP 指令。
func (s *Server) handleDownloadDesktopRDP(w http.ResponseWriter, r *http.Request) {
	session, err := s.app.DesktopSession(r.PathValue("id"))
	if errors.Is(err, desktop.ErrSessionNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if session.Protocol != desktop.ProtocolRDP {
		http.Error(w, "该桌面会话不是 RDP 协议", http.StatusBadRequest)
		return
	}
	username := strings.NewReplacer("\r", "", "\n", "").Replace(session.Username)
	content := fmt.Sprintf("full address:s:%s\r\nprompt for credentials:i:1\r\n", session.LocalAddress)
	if username != "" {
		content += fmt.Sprintf("username:s:%s\r\n", username)
	}
	w.Header().Set("Content-Type", "application/x-rdp")
	w.Header().Set("Content-Disposition", `attachment; filename="proxyd-desktop.rdp"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(content))
}

// writeDesktopStatus 把应用层快照转换成 API DTO 并输出 JSON。
//
// 参数说明：w 是响应写入器；status 是应用层组合快照。
//
// 返回值说明：无；所有领域会话均映射为 snake_case 字段和平台启动信息。
//
// 错误情况：JSON 编码错误由共享 writeJSON 处理；转换本身不失败。
func (s *Server) writeDesktopStatus(w http.ResponseWriter, status app.DesktopStatus) {
	sessions := make([]desktopSessionResponse, 0, len(status.Sessions))
	for _, session := range status.Sessions {
		sessions = append(sessions, desktopSessionToResponse(session))
	}
	// 空连接必须编码为 [] 而不是 null。Web 虽能防御 null，但稳定的集合语义能让外部
	// API 客户端直接遍历，也避免删除最后一条档案后响应类型发生变化。
	connections := append(make([]config.DesktopConnection, 0, len(status.Connections)), status.Connections...)
	writeJSON(w, desktopStatusResponse{
		Services:      status.Services,
		Connections:   connections,
		Sessions:      sessions,
		RemoteEnabled: status.RemoteEnabled,
		APILoopback:   status.APILoopback,
	})
}

// desktopSessionToResponse 显式映射领域会话字段并生成浏览器启动目标。
//
// 参数说明：session 是不含 HTTP/JSON 依赖的领域快照。
//
// 返回值说明：desktopSessionResponse；RDP 使用同源下载端点，VNC 使用系统 URI handler。
//
// 错误情况：无；协议只可能来自已经校验的 SessionSpec，未知值仍保守返回空启动地址。
func desktopSessionToResponse(session desktop.Session) desktopSessionResponse {
	response := desktopSessionResponse{
		ID:                session.ID,
		ConnectionName:    session.ConnectionName,
		Protocol:          string(session.Protocol),
		RemotePort:        session.RemotePort,
		Username:          session.Username,
		LocalAddress:      session.LocalAddress,
		StartedAt:         session.StartedAt.Format(timeRFC3339Nano),
		ActiveConnections: session.ActiveConnections,
	}
	switch session.Protocol {
	case desktop.ProtocolRDP:
		response.LaunchKind = "download"
		response.LaunchURL = "/api/desktop/sessions/" + session.ID + "/rdp"
	case desktop.ProtocolVNC:
		response.LaunchKind = "uri"
		response.LaunchURL = "vnc://" + session.LocalAddress
	}
	return response
}

// timeRFC3339Nano 固定 API 时间精度，避免直接依赖 time 包常量散落在映射逻辑中。
const timeRFC3339Nano = "2006-01-02T15:04:05.999999999Z07:00"
