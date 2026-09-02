// Package api 提供 proxyd 自有的 HTTP API 与内嵌 Web 控制台。
// mihomo 的 external-controller 路由不可扩展，因此使用独立监听地址。
package api

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	neturl "net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"proxyd/internal/app"
	"proxyd/internal/config"
	"proxyd/internal/logbuf"
	"proxyd/internal/node"
	"proxyd/internal/subscribe"
)

// webDistFS 是前端构建产物的内嵌文件系统。
//
// 为什么放在 internal/api/dist：Go embed 只能嵌入当前 package 目录下的文件，
// 因此 Vite 构建输出直接写入这里，避免运行时依赖外部 web/dist 目录。
//
//go:embed all:dist
var webDistFS embed.FS

// PortEntry 是映射表中的单条端口记录。
type PortEntry struct {
	Port         int    `json:"port"`
	Node         string `json:"node"`
	Subscription string `json:"subscription"`
	Delay        uint16 `json:"delay"`
	Alive        bool   `json:"alive"`
}

// NodeEntry 是节点列表中的单条记录。
type NodeEntry struct {
	Name         string `json:"name"`
	Key          string `json:"key"`  // 稳定身份（协议+地址+凭据），main-node 等按它引用节点
	Type         string `json:"type"` // 出站协议（ss/vmess/...）
	Subscription string `json:"subscription"`
	Delay        uint16 `json:"delay"`
	Alive        bool   `json:"alive"`
	FailReason   string `json:"fail_reason,omitempty"` // 测速失败原因
	Port         int    `json:"port"`                  // 0 表示未映射
}

// SubEntry 是订阅聚合信息。
type SubEntry struct {
	Name     string              `json:"name"`
	URL      string              `json:"url"`
	Type     string              `json:"type"`
	Enabled  bool                `json:"enabled"`
	State    string              `json:"state"` // disabled|empty|error|degraded|healthy
	Total    int                 `json:"total"`
	Alive    int                 `json:"alive"`
	UserInfo *subscribe.UserInfo `json:"userinfo,omitempty"`
}

// Overview 是 /api/overview 的响应。
type Overview struct {
	Mode               string                 `json:"mode"`
	Listen             string                 `json:"listen"`
	MixedPort          int                    `json:"mixed_port"`
	MainAuto           bool                   `json:"main_auto"`            // 主端口是否固定走最优节点（跳过规则）
	MainNode           string                 `json:"main_node"`            // 主端口固定节点（node key；空串=跟随规则）
	MainNodeUp         bool                   `json:"main_node_up"`         // main-node 当前是否生效（节点可用且 main-auto 未开）
	AutoPort           int                    `json:"auto_port"`            // 0 表示关闭
	PortMappingEnabled bool                   `json:"port_mapping_enabled"` // 一对一节点端口 listener 是否实际启用
	SystemProxy        bool                   `json:"system_proxy"`         // 配置状态（是否启用系统代理）
	TUN                app.TUNStatus          `json:"tun"`                  // TUN 开关与当前进程权限状态
	DNSPreset          string                 `json:"dns_preset"`           // off|fake-ip|redir-host
	DNSCustom          bool                   `json:"dns_custom"`           // true 表示手写 dns 段优先，预设暂不生效
	Version            app.VersionCheckStatus `json:"version_check"`        // 启动异步检查的缓存状态，不在 overview 请求中联网
	Autostart          bool                   `json:"autostart"`            // OS 级开机自启项是否存在（实时查询）
	ServerTime         string                 `json:"server_time"`          // 服务器本地时间（RFC3339，带时区偏移），供概览"更新于"显示
	PortRange          [2]int                 `json:"port_range"`
	Subs               []SubEntry             `json:"subscriptions"`
	ManualNodes        []app.ManualNodeEntry  `json:"manual_nodes"`
	Ports              []PortEntry            `json:"ports"`            // 当前实际监听的一对一节点端口
	PortAssignments    []PortEntry            `json:"port_assignments"` // 稳定分配快照；关闭映射时仍保留
	Nodes              []NodeEntry            `json:"nodes"`
	CustomRules        []string               `json:"custom_rules"`
	Groups             []config.NodeGroup     `json:"groups"`
}

// LogsResponse 是 /api/logs 的响应。
type LogsResponse struct {
	Entries []logbuf.Entry `json:"entries"`
}

// maxImportedConfigBytes 限制配置导入请求体为 1 MiB。
// 配置文件通常只有数 KiB；上限用于避免本地 API 被误传大文件后占用过多内存。
const maxImportedConfigBytes int64 = 1 << 20

// Server 包装自有 API 与 Web 控制台的 HTTP 服务。
type Server struct {
	addr string
	app  *app.App
	srv  *http.Server
	ln   net.Listener
}

// New 创建 API 服务（addr 如 127.0.0.1:19091）。
func New(addr string, a *app.App) *Server {
	return &Server{addr: addr, app: a}
}

// Start 在后台启动 API 与内嵌 Web 控制台监听。
//
// 参数：无；接收者 `s` 已包含监听地址和应用层编排器。
//
// 返回值：
//   - error：监听地址占用、地址非法或底层 `net.Listen` 失败时返回错误；成功时返回 nil。
//
// 错误情况：
//   - 路由注册阶段不做外部 I/O，不会因为 mihomo 未启动而失败；真正的 controller 访问发生在各 handler 内。
//   - `/api/connections` 与 `/api/traffic` 都是对 external-controller 的受控代理，统一由后端附加 secret，
//     这样 Web/CLI 无需持有凭据，也避免并发开发时在多个入口各自复制鉴权逻辑。
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ports", s.handlePorts) // 兼容旧接口
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("POST /api/mode", s.handleSetMode)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/test", s.handleTest)
	mux.HandleFunc("POST /api/subscriptions", s.handleAddSub)
	mux.HandleFunc("PUT /api/subscriptions/{name}", s.handleUpdateSub)
	mux.HandleFunc("DELETE /api/subscriptions/{name}", s.handleDelSub)
	mux.HandleFunc("POST /api/subscriptions/{name}/refresh", s.handleRefreshSub)
	mux.HandleFunc("POST /api/subscriptions/{name}/test", s.handleTestSub)
	mux.HandleFunc("POST /api/port-range", s.handleSetPortRange)
	mux.HandleFunc("POST /api/port-mapping", s.handleSetPortMapping)
	mux.HandleFunc("GET /api/rules", s.handleListRules)
	mux.HandleFunc("POST /api/rules", s.handleAddRule)
	mux.HandleFunc("POST /api/rules/reorder", s.handleMoveRule)
	mux.HandleFunc("PUT /api/rules/{index}", s.handleUpdateRule)
	mux.HandleFunc("DELETE /api/rules/{index}", s.handleDelRule)
	mux.HandleFunc("GET /api/groups", s.handleListGroups)
	mux.HandleFunc("POST /api/groups", s.handleAddGroup)
	mux.HandleFunc("PUT /api/groups/{name}", s.handleUpdateGroup)
	mux.HandleFunc("DELETE /api/groups/{name}", s.handleDelGroup)
	mux.HandleFunc("POST /api/auto-port", s.handleSetAutoPort)
	mux.HandleFunc("POST /api/main-auto", s.handleSetMainAuto)
	mux.HandleFunc("POST /api/main-node", s.handleSetMainNode)
	mux.HandleFunc("POST /api/main-port", s.handleSetMainPort)
	mux.HandleFunc("POST /api/system-proxy", s.handleSetSystemProxy)
	mux.HandleFunc("GET /api/tun", s.handleTUNStatus)
	mux.HandleFunc("POST /api/tun", s.handleSetTUN)
	mux.HandleFunc("POST /api/dns-preset", s.handleSetDNSPreset)
	mux.HandleFunc("POST /api/update-check", s.handleSetUpdateCheck)
	mux.HandleFunc("GET /api/config/export", s.handleExportConfig)
	mux.HandleFunc("POST /api/config/import/preview", s.handlePreviewImportConfig)
	mux.HandleFunc("POST /api/config/import", s.handleImportConfig)
	mux.HandleFunc("POST /api/autostart", s.handleSetAutostart)
	mux.HandleFunc("GET /api/rule-urls", s.handleListRuleURLs)
	mux.HandleFunc("POST /api/rule-urls", s.handleAddRuleURL)
	mux.HandleFunc("DELETE /api/rule-urls/{name}", s.handleDelRuleURL)
	mux.HandleFunc("GET /api/rule-urls/{name}/content", s.handleRuleURLContent)
	mux.HandleFunc("GET /api/manual-nodes", s.handleListManualNodes)
	mux.HandleFunc("POST /api/manual-nodes", s.handleAddManualNode)
	mux.HandleFunc("DELETE /api/manual-nodes/{index}", s.handleDelManualNode)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/traffic", s.handleTraffic)
	mux.HandleFunc("GET /api/connections", s.handleConnections)
	mux.HandleFunc("DELETE /api/connections", s.handleDeleteConnections)
	mux.HandleFunc("DELETE /api/connections/{id}", s.handleDeleteConnection)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /", s.handleStatic)
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

// Addr 返回实际监听地址（Start 传入 ":0" 时用于取回端口）。
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.addr
	}
	return s.ln.Addr().String()
}

// Shutdown 优雅关闭。
func (s *Server) Shutdown(ctx context.Context) {
	if s.srv != nil {
		_ = s.srv.Shutdown(ctx)
	}
}

// handleStatic 提供内嵌 Web 控制台静态资源，并为前端路由返回 index.html。
//
// 参数：
//   - w: http.ResponseWriter，用于写出静态文件、404 或 fallback 页面。
//   - r: *http.Request，读取 URL path 判断是资产、前端路由还是未知 API。
//
// 返回值：无；响应通过 w 写出。
//
// 错误情况：
//   - `/api/*` 未命中显式 API 路由时返回 404，避免把 API 拼写错误伪装成页面。
//   - 静态资源不存在且不是 API 路径时返回 `index.html`，让 SPA 自己处理路由。
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	dist, err := fs.Sub(webDistFS, "dist")
	if err != nil {
		http.Error(w, "web assets unavailable", http.StatusInternalServerError)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "." || name == "" {
		name = "index.html"
	}
	if f, err := dist.Open(name); err == nil {
		_ = f.Close()
		http.FileServer(http.FS(dist)).ServeHTTP(w, r)
		return
	}
	http.ServeFileFS(w, r, dist, "index.html")
}

func (s *Server) handlePorts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.portEntries())
}

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

// handleTraffic 代理 mihomo `/traffic` 实时速率流。
//
// 参数：
//   - w: http.ResponseWriter，用于逐行写出 NDJSON 数据并 flush。
//   - r: *http.Request，携带客户端取消信号；浏览器关闭页面时上游请求会随之取消。
//
// 返回值：无；响应是 mihomo 每秒输出的 JSON 行：
// `{"up":0,"down":0,"upTotal":0,"downTotal":0}`。
//
// 错误情况：
//   - external-controller 缺失或 URL 非法时返回 500。
//   - mihomo API 不可达或返回非 200 时返回 502。
//   - 客户端不支持流式 flush 时返回 500。
func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	resp, err := s.doControllerRequest(r.Context(), http.MethodGet, "/traffic", nil)
	if err != nil {
		s.writeControllerProxyError(w, "traffic", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.writeControllerUnexpectedStatus(w, "traffic", resp.StatusCode)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	// mihomo 的 /traffic 同时支持 WebSocket 和普通 HTTP 流。这里选择普通 HTTP 流，
	// 是因为后端代理只需附加 Authorization 头并逐行转发，前端无需知道 secret，
	// 也不需要额外引入 WebSocket 协议升级处理。
	reader := bufio.NewReader(resp.Body)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, err := w.Write(line); err != nil {
				return
			}
			flusher.Flush()
		}
		if readErr != nil {
			if readErr != io.EOF && r.Context().Err() == nil {
				log.Printf("[api] traffic stream interrupted: %v", readErr)
			}
			return
		}
	}
}

// handleConnections 代理 mihomo `GET /connections` JSON。
//
// 参数：
//   - w: http.ResponseWriter，用于写出上游成功响应的状态码、头和正文。
//   - r: *http.Request，提供客户端取消信号，并携带需要原样透传到 mihomo 的查询参数。
//
// 返回值：无；成功时透传上游 2xx/204 语义。
//
// 错误情况：
//   - external-controller 缺失或 URL 非法时返回 500，明确区分为本地配置错误。
//   - 上游不可达或返回非 2xx 时返回稳定 502，不泄漏 secret 或上游正文。
//   - 响应体复制异常只记日志，因为响应可能已经开始写出，继续改状态码没有意义。
func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	s.proxyControllerResponse(w, r, http.MethodGet, "/connections", r.URL.Query(), "connections")
}

// handleDeleteConnections 代理 mihomo `DELETE /connections`，用于关闭全部连接。
//
// 参数：
//   - w: http.ResponseWriter，用于返回上游成功状态；mihomo 常见返回为 204 No Content。
//   - r: *http.Request，提供取消信号；当前接口不消费请求体。
//
// 返回值：无；成功时透传上游 2xx/204 语义。
//
// 错误情况：
//   - external-controller 配置非法时返回 500。
//   - 上游不可达或返回非 2xx 时返回稳定 502，避免把内部错误页直接暴露给前端或 CLI。
func (s *Server) handleDeleteConnections(w http.ResponseWriter, r *http.Request) {
	s.proxyControllerResponse(w, r, http.MethodDelete, "/connections", nil, "delete connections")
}

// handleDeleteConnection 代理 mihomo `DELETE /connections/{id}`，用于关闭单条连接。
//
// 参数：
//   - w: http.ResponseWriter，用于返回上游成功状态；mihomo 常见返回为 204 No Content。
//   - r: *http.Request，路径参数 `{id}` 会先作为单个 path segment 安全转义，再拼到上游 URL。
//
// 返回值：无；成功时透传上游 2xx/204 语义。
//
// 错误情况：
//   - id 为空时返回 400，避免误把“删除单条”请求降级成“删除全部”。
//   - external-controller 配置非法时返回 500。
//   - 上游不可达或返回非 2xx 时返回稳定 502，同时确保未转义的特殊字符不会破坏上游路径结构。
func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "bad request: id required", http.StatusBadRequest)
		return
	}
	s.proxyControllerResponse(w, r, http.MethodDelete, "/connections/"+neturl.PathEscape(id), nil, "delete connection")
}

// doControllerRequest 构造并执行一次发往 mihomo external-controller 的 HTTP 请求。
//
// 参数：
//   - ctx: context.Context，请求生命周期；客户端断开、超时或调用方取消时会立即终止上游请求。
//   - method: string，HTTP 方法，例如 GET、DELETE。
//   - endpoint: string，mihomo API 路径，必须以 `/` 开头；若包含路径参数，调用方必须先做好 PathEscape。
//   - query: neturl.Values，可选查询参数；非 nil 时会完整编码到 URL 上。
//
// 返回值：
//   - *http.Response：上游原始响应；调用方负责关闭 Body。
//   - error：URL 拼接失败、本地请求对象构造失败或实际网络请求失败时返回错误。
//
// 错误情况：
//   - helper 只负责附加 Bearer secret，不会把 secret 拼进任何错误文本。
//   - setup 阶段错误表示 proxyd 本地 external-controller 配置无效；execute 阶段错误表示上游不可达。
//   - 这里不判断状态码，因为不同 handler 对成功状态的接受范围可能不同。
func (s *Server) doControllerRequest(ctx context.Context, method, endpoint string, query neturl.Values) (*http.Response, error) {
	target, err := mihomoEndpointURL(s.app.Config().ExternalController, endpoint, query)
	if err != nil {
		return nil, fmt.Errorf("setup controller request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, fmt.Errorf("setup controller request: %w", err)
	}
	if secret := strings.TrimSpace(s.app.Config().Secret); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute controller request: %w", err)
	}
	return resp, nil
}

// proxyControllerResponse 代理一个“成功即透传、失败即稳定 502”的 controller 接口。
//
// 参数：
//   - w: http.ResponseWriter，用于写出透传后的状态码、关键响应头和正文。
//   - r: *http.Request，提供 context；其请求体不会被读取或转发。
//   - method: string，转发给 external-controller 的 HTTP 方法。
//   - endpoint: string，目标 mihomo API 路径；调用方负责完成路径参数安全编码。
//   - query: neturl.Values，要透传的查询参数；nil 表示不携带查询串。
//   - action: string，稳定动作名，只用于日志与错误提示，不能包含敏感信息。
//
// 返回值：无；成功时直接向客户端写出上游 2xx/204 响应。
//
// 错误情况：
//   - 上游非 2xx 时不会透传正文，避免把 controller 的错误页、内部细节或潜在敏感内容反射出去。
//   - 只复制对调用方有意义的 `Content-Type` 与 `Cache-Control`，避免无关头部影响 proxyd 的统一行为。
//   - 响应体复制中途失败时仅记录日志，因为 HTTP 响应通常已开始写出，继续补写错误码会制造二次错误。
func (s *Server) proxyControllerResponse(w http.ResponseWriter, r *http.Request, method, endpoint string, query neturl.Values, action string) {
	resp, err := s.doControllerRequest(r.Context(), method, endpoint, query)
	if err != nil {
		s.writeControllerProxyError(w, action, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.writeControllerUnexpectedStatus(w, action, resp.StatusCode)
		return
	}
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if cacheControl := resp.Header.Get("Cache-Control"); cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	if resp.StatusCode == http.StatusNoContent {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil && r.Context().Err() == nil {
		log.Printf("[api] proxy controller %s response copy failed: %v", action, err)
	}
}

// writeControllerProxyError 把 controller 请求阶段错误映射为稳定的 proxyd HTTP 错误。
//
// 参数：
//   - w: http.ResponseWriter，用于输出统一错误响应。
//   - action: string，当前 controller 动作名，例如 `traffic`、`connections`。
//   - err: error，来自请求构造或网络访问阶段；可能包含底层网络细节。
//
// 返回值：无；统一通过 `http.Error` 写出错误响应。
//
// 错误情况：
//   - setup 阶段错误返回 500，明确提示 proxyd 本地 external-controller 配置有问题。
//   - execute 阶段错误返回 502，表明 mihomo 不可达或网络异常。
//   - 对外错误文本不拼接原始 err，避免把环境信息、路径或潜在敏感内容暴露给前端/CLI。
func (s *Server) writeControllerProxyError(w http.ResponseWriter, action string, err error) {
	if strings.HasPrefix(err.Error(), "setup controller request:") {
		http.Error(w, "controller request setup failed", http.StatusInternalServerError)
		return
	}
	http.Error(w, action+" upstream unavailable", http.StatusBadGateway)
}

// writeControllerUnexpectedStatus 把 controller 的非 2xx 状态收敛为稳定 502。
//
// 参数：
//   - w: http.ResponseWriter，用于写出统一错误响应。
//   - action: string，当前 controller 动作名，帮助调用方区分失败来源。
//   - statusCode: int，上游返回的 HTTP 状态码。
//
// 返回值：无；统一返回 502。
//
// 错误情况：
//   - 不透传上游正文，也不依赖上游 reason phrase，避免把不稳定文案或内部信息直接暴露给调用方。
//   - 仅保留数字状态码，足够定位问题，同时保证接口错误文案长期稳定。
func (s *Server) writeControllerUnexpectedStatus(w http.ResponseWriter, action string, statusCode int) {
	http.Error(w, fmt.Sprintf("%s upstream returned status %d", action, statusCode), http.StatusBadGateway)
}

// mihomoEndpointURL 把 external-controller 配置拼成 mihomo API URL。
//
// 参数：
//   - controller: string，配置里的 external-controller，可带或不带 scheme。
//   - endpoint: string，目标 API 路径，必须以 `/` 开头；允许包含已经完成百分号编码的 path segment。
//   - query: neturl.Values，可选查询参数；非空时会编码到 URL 的 RawQuery 中。
//
// 返回值：
//   - string，完整 URL。
//   - error，controller 为空、endpoint 非法或 URL 无 host 时返回。
//
// 错误情况：
//   - 用户配置了非法 external-controller 时返回错误，由 handler 转成 500。
func mihomoEndpointURL(controller, endpoint string, query neturl.Values) (string, error) {
	controller = strings.TrimSpace(controller)
	if controller == "" {
		return "", fmt.Errorf("external-controller is empty")
	}
	if !strings.HasPrefix(endpoint, "/") {
		return "", fmt.Errorf("endpoint must start with /")
	}
	if !strings.Contains(controller, "://") {
		controller = "http://" + controller
	}
	u, err := neturl.Parse(controller)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("external-controller host is empty")
	}
	decodedEndpoint, err := neturl.PathUnescape(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint path: %w", err)
	}
	basePath := strings.TrimRight(u.Path, "/")
	u.Path = basePath + decodedEndpoint
	u.RawPath = basePath + endpoint
	if query != nil {
		u.RawQuery = query.Encode()
	} else {
		u.RawQuery = ""
	}
	return u.String(), nil
}

// assignmentEntries 把应用层稳定 assignments 转换为 API 只读记录。
//
// 参数：无。
//
// 返回值：[]PortEntry，包含当前稳定分配；调用方可以自由筛选而不修改应用状态。
//
// 错误情况：无；assignment 在应用层生成时已经保证 Node 非空。
func (s *Server) assignmentEntries() []PortEntry {
	assigns := s.app.Assignments()
	entries := make([]PortEntry, 0, len(assigns))
	for _, as := range assigns {
		entries = append(entries, PortEntry{
			Port:         as.Port,
			Node:         as.Node.Name,
			Subscription: as.Node.Subscription,
			Delay:        as.Node.Delay,
			Alive:        as.Node.Alive,
		})
	}
	return entries
}

// portEntries 返回当前真正启用的一对一节点监听端口。
//
// 参数：无。
//
// 返回值：[]PortEntry；端口映射关闭时返回空切片，开启时返回稳定 assignments。
//
// 错误情况：无；该方法刻意区分“已分配但停用”和“正在监听”，避免 API 误报端口可用。
func (s *Server) portEntries() []PortEntry {
	if !s.app.Config().PortMappingEnabled() {
		return []PortEntry{}
	}
	return s.assignmentEntries()
}

// handleOverview 聚合应用内存快照，返回控制台一次轮询所需的完整只读状态。
//
// 参数：
//   - w: http.ResponseWriter，写出 JSON 响应。
//   - 请求参数未使用；该接口不接受筛选条件。
//
// 返回值：无；成功返回 HTTP 200 与 Overview JSON。
//
// 错误情况：无外部 I/O；版本检查只读取 App 缓存，绝不会在十秒轮询路径同步访问 GitHub。
func (s *Server) handleOverview(w http.ResponseWriter, _ *http.Request) {
	cfg := s.app.Config()
	subInfos := s.app.SubscriptionUserInfos()
	portOf := map[string]int{}
	if cfg.PortMappingEnabled() {
		for _, as := range s.app.Assignments() {
			portOf[as.Node.Name] = as.Port
		}
	}

	subs := map[string]int{} // 订阅名 -> ov.Subs 下标
	ov := Overview{
		Mode:               s.app.Mode(),
		Listen:             cfg.Listen,
		MixedPort:          cfg.MixedPort,
		MainAuto:           cfg.MainAuto,
		MainNode:           cfg.MainNode,
		AutoPort:           cfg.AutoPort,
		PortMappingEnabled: cfg.PortMappingEnabled(),
		SystemProxy:        cfg.SystemProxy,
		TUN:                s.app.TUNStatus(),
		DNSPreset:          cfg.DNSPreset,
		DNSCustom:          len(cfg.DNS) > 0,
		Version:            s.app.VersionStatus(),
		Autostart:          s.app.AutostartStatus(),
		ServerTime:         time.Now().Format(time.RFC3339), // 服务器本地时间（含时区偏移）
		PortRange:          cfg.PortRange,
		Subs:               []SubEntry{},
		ManualNodes:        s.app.ManualNodes(),
		Nodes:              []NodeEntry{},
		CustomRules:        s.app.CustomRules(),
		Groups:             s.app.Groups(),
	}
	for _, sub := range s.app.Subscriptions() {
		subs[sub.Name] = len(ov.Subs)
		entry := SubEntry{Name: sub.Name, URL: sub.URL, Type: sub.Type, Enabled: sub.IsEnabled()}
		if info, ok := subInfos[sub.Name]; ok {
			entry.UserInfo = &info
		}
		ov.Subs = append(ov.Subs, entry)
	}
	for _, n := range s.app.Nodes() {
		ov.Nodes = append(ov.Nodes, NodeEntry{
			Name:         n.Name,
			Key:          n.Key(),
			Type:         nodeType(n),
			Subscription: n.Subscription,
			Delay:        n.Delay,
			Alive:        n.Alive,
			FailReason:   n.FailReason,
			Port:         portOf[n.Name],
		})
		if i, ok := subs[n.Subscription]; ok {
			ov.Subs[i].Total++
			if n.Alive {
				ov.Subs[i].Alive++
			}
		}
		if cfg.MainNode != "" && !cfg.MainAuto && n.Alive && n.Key() == cfg.MainNode && portOf[n.Name] != 0 {
			ov.MainNodeUp = true
		}
	}
	for index := range ov.Subs {
		subscription := &ov.Subs[index]
		switch {
		case !subscription.Enabled:
			subscription.State = "disabled"
		case subscription.Total == 0:
			subscription.State = "empty"
		case subscription.Alive == 0:
			subscription.State = "error"
		case subscription.Alive < subscription.Total:
			subscription.State = "degraded"
		default:
			subscription.State = "healthy"
		}
	}
	ov.Ports = s.portEntries()
	ov.PortAssignments = s.assignmentEntries()
	writeJSON(w, ov)
}

func (s *Server) handleSetMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetMode(req.Mode); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"mode": s.app.Mode()})
}

func (s *Server) handleAddSub(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Type    string `json:"type"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		http.Error(w, "bad request: url required", http.StatusBadRequest)
		return
	}
	sub, err := s.app.AddSubscriptionEntry(config.Subscription{
		Name: req.Name, URL: req.URL, Type: req.Type, Enabled: req.Enabled,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if sub.IsEnabled() {
		s.trigger(true)
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, sub)
}

// handleUpdateSub 同步编辑订阅名称、URL、类型和启用状态。
//
// 参数：
//   - w: http.ResponseWriter，写出提交后的订阅或事务错误。
//   - r: *http.Request，路径 `{name}` 是当前名称，请求体包含目标 name/url/type/enabled。
//
// 返回值：无；成功返回 HTTP 200 与完整订阅值。
//
// 错误情况：JSON/字段非法、订阅不存在、启用拉取无缓存、健康检测、热更新、持久化
// 或回滚失败时返回 400；同步执行使界面不会先乐观显示再被后台失败推翻。
func (s *Server) handleUpdateSub(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Type    string `json:"type"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	updated, err := s.app.UpdateSubscription(ctx, r.PathValue("name"), config.Subscription{
		Name: req.Name, URL: req.URL, Type: req.Type, Enabled: req.Enabled,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, updated)
}

func (s *Server) handleDelSub(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RemoveSubscription(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.trigger(true)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRefresh(w http.ResponseWriter, _ *http.Request) {
	s.trigger(true)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"refreshing"}`))
}

// handleTest 手动测速：只对现有节点做健康检测/延迟测试，不重新拉订阅。
func (s *Server) handleTest(w http.ResponseWriter, _ *http.Request) {
	s.trigger(false)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"testing"}`))
}

// handleRefreshSub 只刷新单个订阅（拉取该订阅 + 检测其节点 + 热更新）。
// 与全局刷新不同，这里同步执行（最长 3 分钟）以便把拉取失败等原因直接返回给前端。
func (s *Server) handleRefreshSub(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := s.app.RefreshSubscription(ctx, r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleTestSub 只对单个订阅的现有节点做健康检测/延迟测试，同步返回结果。
func (s *Server) handleTestSub(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := s.app.TestSubscription(ctx, r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// trigger 异步执行一轮流水线；fetch=true 拉订阅+测速，false 仅测速。
// 完整一轮可能耗时数十秒（订阅拉取 + 全部节点测速）。
func (s *Server) trigger(fetch bool) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := s.app.Refresh(ctx, fetch); err != nil {
			log.Printf("[api] trigger(fetch=%v): %v", fetch, err)
		}
	}()
}

// handleSetPortRange 修改节点映射端口区间：持久化 + 重新分配端口 + 热更新（不同步测速）。
func (s *Server) handleSetPortRange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Range string `json:"range"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pr, err := config.ParsePortRange(req.Range)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.app.SetPortRange(pr[0], pr[1]); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string][2]int{"port_range": pr})
}

// handleSetPortMapping 开关健康节点的一对一端口 listener，并返回提交后的有效状态。
//
// 参数：
//   - w: http.ResponseWriter，写出 JSON 响应或事务失败信息。
//   - r: *http.Request，请求体必须是 `{ "enabled": boolean }`。
//
// 返回值：无；成功返回 HTTP 200 与 port_mapping_enabled 字段。
//
// 错误情况：JSON 无效返回 400；热更新、持久化或回滚失败也返回 400，正文保留
// 应用层合并错误，便于 UI 明确提示用户检查运行态。
func (s *Server) handleSetPortMapping(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetPortMapping(req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"port_mapping_enabled": s.app.Config().PortMappingEnabled()})
}

func (s *Server) handleListRules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.CustomRules())
}

// handleAddRule 追加自定义规则（前置到内置规则之前），持久化 + 热更新。
func (s *Server) handleAddRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rule string `json:"rule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Rule) == "" {
		http.Error(w, "bad request: rule required", http.StatusBadRequest)
		return
	}
	if err := s.app.AddRule(req.Rule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, s.app.CustomRules())
}

func (s *Server) handleDelRule(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		http.Error(w, "bad request: index must be an integer", http.StatusBadRequest)
		return
	}
	if err := s.app.RemoveRule(index); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateRule 原位编辑一条 custom-rules 规则并保留其优先级位置。
//
// 参数：
//   - w: http.ResponseWriter，写出更新后的完整自定义规则列表或错误。
//   - r: *http.Request，路径 index 为零基下标，请求体为 `{ "rule": "..." }`。
//
// 返回值：无；成功返回 HTTP 200 与最新规则数组。
//
// 错误情况：下标/JSON/规则非法、mihomo 自检、持久化或回滚失败时返回 400。
func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		http.Error(w, "bad request: index must be an integer", http.StatusBadRequest)
		return
	}
	var req struct {
		Rule string `json:"rule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Rule) == "" {
		http.Error(w, "bad request: rule required", http.StatusBadRequest)
		return
	}
	if err := s.app.UpdateRule(index, req.Rule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.app.CustomRules())
}

// handleMoveRule 调整 custom-rules 中一条规则的优先级顺序。
//
// 参数：
//   - w: http.ResponseWriter，写出重排后的完整规则列表或错误。
//   - r: *http.Request，请求体为 `{ "from": number, "to": number }`。
//
// 返回值：无；成功返回 HTTP 200 与最新规则数组。
//
// 错误情况：JSON/下标非法、热更新、持久化或回滚失败时返回 400；远程规则源内容
// 不在该接口索引空间内，因此不能被误编辑或重排。
func (s *Server) handleMoveRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From int `json:"from"`
		To   int `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.MoveRule(req.From, req.To); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.app.CustomRules())
}

func (s *Server) handleListGroups(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.Groups())
}

// handleAddGroup 新增节点分组（组内 url-test 自动选优），持久化 + 热更新。
func (s *Server) handleAddGroup(w http.ResponseWriter, r *http.Request) {
	var g config.NodeGroup
	if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.AddGroup(g); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, g)
}

// handleUpdateGroup 修改现有策略分组的端口、类型或成员来源。
//
// 参数：
//   - w: http.ResponseWriter，写出更新后的分组或应用层事务错误。
//   - r: *http.Request，路径 name 为当前分组名，请求体为完整 NodeGroup JSON。
//
// 返回值：无；成功返回 HTTP 200 与提交后的分组。
//
// 错误情况：请求体非法、分组不存在、尝试改名、端口冲突、热更新或持久化失败时返回 400。
func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	var group config.NodeGroup
	if err := json.NewDecoder(r.Body).Decode(&group); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.UpdateGroup(r.PathValue("name"), group); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, group)
}

func (s *Server) handleDelGroup(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RemoveGroup(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetAutoPort 设置自动选优端口（0=关闭），持久化 + 热更新。
func (s *Server) handleSetAutoPort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetAutoPort(req.Port); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]int{"auto_port": req.Port})
}

// handleSetMainAuto 开关「主端口使用最优节点」（跳过规则匹配），持久化 + 热更新。
func (s *Server) handleSetMainAuto(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetMainAuto(req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"main_auto": req.Enabled})
}

// handleSetMainNode 设置主端口固定节点（node key；空串=恢复规则模式），持久化 + 热更新。
// main-auto 开启时该设置被忽略（auto 优先）。
func (s *Server) handleSetMainNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node string `json:"node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetMainNode(req.Node); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"main_node": req.Node})
}

// handleSetMainPort 修改主端口：校验冲突 + 持久化 + 热更新；
// 系统代理已开启时自动重新绑定到新端口。
func (s *Server) handleSetMainPort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetMainPort(req.Port); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]int{"mixed_port": req.Port})
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

func (s *Server) handleListRuleURLs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.RuleURLs())
}

// handleAddRuleURL 新增规则源：持久化 + 立即拉取该源 + 热更新。
func (s *Server) handleAddRuleURL(w http.ResponseWriter, r *http.Request) {
	var ru config.RuleURL
	if err := json.NewDecoder(r.Body).Decode(&ru); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.AddRuleURL(ru); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, s.app.RuleURLs())
}

func (s *Server) handleDelRuleURL(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RemoveRuleURL(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRuleURLContent 返回规则源适合管理界面阅读的文本。
//
// 参数：
//   - w: http.ResponseWriter，写出 UTF-8 文本或 404 错误。
//   - r: *http.Request，路径参数 name 指定要查看的规则源。
//
// 返回值：无；普通规则源返回原文，整体 Base64 gfwlist 返回解码后的 AutoProxy 内容。
//
// 错误情况：
// 规则源不存在，或缓存缺失且现场拉取失败时返回 404。接口不在前端重复实现 Base64
// 识别，确保 Web、CLI 以及未来调用方共享同一个可读内容契约。
func (s *Server) handleRuleURLContent(w http.ResponseWriter, r *http.Request) {
	body, err := s.app.RuleURLContent(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(body)
}

func (s *Server) handleListManualNodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.ManualNodes())
}

// handleAddManualNode 添加手动节点：校验 + 持久化，随后后台刷新纳入节点池。
func (s *Server) handleAddManualNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string `json:"url"`
		Name string `json:"name,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		http.Error(w, "bad request: url required", http.StatusBadRequest)
		return
	}
	entry, err := s.app.AddManualNode(req.URL, req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.trigger(true)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, entry)
}

func (s *Server) handleDelManualNode(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		http.Error(w, "bad request: index must be an integer", http.StatusBadRequest)
		return
	}
	if err := s.app.RemoveManualNode(index); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.trigger(true)
	w.WriteHeader(http.StatusNoContent)
}

// nodeType 取节点的出站协议名。
func nodeType(n *node.Node) string {
	if t, ok := n.Mapping["type"].(string); ok {
		return t
	}
	return ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
