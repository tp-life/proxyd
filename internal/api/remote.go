package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"proxyd/internal/config"
	"proxyd/internal/remote"
)

// 本文件承载「远程连接」周边模块（remote/tailcat 隧道）的 HTTP 端点。
// 安全约定：本机 token 与远端 token 都是连接凭据，列表/状态接口一律返回
// 打码摘要；完整本机 token 只能通过 GET /api/remote/token 显式获取。

// registerRemoteRoutes 注册「远程连接」周边模块路由。
// 参数说明：mux 为 *http.ServeMux，应用管理 API 认证的路由器。
// 返回值说明：无。
// 错误情况：重复注册按 ServeMux 约定触发 panic，正常启动只调用一次。
func (s *Server) registerRemoteRoutes(mux *http.ServeMux) {
	s.registerRemoteSSHRoutes(mux)
	mux.HandleFunc("GET /api/remote", s.handleGetRemote)
	mux.HandleFunc("POST /api/remote", s.handleSetRemote)
	mux.HandleFunc("GET /api/remote/token", s.handleGetRemoteToken)
	mux.HandleFunc("GET /api/remote/relay", s.handleGetRemoteRelay)
	mux.HandleFunc("POST /api/remote/relay", s.handleSetRemoteRelay)
	mux.HandleFunc("POST /api/remote/serve", s.handleSetRemoteServe)
	mux.HandleFunc("POST /api/remote/allow", s.handleSetRemoteAllow)
	mux.HandleFunc("POST /api/remote/ping", s.handlePingRemote)
	mux.HandleFunc("GET /api/remote/audit", s.handleGetRemoteAudit)
	mux.HandleFunc("POST /api/remote/keyfile", s.handleSetRemoteKeyFile)
	mux.HandleFunc("GET /api/remote/keyfile/export", s.handleExportRemoteKeyFile)
	mux.HandleFunc("POST /api/remote/keyfile/import", s.handleImportRemoteKeyFile)
	mux.HandleFunc("POST /api/remote/builtin-ssh", s.handleSetRemoteBuiltinSSH)
	mux.HandleFunc("POST /api/remote/shell-user", s.handleSetRemoteShellUser)
	mux.HandleFunc("POST /api/remote/web-terminal", s.handleSetRemoteWebTerminal)
	mux.HandleFunc("GET /api/remote/terminal", s.handleRemoteTerminal)
	mux.HandleFunc("GET /api/remote/tempkey", s.handleGetRemoteTempKey)
	mux.HandleFunc("POST /api/remote/tempkey/reset", s.handleResetRemoteTempKey)
	mux.HandleFunc("GET /api/remote/remotes", s.handleListRemotes)
	mux.HandleFunc("POST /api/remote/remotes", s.handleAddRemote)
	mux.HandleFunc("DELETE /api/remote/remotes/{name}", s.handleDelRemote)
	mux.HandleFunc("GET /api/remote/remotes/{name}/token", s.handleGetRemotePeerToken)
	mux.HandleFunc("POST /api/remote/files/list", s.handleListRemoteFiles)
	mux.HandleFunc("POST /api/remote/files/download", s.handleDownloadRemoteFile)
	mux.HandleFunc("POST /api/remote/files/upload", s.handleUploadRemoteFile)
	mux.HandleFunc("POST /api/remote/forwards", s.handleAddRemoteForward)
	mux.HandleFunc("PUT /api/remote/forwards/{name}", s.handleSetRemoteForward)
	mux.HandleFunc("DELETE /api/remote/forwards/{name}", s.handleDelRemoteForward)
}

// remoteFileRequest 是文件浏览与下载端点共享的请求值对象。
// PrivateKey/Passphrase 只用于当前请求建立 SSH，会在请求结束后随内存释放，绝不落盘。
type remoteFileRequest struct {
	Remote     string `json:"remote"`
	Path       string `json:"path"`
	Username   string `json:"username"`
	PrivateKey string `json:"private_key"`
	Passphrase string `json:"passphrase"`
}

// credentials 把 API DTO 转换为 remote 领域值对象。
// 参数说明：无，接收者中的字符串来自当前受认证管理 API 请求体。
// 返回值说明：remote.FileCredentials，字节切片只在当前请求生命周期使用。
// 错误情况：无；私钥格式与口令正确性在 SSH 建连时校验。
func (r remoteFileRequest) credentials() remote.FileCredentials {
	return remote.FileCredentials{
		Username: strings.TrimSpace(r.Username), PrivateKey: []byte(r.PrivateKey), Passphrase: []byte(r.Passphrase),
	}
}

// decodeRemoteFileRequest 读取有界 JSON 文件请求，避免凭据或路径字段无限占用内存。
// 参数说明：w 用于 MaxBytesReader 的超限处理；r 为当前 HTTP 请求。
// 返回值说明：remoteFileRequest 和 error；成功时字段已去除必要的首尾空白。
// 错误情况：请求超过 128 KiB、JSON 非法或 remote/path 缺失时返回错误。
func decodeRemoteFileRequest(w http.ResponseWriter, r *http.Request) (remoteFileRequest, error) {
	var request remoteFileRequest
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return request, fmt.Errorf("文件请求格式无效或超过 128 KiB")
	}
	request.Remote = strings.TrimSpace(request.Remote)
	request.Path = strings.TrimSpace(request.Path)
	if request.Remote == "" {
		return request, fmt.Errorf("远程设备不能为空")
	}
	if request.Path == "" {
		request.Path = "."
	}
	return request, nil
}

// handleListRemoteFiles 通过 tailcat 隧道和 SFTP 列出远端目录。
//
// 参数说明：w 输出 remote.FileListing；r 的 JSON 包含远端名称、路径和可选 SSH 凭据。
// 返回值说明：无；成功返回 200 JSON，条目中不包含 token 或私钥。
// 错误情况：请求非法、远端不存在、SSH 认证失败或目录无权限时返回 400。
func (s *Server) handleListRemoteFiles(w http.ResponseWriter, r *http.Request) {
	request, err := decodeRemoteFileRequest(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	client, err := s.app.OpenRemoteFileClient(r.Context(), request.Remote, request.credentials())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer client.Close()
	listing, err := client.List(r.Context(), request.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, listing)
}

// handleDownloadRemoteFile 把远端普通文件流式写入浏览器下载响应。
//
// 参数说明：w 接收 attachment 响应；r 的 JSON 包含远端名称、文件路径和可选 SSH 凭据。
// 返回值说明：无；成功时返回 application/octet-stream，并在已知时设置 Content-Length。
// 错误情况：请求/连接/文件打开失败在写响应头前返回 400；开始传输后的断连只能终止流，
// 不再尝试把文本错误混入二进制响应。
func (s *Server) handleDownloadRemoteFile(w http.ResponseWriter, r *http.Request) {
	request, err := decodeRemoteFileRequest(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	client, err := s.app.OpenRemoteFileClient(r.Context(), request.Remote, request.credentials())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer client.Close()
	download, err := client.OpenDownload(request.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer download.Reader.Close()
	filename := download.Name
	if filename == "" || filename == "." || filename == "/" {
		filename = path.Base(request.Path)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Cache-Control", "no-store")
	if download.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(download.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, download.Reader)
}

// remoteFileUploadLimit 是单次网页上传（含 multipart 元数据）的硬上限。
// 使用 int64 常量避免 32 位平台溢出；超限由 net/http 在继续读取前中断。
const remoteFileUploadLimit = int64(8 << 30)

// handleUploadRemoteFile 流式接收 multipart 文件并原子替换远端目标路径。
//
// 参数说明：w 返回写入字节数；r 为 multipart/form-data，请求字段 remote/path/username/
// private_key/passphrase 必须位于 file part 之前，便于不落本机临时文件地直接转发上传流。
// 返回值说明：无；成功返回 {"written":N}。
// 错误情况：请求超过 8 GiB、字段顺序/内容非法、SSH/SFTP 连接或远端写入失败时返回 400。
// 上限包含 multipart 开销，防止管理 API 被无限请求占用；失败的远端临时文件由领域层清理。
func (s *Server) handleUploadRemoteFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, remoteFileUploadLimit)
	reader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "文件上传必须使用 multipart/form-data", http.StatusBadRequest)
		return
	}
	request := remoteFileRequest{}
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			http.Error(w, "读取上传数据失败或超过 8 GiB", http.StatusBadRequest)
			return
		}
		name := part.FormName()
		if name != "file" {
			value, readErr := io.ReadAll(io.LimitReader(part, 128<<10))
			_ = part.Close()
			if readErr != nil {
				http.Error(w, "读取上传字段失败", http.StatusBadRequest)
				return
			}
			switch name {
			case "remote":
				request.Remote = strings.TrimSpace(string(value))
			case "path":
				request.Path = strings.TrimSpace(string(value))
			case "username":
				request.Username = strings.TrimSpace(string(value))
			case "private_key":
				request.PrivateKey = string(value)
			case "passphrase":
				request.Passphrase = string(value)
			}
			continue
		}
		if request.Remote == "" || request.Path == "" {
			_ = part.Close()
			http.Error(w, "remote 与 path 字段必须位于 file 之前", http.StatusBadRequest)
			return
		}
		client, openErr := s.app.OpenRemoteFileClient(r.Context(), request.Remote, request.credentials())
		if openErr != nil {
			_ = part.Close()
			http.Error(w, openErr.Error(), http.StatusBadRequest)
			return
		}
		written, uploadErr := client.Upload(request.Path, part)
		_ = part.Close()
		_ = client.Close()
		if uploadErr != nil {
			http.Error(w, uploadErr.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]int64{"written": written})
		return
	}
	http.Error(w, "缺少 file 上传字段", http.StatusBadRequest)
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
	Token       string `json:"token,omitempty"` // 覆盖内嵌 Status 的同名字段：打码摘要
	APIListen   string `json:"api_listen"`      // 仅用于 Web 终端风险提示，不属于 remote 数据面状态
	APILoopback bool   `json:"api_loopback"`    // false 时开启动作必须二次确认
}

// handleGetRemote 返回远程连接模块状态（token 打码）。
func (s *Server) handleGetRemote(w http.ResponseWriter, _ *http.Request) {
	st := s.app.RemoteStatus()
	apiListen, apiLoopback := s.app.RemoteAPIExposure()
	writeJSON(w, remoteStatusResponse{
		Status:      st,
		Token:       maskToken(st.Token),
		APIListen:   apiListen,
		APILoopback: apiLoopback,
	})
}

// handleGetRemoteToken 显式返回完整本机 token（供复制分享）。
func (s *Server) handleGetRemoteToken(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"token": s.app.RemoteStatus().Token})
}

// remoteRelayResponse 是专用中继配置端点的响应。
//
// DERP map URL 可能携带私有查询凭据，因此完整值不进入通用状态响应，只由这个受
// 管理 API 认证保护的专用 GET 端点返回。
type remoteRelayResponse struct {
	Region     string `json:"region"`
	DERPMapURL string `json:"derpmap_url"`
}

// handleGetRemoteRelay 返回可编辑的 tailcat 中继配置原值。
//
// 参数说明：
//   - w: http.ResponseWriter，用于输出 region 与完整 derpmap_url。
//   - r: *http.Request，当前未读取请求参数，由统一管理 API 中间件完成认证。
//
// 返回值说明：无；成功写入 200 JSON。
//
// 错误情况：无；配置读取是内存快照，不触发网络探测。
func (s *Server) handleGetRemoteRelay(w http.ResponseWriter, _ *http.Request) {
	region, derpMapURL := s.app.RemoteRelaySettings()
	writeJSON(w, remoteRelayResponse{Region: region, DERPMapURL: derpMapURL})
}

// handleSetRemoteRelay 事务更新 tailcat 中继选择并热重建运行中的服务端。
//
// 参数说明：
//   - w: http.ResponseWriter，成功时输出已经提交的中继配置。
//   - r: *http.Request，请求体为 {"region":"...","derpmap_url":"..."}。
//
// 返回值说明：无；成功写入 200 JSON。
//
// 错误情况：JSON 非法、配置校验失败、DERP 探测失败、服务端重建失败或落盘失败时
// 返回 400；应用层事务会恢复旧配置与运行态。
func (s *Server) handleSetRemoteRelay(w http.ResponseWriter, r *http.Request) {
	var request remoteRelayResponse
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetRemoteRelay(request.Region, request.DERPMapURL); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetRemoteRelay(w, r)
}

// handleSetRemote 热切换远程连接服务端总开关，并由应用层原子联动 builtin-ssh。
//
// 参数说明：
//   - w: http.ResponseWriter，返回与 GET /api/remote 同构的最新状态。
//   - r: *http.Request，请求体包含 enabled 布尔值。
//
// 返回值说明：无；成功写入 200 JSON，失败写入 400 文本错误。
//
// 错误情况：JSON 非法、隧道调和失败或配置持久化失败时返回 400，事务保持原状态。
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
		Entries []config.RemoteAllowEntry `json:"entries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetRemoteAllow(req.Entries); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetRemote(w, r)
}

// handlePingRemote 对一个已保存远端或完整 token 执行一次隧道内 disco ping。
//
// 参数说明：
//   - w: http.ResponseWriter，用于输出 ProbeResult 或可读错误。
//   - r: *http.Request，请求体为 {"remote":"名称或 tc... token"}，context 控制取消。
//
// 返回值说明：无；成功返回 200 JSON，失败返回 400，避免把连接凭据写入 URL 与访问日志。
//
// 错误情况：JSON 非法、名称不存在、token 非法、授权拒绝或网络超时均返回错误文本。
func (s *Server) handlePingRemote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Remote string `json:"remote"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	result, err := s.app.PingRemote(ctx, strings.TrimSpace(req.Remote))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, result)
}

// handleGetRemoteAudit 返回 remote 专用审计环的最近事件。
//
// 参数说明：
//   - w: http.ResponseWriter，用于输出 {"entries": [...]}。
//   - r: *http.Request，可选 tail 查询参数，默认 100，允许范围 1..500。
//
// 返回值说明：无；成功返回按时间从旧到新排列的 JSON 数组。
//
// 错误情况：tail 非整数或越界时返回 400，不截断成含糊结果。
func (s *Server) handleGetRemoteAudit(w http.ResponseWriter, r *http.Request) {
	tail := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("tail")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 500 {
			http.Error(w, "tail 必须是 1..500 的整数", http.StatusBadRequest)
			return
		}
		tail = parsed
	}
	writeJSON(w, map[string]any{"entries": s.app.RemoteAudit(tail)})
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

// handleExportRemoteKeyFile 下载当前实际使用的完整服务端私钥。
//
// 参数说明：
//   - w: http.ResponseWriter，用于写入敏感 JSON 文件与下载响应头。
//   - r: *http.Request，仅用于符合 handler 签名，不读取请求参数。
//
// 返回值说明：无；成功返回 attachment JSON，内容可直接供 tailcat CLI 使用。
//
// 错误情况：密钥生成或读取失败时返回 500。私钥不会进入普通状态接口或错误日志。
func (s *Server) handleExportRemoteKeyFile(w http.ResponseWriter, _ *http.Request) {
	data, err := s.app.ExportRemoteServerKey()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="proxyd-server.private.json"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// handleImportRemoteKeyFile 接收原始 tailcat 私钥 JSON 并事务切换服务端身份。
//
// 参数说明：
//   - w: http.ResponseWriter，用于返回最新 remote 状态或校验错误。
//   - r: *http.Request，请求体是原始 *.private.json，最大 64 KiB。
//
// 返回值说明：无；成功返回 200 和最新打码状态，前端据此提示 token 已切换。
//
// 错误情况：文件过大/读取失败返回 400；格式非法或事务回滚返回 400，且旧身份保持可用。
func (s *Server) handleImportRemoteKeyFile(w http.ResponseWriter, r *http.Request) {
	reader := http.MaxBytesReader(w, r.Body, 64<<10)
	data, err := io.ReadAll(reader)
	if err != nil {
		http.Error(w, "密钥文件读取失败或超过 64 KiB", http.StatusBadRequest)
		return
	}
	if err := s.app.ImportRemoteServerKey(data); err != nil {
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

// handleSetRemoteShellUser 修改远程会话降权账户（空字符串恢复进程用户语义）。
// root 运行时开启内嵌 SSH/Web 终端前必须先设置普通账户，拒绝默认提供 root shell。
func (s *Server) handleSetRemoteShellUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ShellUser string `json:"shell_user"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetRemoteShellUser(req.ShellUser); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetRemote(w, r)
}

// handleSetRemoteWebTerminal 热切换浏览器终端开关，并传递非回环暴露的显式确认凭据。
//
// 参数说明：
//   - w: http.ResponseWriter，用于返回最新打码状态或安全门错误。
//   - r: *http.Request，请求体为 enabled 与 acknowledge_exposure 布尔字段。
//
// 返回值说明：无；成功返回与 GET /api/remote 同构的状态。
//
// 错误情况：JSON 非法返回 400；非回环 api-listen 开启且未确认、配置落盘失败时返回 400。
func (s *Server) handleSetRemoteWebTerminal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled             bool `json:"enabled"`
		AcknowledgeExposure bool `json:"acknowledge_exposure"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetRemoteWebTerminal(req.Enabled, req.AcknowledgeExposure); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetRemote(w, r)
}

// terminalClientMessage 是浏览器发送的文本控制帧；键盘输入始终使用二进制帧。
type terminalClientMessage struct {
	Type    string `json:"type"`
	Columns int    `json:"cols"`
	Rows    int    `json:"rows"`
}

// terminalStream 描述 WebSocket 桥接所需的最小终端会话接口，便于隔离协议测试。
type terminalStream interface {
	io.ReadWriteCloser
	Resize(remote.TerminalSize) error
}

// handleRemoteTerminal 把同源 WebSocket 升级为进程内 SSH shell 的交互 PTY 会话。
// 会话由 remote 模块自带的 shell 服务承载，不要求远程连接服务端运行或开启 builtin-ssh。
//
// 参数说明：
//   - w: http.ResponseWriter，用于升级协议或在升级前返回 404。
//   - r: *http.Request，可选 cols/rows 查询参数作为首次 PTY 尺寸，请求 context 控制会话寿命。
//
// 返回值说明：无；成功升级后，二进制帧承载终端输入/输出，文本帧仅承载 resize JSON。
//
// 错误情况：开关关闭返回 404；升级后初始化失败会发送可展示的 error 控制帧再关闭，
// 浏览器断开或 shell 退出会释放 SSH/PTY/子进程资源。
func (s *Server) handleRemoteTerminal(w http.ResponseWriter, r *http.Request) {
	status := s.app.RemoteStatus()
	if !status.WebTerminal {
		http.NotFound(w, r)
		return
	}

	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	connection.SetReadLimit(64 << 10)
	// WebSocket 完成 Hijack 后不再复用 http.Request.Context：coder/websocket 明确提示
	// 该 context 的生命周期在升级后不可依赖。独立 context 由桥接结束或 handler 返回时
	// 取消；浏览器断开会先让 WebSocket Read/Write 返回，再沿同一路径关闭 SSH/PTY。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer connection.Close(websocket.StatusNormalClosure, "终端会话已结束")

	session, err := s.app.OpenRemoteWebTerminal(ctx, terminalSizeFromRequest(r))
	if err != nil {
		_ = writeTerminalControl(ctx, connection, "error", err.Error())
		return
	}
	defer session.Close()
	bridgeTerminalWebSocket(ctx, connection, session)
}

// terminalSizeFromRequest 解析 WebSocket URL 上的首次终端尺寸，非法值交给领域层默认化。
//
// 参数说明：
//   - r: *http.Request，查询参数 cols/rows 来自 xterm 首次布局结果。
//
// 返回值说明：remote.TerminalSize；缺失、非整数或越界值会在 NormalizeTerminalSize 中收敛。
//
// 错误情况：无；解析失败保持零值，不因装饰性尺寸参数拒绝整个终端会话。
func terminalSizeFromRequest(r *http.Request) remote.TerminalSize {
	columns, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	return remote.TerminalSize{Columns: columns, Rows: rows}
}

// bridgeTerminalWebSocket 并发桥接终端输出与浏览器输入，任一方向结束即取消整个会话。
//
// 参数说明：
//   - ctx: context.Context，HTTP/WebSocket 生命周期。
//   - connection: *websocket.Conn，已完成同源校验和协议升级的浏览器连接。
//   - terminal: terminalStream，已启动 shell 的 SSH/PTY 会话。
//
// 返回值说明：无；第一个方向结束后立即关闭终端与 WebSocket，不等待失活方向超时。
//
// 错误情况：读写、控制帧或 resize 错误都会结束会话；终端输出不会写入普通日志，避免泄露命令内容。
func bridgeTerminalWebSocket(ctx context.Context, connection *websocket.Conn, terminal terminalStream) {
	bridgeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	go func() {
		results <- copyTerminalOutput(bridgeCtx, connection, terminal)
	}()
	go func() {
		results <- copyTerminalInput(bridgeCtx, connection, terminal)
	}()
	select {
	case <-bridgeCtx.Done():
	case <-results:
	}
	// 先关终端，再关 WebSocket；这样阻塞在 PTY Read 与 WebSocket Read 的两个方向都会被解除。
	_ = terminal.Close()
	_ = connection.Close(websocket.StatusNormalClosure, "终端会话已结束")
}

// copyTerminalOutput 把 PTY 原始输出逐块写为 WebSocket 二进制消息。
//
// 参数说明：ctx 控制写超时与取消；connection 是浏览器连接；terminal 是输出源。
//
// 返回值说明：error；shell 正常退出产生的 io.EOF 转换为 nil。
//
// 错误情况：PTY 读取或 WebSocket 写入失败时返回错误，由桥接器统一关闭会话。
func copyTerminalOutput(ctx context.Context, connection *websocket.Conn, terminal terminalStream) error {
	buffer := make([]byte, 32<<10)
	for {
		read, err := terminal.Read(buffer)
		if read > 0 {
			if writeErr := connection.Write(ctx, websocket.MessageBinary, buffer[:read]); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// copyTerminalInput 把浏览器消息分派为键盘字节或窗口尺寸控制。
//
// 参数说明：ctx 控制读取取消；connection 是浏览器连接；terminal 是输入/resize 目标。
//
// 返回值说明：error；连接关闭、消息非法、stdin 写入失败或 resize 失败时返回。
//
// 错误情况：单条消息超过 64 KiB、未知消息类型、非法 JSON 或短写均终止会话，
// 防止协议失同步后继续把不可信控制数据解释成 shell 输入。
func copyTerminalInput(ctx context.Context, connection *websocket.Conn, terminal terminalStream) error {
	for {
		messageType, payload, err := connection.Read(ctx)
		if err != nil {
			return err
		}
		switch messageType {
		case websocket.MessageBinary:
			for len(payload) > 0 {
				written, writeErr := terminal.Write(payload)
				if writeErr != nil {
					return writeErr
				}
				if written <= 0 {
					return io.ErrShortWrite
				}
				payload = payload[written:]
			}
		case websocket.MessageText:
			var message terminalClientMessage
			if err := json.Unmarshal(payload, &message); err != nil {
				return fmt.Errorf("解析终端控制消息失败: %w", err)
			}
			if message.Type != "resize" {
				return fmt.Errorf("不支持的终端控制消息 %q", message.Type)
			}
			if err := terminal.Resize(remote.TerminalSize{Columns: message.Columns, Rows: message.Rows}); err != nil {
				return fmt.Errorf("调整终端窗口失败: %w", err)
			}
		default:
			return fmt.Errorf("不支持的 WebSocket 消息类型 %d", messageType)
		}
	}
}

// writeTerminalControl 在 SSH 初始化失败时向已升级的浏览器发送可展示文本控制帧。
//
// 参数说明：ctx 控制写入；connection 是浏览器连接；kind/message 是控制类型与中文说明。
//
// 返回值说明：error，JSON 编码或 WebSocket 写失败时返回。
//
// 错误情况：连接已关闭或客户端停止读取时返回错误；调用方随后仍会释放连接。
func writeTerminalControl(ctx context.Context, connection *websocket.Conn, kind, message string) error {
	payload, err := json.Marshal(map[string]string{"type": kind, "message": message})
	if err != nil {
		return err
	}
	return connection.Write(ctx, websocket.MessageText, payload)
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
