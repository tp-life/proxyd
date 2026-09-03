package api

// mihomo external-controller 受控代理：/api/traffic 与 /api/connections*。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"strings"
)

// registerControllerRoutes 注册 mihomo external-controller 的受控代理路由。
func (s *Server) registerControllerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/traffic", s.handleTraffic)
	mux.HandleFunc("GET /api/connections", s.handleConnections)
	mux.HandleFunc("DELETE /api/connections", s.handleDeleteConnections)
	mux.HandleFunc("DELETE /api/connections/{id}", s.handleDeleteConnection)
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
			// proxyd 每次热应用配置都会经 hub.Parse → ApplyConfig 重建 mihomo 的
			// REST API（见 core.Runner.Reload），在途的 /traffic 长连接会被上游主动
			// 掐断，读取侧得到 io.ErrUnexpectedEOF。这是正常生命周期事件，前端会自动
			// 重连，不应记为错误；其余读错误才需要排查。
			if readErr != io.EOF && !errors.Is(readErr, io.ErrUnexpectedEOF) && r.Context().Err() == nil {
				log.Printf("[api] traffic stream interrupted: %v", readErr)
			}
			return
		}
	}
}

// handleConnections 代理 mihomo `GET /connections` JSON，并注入当前内存占用。
//
// 参数：
//   - w: http.ResponseWriter，用于写出上游成功响应的状态码、头和正文。
//   - r: *http.Request，提供客户端取消信号，并携带需要原样透传到 mihomo 的查询参数。
//
// 返回值：无；成功时透传上游 JSON，且在顶层补充 `memory`（字节数）字段。
//
// 错误情况：
//   - external-controller 缺失或 URL 非法时返回 500，明确区分为本地配置错误。
//   - 上游不可达或返回非 2xx 时返回稳定 502，不泄漏 secret 或上游正文。
//   - 内存来自后台 watcher 缓存（mihomo `/memory` 是一秒一跳的常驻流，不能同步拉取），
//     尚未采集到时省略该字段；上游 JSON 解析失败时原样透传，连接列表本身不受影响。
func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	resp, err := s.doControllerRequest(r.Context(), http.MethodGet, "/connections", r.URL.Query())
	if err != nil {
		s.writeControllerProxyError(w, "connections", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.writeControllerUnexpectedStatus(w, "connections", resp.StatusCode)
		return
	}
	// 连接数量可能很多，但仍给一个上限，避免异常上游把内存打爆。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil && r.Context().Err() == nil {
		log.Printf("[api] proxy controller connections response read failed: %v", err)
	}
	out := enrichConnectionsWithMemory(body, s.memoryBytes.Load())
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if cacheControl := resp.Header.Get("Cache-Control"); cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(out); err != nil && r.Context().Err() == nil {
		log.Printf("[api] proxy controller connections response write failed: %v", err)
	}
}

// enrichConnectionsWithMemory 把内存占用注入连接列表响应顶层。
//
// 参数：
//   - body: []byte，mihomo `/connections` 的原始 JSON。
//   - memory: uint64，mihomo `/memory` 返回的 inuse 字节数；0 表示不可用。
//
// 返回值：
//   - []byte：注入 `memory` 字段后的 JSON；解析失败或 memory 为 0 时原样返回。
//
// 错误情况：
//   - 无；所有异常输入都会退化为原样透传。
func enrichConnectionsWithMemory(body []byte, memory uint64) []byte {
	if memory == 0 {
		return body
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	raw, err := json.Marshal(memory)
	if err != nil {
		return body
	}
	payload["memory"] = raw
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
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
