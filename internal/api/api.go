// Package api 提供 proxyd 自有的 HTTP API 与内嵌 Web 控制台。
// mihomo 的 external-controller 路由不可扩展，因此使用独立监听地址。
package api

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"proxyd/internal/app"
)

// webDistFS 是前端构建产物的内嵌文件系统。
//
// 为什么放在 internal/api/dist：Go embed 只能嵌入当前 package 目录下的文件，
// 因此 Vite 构建输出直接写入这里，避免运行时依赖外部 web/dist 目录。
//
//go:embed all:dist
var webDistFS embed.FS

// apiIdleTimeout 是 HTTP keep-alive 连接在两个请求之间允许闲置的最长时间。
// 该超时不作用于正在传输的 /api/traffic 流，只回收客户端遗忘的空闲连接及其缓冲区。
const apiIdleTimeout = 2 * time.Minute

// asyncRefreshTimeout 是一次后台全局刷新或测速允许执行的最长时间。
// 与原 API 行为保持一致；超时上下文同时作为服务关闭时取消在途任务的边界。
const asyncRefreshTimeout = 3 * time.Minute

// Server 包装自有 API 与 Web 控制台的 HTTP 服务。
type Server struct {
	addr string
	app  *app.App
	srv  *http.Server
	ln   net.Listener

	triggerMu      sync.Mutex
	triggerWake    chan struct{}
	triggerCancel  context.CancelFunc
	triggerClosed  bool
	pendingRefresh bool
	pendingTest    bool
	runRefresh     func(context.Context, bool) error

	// memoryBytes 缓存 mihomo `/memory` 流推送的最新 inuse 值；0 表示尚未拿到。
	// `/memory` 是一秒一跳的常驻流，不能在请求路径上同步解码，否则每次轮询都会被阻塞一拍。
	memoryBytes atomic.Uint64
	memCancel   context.CancelFunc

	// restartFn 由进程入口注入，负责派生重启子进程；nil 表示当前运行方式不支持 API 重启。
	restartFn func() error
	// restartInFlight 保证重启只触发一次；重启后进程退出，无需复位。
	restartInFlight atomic.Bool
}

// New 创建 API 服务及惰性启动的刷新任务合并器。
//
// 参数：
//   - addr: string，API 监听地址，例如 127.0.0.1:19091；可使用端口 0 由系统分配。
//   - a: *app.App，应用层用例编排器，负责执行刷新、配置变更和只读查询。
//
// 返回值：
//   - *Server：尚未监听网络的服务实例；首次 trigger 时才创建后台 worker。
//
// 错误情况：构造阶段不做监听或外部 I/O，因此不返回错误。a 为 nil 时构造仍可完成，
// 但任何需要应用层的 handler 都无法工作；正常入口必须传入有效 App。
func New(addr string, a *app.App) *Server {
	return &Server{
		addr:        addr,
		app:         a,
		triggerWake: make(chan struct{}, 1),
		runRefresh:  a.Refresh,
	}
}

// SetRestarter 注入进程重启回调，供 POST /api/restart 使用。
//
// 参数：
//   - fn: func() error，由进程入口实现，负责派生执行 restart 的 detached 子进程；
//     传入 nil 或不调用时，重启接口返回 503。
//
// 返回值：无。
//
// 错误情况：无；该 setter 只保存引用，真正的错误延迟到重启请求时由 fn 返回并记日志。
func (s *Server) SetRestarter(fn func() error) {
	s.restartFn = fn
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
	// 代理域路由，按关注点分散在各 proxy_*.go 的 register 方法中。
	s.registerProxySubscriptionRoutes(mux)
	s.registerProxyNodeRoutes(mux)
	s.registerProxyRuleRoutes(mux)
	s.registerProxyGroupRoutes(mux)
	s.registerProxyPortRoutes(mux)
	// 系统与共享路由（system-proxy/tun/dns-preset/update-check/config/restart/autostart/logs/healthz）。
	s.registerSystemRoutes(mux)
	// mihomo external-controller 受控代理（/api/traffic、/api/connections*）。
	s.registerControllerRoutes(mux)
	// 「远程连接」周边模块路由（/api/remote*）。
	s.registerRemoteRoutes(mux)
	mux.HandleFunc("GET /", s.handleStatic)
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.ln = ln
	// ReadHeaderTimeout 防止慢速请求头长期占用连接；IdleTimeout 只回收请求间的空闲
	// keep-alive。不能设置全局 WriteTimeout，因为 /api/traffic 是合法的常驻流式响应。
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       apiIdleTimeout,
	}
	go func() { _ = s.srv.Serve(ln) }()
	s.startMemoryWatcher()
	return nil
}

// Addr 返回实际监听地址（Start 传入 ":0" 时用于取回端口）。
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.addr
	}
	return s.ln.Addr().String()
}

// Shutdown 停止接收后台刷新请求，并优雅关闭 HTTP 服务；超时后强制断开连接。
//
// 参数：
//   - ctx: context.Context，限制 HTTP 在途请求完成的等待时间。
//
// 返回值：无；关闭错误不再向上传播，调用方可依赖方法幂等地完成退出清理。
//
// 错误情况：/api/traffic 是常驻 NDJSON 流，浏览器未关闭时 Shutdown 可能等到 ctx
// 超时，此时退化为 Close。后台刷新 worker 会先收到取消信号，但这里不无限等待它，
// 因为应用层事务可能正在等待自身锁；worker 数量固定为一个，不会继续累积资源。
func (s *Server) Shutdown(ctx context.Context) {
	s.stopTriggerWorker()
	if s.memCancel != nil {
		s.memCancel()
	}
	if s.srv == nil {
		return
	}
	if err := s.srv.Shutdown(ctx); err != nil {
		_ = s.srv.Close()
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

// trigger 把全局刷新或测速请求提交给单一后台 worker，并合并尚未执行的同类请求。
//
// 参数：
//   - fetch: bool，true 表示拉订阅后测速，false 表示只测试当前节点。
//
// 返回值：无；API 仍立即返回 202，任务结果延续既有行为写入日志与应用状态。
//
// 错误情况：服务已关闭时忽略新任务。正在执行期间，同类突发请求最多保留一个待执行
// 标记；完整刷新与仅测速分别保留，避免把不同用户意图互相覆盖。
func (s *Server) trigger(fetch bool) {
	s.triggerMu.Lock()
	defer s.triggerMu.Unlock()
	if s.triggerClosed {
		return
	}
	if s.triggerCancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.triggerCancel = cancel
		go s.runTriggerWorker(ctx)
	}
	if fetch {
		s.pendingRefresh = true
	} else {
		s.pendingTest = true
	}
	select {
	case s.triggerWake <- struct{}{}:
	default:
		// 一个唤醒信号足以让 worker 读取 mutex 保护的全部待执行标记；重复信号没有价值。
	}
}

// runTriggerWorker 串行消费合并后的全局刷新与测速任务。
//
// 参数：
//   - ctx: context.Context，服务关闭时取消 worker 和当前应用层任务。
//
// 返回值：无；每个任务独立建立三分钟超时并把失败写入日志。
//
// 错误情况：应用层错误不终止 worker，后续待执行任务仍会继续。关闭导致的 context
// 错误不重复记录，避免正常退出污染日志；panic 延续 Go 默认行为，不在此吞掉程序错误。
func (s *Server) runTriggerWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.triggerWake:
			for {
				fetch, ok := s.takePendingTrigger()
				if !ok {
					break
				}
				taskCtx, cancel := context.WithTimeout(ctx, asyncRefreshTimeout)
				err := s.runRefresh(taskCtx, fetch)
				cancel()
				if err != nil && ctx.Err() == nil {
					log.Printf("[api] trigger(fetch=%v): %v", fetch, err)
				}
			}
		}
	}
}

// takePendingTrigger 原子地取出一个待执行任务，完整刷新优先于仅测速。
//
// 参数：无；待执行状态保存在 Server 内部并由 triggerMu 保护。
//
// 返回值：
//   - bool：任务的 fetch 参数；没有任务时值无意义。
//   - bool：是否成功取得任务。
//
// 错误情况：无。只清除本次取出的一个标记，另一类任务仍会在当前唤醒周期继续执行。
func (s *Server) takePendingTrigger() (bool, bool) {
	s.triggerMu.Lock()
	defer s.triggerMu.Unlock()
	if s.pendingRefresh {
		s.pendingRefresh = false
		return true, true
	}
	if s.pendingTest {
		s.pendingTest = false
		return false, true
	}
	return false, false
}

// stopTriggerWorker 禁止新任务、清空未执行任务并取消唯一后台 worker。
//
// 参数：无。
//
// 返回值：无；方法可重复调用，尚未触发过任务时只记录关闭状态。
//
// 错误情况：不等待应用层回调退出，避免回调正等待应用互斥锁时阻塞 HTTP 关闭。取消
// 上下文会尽快终止支持 context 的网络操作，且固定单 worker 保证最多残留一个在途调用。
func (s *Server) stopTriggerWorker() {
	s.triggerMu.Lock()
	if s.triggerClosed {
		s.triggerMu.Unlock()
		return
	}
	s.triggerClosed = true
	s.pendingRefresh = false
	s.pendingTest = false
	cancel := s.triggerCancel
	s.triggerMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// startMemoryWatcher 启动后台 goroutine 订阅 mihomo `/memory` 流。
//
// 参数：无；接收者 `s` 提供 external-controller 配置与缓存字段。
//
// 返回值：无；watcher 生命周期由 Shutdown 通过 memCancel 结束。
//
// 错误情况：
//   - mihomo `/memory` 是常驻 NDJSON 式流（每秒推送一次 inuse），同步拉取会阻塞请求路径，
//     因此只能在后台持续消费并把最新值写入缓存；连接断开或上游不可达时延迟重试。
func (s *Server) startMemoryWatcher() {
	ctx, cancel := context.WithCancel(context.Background())
	s.memCancel = cancel
	go s.watchMemory(ctx)
}

// watchMemory 持续消费 mihomo `/memory` 流并刷新内存缓存。
//
// 参数：
//   - ctx: context.Context，Shutdown 时取消，正在解码的流会随连接关闭而退出。
//
// 返回值：无；只有 ctx 取消后才会返回。
//
// 错误情况：
//   - 建连失败、非 2xx 或流中断都进入 2 秒退避重试，不写日志，避免上游长期不可用时刷日志。
func (s *Server) watchMemory(ctx context.Context) {
	for ctx.Err() == nil {
		_ = s.streamMemory(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// streamMemory 建立一次 `/memory` 流连接并逐条消费，直到流出错或被取消。
//
// 参数：
//   - ctx: context.Context，控制这次流的生命周期。
//
// 返回值：
//   - error：建连失败、非 2xx 或流解码失败时返回；ctx 取消导致的错误同样返回，由调用方判断。
//
// 错误情况：
//   - 上游推送的 inuse 为 0 时忽略该帧，保留上一次有效值，避免启动瞬间的空采样把缓存清零。
func (s *Server) streamMemory(ctx context.Context) error {
	resp, err := s.doControllerRequest(ctx, http.MethodGet, "/memory", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("memory stream returned status %d", resp.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
	for {
		var payload struct {
			Inuse uint64 `json:"inuse"`
		}
		if err := decoder.Decode(&payload); err != nil {
			return err
		}
		if payload.Inuse > 0 {
			s.memoryBytes.Store(payload.Inuse)
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
