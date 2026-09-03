package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"proxyd/internal/app"
	"proxyd/internal/config"
	"proxyd/internal/logbuf"
)

// TestTriggerCoalescesBurst 验证 API 短时间收到大量相同刷新请求时，只保留一个正在
// 执行的任务和一个待执行任务，避免请求数直接转化为长期等待应用互斥锁的 goroutine。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于控制刷新回调阻塞点并检查调用次数。
//
// 返回值：无；通过两次执行记录和无第三次执行断言表达结果。
//
// 错误情况：首个任务未启动、突发请求全部丢失、相同任务未合并、fetch 语义改变，
// 或关闭调度器不能取消在途回调时测试失败。
func TestTriggerCoalescesBurst(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	server := New("127.0.0.1:0", a)
	started := make(chan bool, 3)
	release := make(chan struct{}, 2)

	// 注入可控回调只替代应用层执行边界，不改变 trigger 的排队与合并逻辑。回调同时
	// 监听 ctx，确保测试失败时 Shutdown 仍能终止 worker，而不会遗留测试 goroutine。
	server.runRefresh = func(ctx context.Context, fetch bool) error {
		select {
		case started <- fetch:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.Cleanup(func() { server.Shutdown(context.Background()) })

	server.trigger(true)
	select {
	case fetch := <-started:
		if !fetch {
			t.Fatal("完整刷新被错误转换为仅测速")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("首个刷新任务未启动")
	}

	for i := 0; i < 500; i++ {
		server.trigger(true)
	}
	release <- struct{}{}
	select {
	case fetch := <-started:
		if !fetch {
			t.Fatal("合并后的完整刷新被错误转换为仅测速")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("突发刷新请求被全部丢弃")
	}

	release <- struct{}{}
	select {
	case fetch := <-started:
		t.Fatalf("相同突发请求未合并，出现第三次执行: fetch=%v", fetch)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestTriggerPreservesDifferentOperations 验证完整刷新执行期间收到的“仅测速”请求不会
// 被同类刷新合并规则误删；两类意图各自最多排队一次并最终都执行。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于控制三次任务的执行边界。
//
// 返回值：无；通过后续任务同时包含 fetch=true 与 fetch=false 断言表达结果。
//
// 错误情况：任一任务未执行、任务类型被转换，或重复请求形成第四次执行时测试失败。
func TestTriggerPreservesDifferentOperations(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	server := New("127.0.0.1:0", a)
	started := make(chan bool, 4)
	release := make(chan struct{}, 3)

	// 回调只模拟耗时应用事务；监听 ctx 让测试清理可以可靠取消仍在阻塞的任务。
	server.runRefresh = func(ctx context.Context, fetch bool) error {
		select {
		case started <- fetch:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.Cleanup(func() { server.Shutdown(context.Background()) })

	server.trigger(true)
	select {
	case fetch := <-started:
		if !fetch {
			t.Fatal("首个完整刷新类型错误")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("首个完整刷新未启动")
	}

	for i := 0; i < 100; i++ {
		server.trigger(false)
		server.trigger(true)
	}
	release <- struct{}{}

	seen := map[bool]bool{}
	for i := 0; i < 2; i++ {
		select {
		case fetch := <-started:
			seen[fetch] = true
			release <- struct{}{}
		case <-time.After(2 * time.Second):
			t.Fatalf("等待第 %d 个合并任务启动超时", i+1)
		}
	}
	if !seen[true] || !seen[false] {
		t.Fatalf("不同操作未被完整保留: seen=%v", seen)
	}
	select {
	case fetch := <-started:
		t.Fatalf("重复请求未合并，出现第四次执行: fetch=%v", fetch)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestStartConfiguresIdleConnectionTimeout 验证 API 服务只回收请求之间长期闲置的
// keep-alive 连接，同时不设置会中断 /api/traffic 长连接响应的 WriteTimeout。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于启动随机端口监听并检查 http.Server 配置。
//
// 返回值：无；通过 IdleTimeout 与 WriteTimeout 断言表达结果。
//
// 错误情况：监听失败、空闲超时未配置，或误配置写超时导致流式 API 存在被切断风险时
// 测试失败。清理使用有界上下文，避免异常连接让测试退出无限等待。
func TestStartConfiguresIdleConnectionTimeout(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	server := New("127.0.0.1:0", a)
	if err := server.Start(); err != nil {
		t.Fatalf("启动 API 测试服务失败: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		server.Shutdown(ctx)
	})
	if server.srv.IdleTimeout <= 0 {
		t.Fatal("API 服务未配置空闲 keep-alive 连接回收时间")
	}
	if server.srv.WriteTimeout != 0 {
		t.Fatalf("流式 API 不应配置全局 WriteTimeout: %s", server.srv.WriteTimeout)
	}
}

// newIPv4Server 创建一个固定监听 `127.0.0.1` 的 httptest 服务。
//
// 参数：
//   - t: *testing.T，当前测试上下文；listen 或启动失败时立即终止测试。
//   - handler: http.Handler，要暴露给被测 API 的上游模拟器。
//
// 返回值：
//   - *httptest.Server：已经启动的本地 HTTP 服务，调用方负责 `Close`。
//
// 错误情况：
//   - 部分受限环境禁止 `httptest.NewServer` 默认绑定 `::1`，这里显式使用 IPv4 回环地址，
//     保证测试在沙箱和 CI 中都能稳定启动。
//   - 若 `127.0.0.1:0` 监听失败，测试会立即失败，因为后续代理逻辑已无从验证。
func newIPv4Server(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ipv4 test server: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}

// TestRuleURLContentDecodesGFWList 验证规则源内容 API 返回解码后的 AutoProxy 文本，
// 而不是把上游整体 Base64 响应直接暴露给访问规则页面。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于启动隔离上游和检查 HTTP 响应。
//
// 返回值：无；通过 HTTP 状态、Content-Type 和正文断言表达结果。
//
// 错误情况：上游或 API 调用失败、响应仍是 Base64、解码正文不完整时测试失败。
func TestRuleURLContentDecodesGFWList(t *testing.T) {
	gfw := "[AutoProxy 0.2.9]\n||example.com\n@@||direct.example\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(gfw))
	upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, encoded)
	}))
	defer upstream.Close()

	a, err := app.New(&config.Config{
		StateDir: t.TempDir(),
		RuleURLs: []config.RuleURL{{Name: "gfwlist", URL: upstream.URL}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	server := New("127.0.0.1:0", a)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/rule-urls/gfwlist/content", nil)
	request.SetPathValue("name", "gfwlist")

	server.handleRuleURLContent(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != gfw {
		t.Fatalf("body=%q, want decoded %q", recorder.Body.String(), gfw)
	}
}

// TestRuleURLContent 验证 GET /api/rule-urls/{name}/content：
// 存在的普通规则源返回可读原文（text/plain），不存在的返回 404。
func TestRuleURLContent(t *testing.T) {
	upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "DOMAIN-SUFFIX,example.com,PROXY")
	}))
	defer upstream.Close()

	a, err := app.New(&config.Config{
		StateDir: t.TempDir(),
		RuleURLs: []config.RuleURL{{Name: "src1", URL: upstream.URL}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)

	// 存在的规则源：200 + 原文
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/rule-urls/src1/content", nil)
	req.SetPathValue("name", "src1")
	srv.handleRuleURLContent(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Body.String() != "DOMAIN-SUFFIX,example.com,PROXY\n" {
		t.Errorf("body = %q", rec.Body.String())
	}

	// 不存在的规则源：404
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/rule-urls/nope/content", nil)
	req.SetPathValue("name", "nope")
	srv.handleRuleURLContent(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的规则源 status=%d, want 404", rec.Code)
	}
}

// TestSubOpsBadRequest 验证按订阅的刷新/测速接口的错误路径：
// 订阅不存在（刷新）或无节点可测（测速）时同步返回 400，而不是异步吞掉错误。
// 成功路径依赖运行中的核心，由 e2e 覆盖。
func TestSubOpsBadRequest(t *testing.T) {
	a, err := app.New(&config.Config{StateDir: t.TempDir()}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)

	for _, path := range []string{"/refresh", "/test"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/subscriptions/nope"+path, nil)
		req.SetPathValue("name", "nope")
		if strings.HasSuffix(path, "/refresh") {
			srv.handleRefreshSub(rec, req)
		} else {
			srv.handleTestSub(rec, req)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST ...%s status=%d, want 400 (body=%s)", path, rec.Code, rec.Body.String())
		}
	}
}

// TestSubscriptionUpdateAPI 验证订阅编辑接口能同步提交禁用与重命名，不依赖后台刷新。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于报告 HTTP、JSON 和内存状态断言失败。
//
// 返回值：无。
//
// 错误情况：接口非 200、返回值未规范化、旧名称仍存在或目标订阅仍启用时测试失败。
func TestSubscriptionUpdateAPI(t *testing.T) {
	a, err := app.New(&config.Config{
		Subscriptions: []config.Subscription{{Name: "old", URL: "https://example.com/sub", Type: "auto"}},
		Listen:        "127.0.0.1",
		Mode:          "rule",
		LogLevel:      "silent",
		StateDir:      t.TempDir(),
		Rules:         []string{"MATCH,PROXY"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)
	srv := New("127.0.0.1:0", a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/subscriptions/old",
		strings.NewReader(`{"name":"renamed","url":"https://example.com/sub","type":"clash","enabled":false}`),
	)
	req.SetPathValue("name", "old")
	srv.handleUpdateSub(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}
	subscriptions := a.Subscriptions()
	if len(subscriptions) != 1 || subscriptions[0].Name != "renamed" || subscriptions[0].Type != "clash" || subscriptions[0].IsEnabled() {
		t.Fatalf("订阅编辑结果异常: %+v", subscriptions)
	}
}

// TestGroupUpdateAPI 验证策略分组 PUT 接口会更新端口、策略与成员来源。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于报告 HTTP 与应用状态断言失败。
//
// 返回值：无。
//
// 错误情况：接口未返回 200，或应用层分组仍保留旧端口/旧策略时测试失败。
func TestGroupUpdateAPI(t *testing.T) {
	a, err := app.New(&config.Config{
		ManualNodes: []string{"socks5://127.0.0.1:1080#manual"},
		Listen:      "127.0.0.1",
		PortRange:   [2]int{42000, 42010},
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
		Groups: []config.NodeGroup{{
			Name: "media", Port: 43000, Type: config.GroupTypeFallback, Subscription: "manual",
		}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)
	srv := New("127.0.0.1:0", a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/groups/media",
		strings.NewReader(`{"name":"media","port":43001,"type":"load-balance","subscription":"manual"}`),
	)
	req.SetPathValue("name", "media")
	srv.handleUpdateGroup(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}
	groups := a.Groups()
	if len(groups) != 1 || groups[0].Port != 43001 || groups[0].Type != config.GroupTypeLoadBalance {
		t.Fatalf("策略分组编辑结果异常: %+v", groups)
	}
}

// TestOverviewServerTime 验证 overview 的 server_time 字段：
// 必须是服务器本地时间（RFC3339 带本地时区偏移），且与当前时刻一致。
// 防止"存成 UTC 但前端按本地显示"这类时区错配回归。
func TestOverviewServerTime(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/overview", nil)
	srv.handleOverview(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var ov struct {
		ServerTime string `json:"server_time"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	if ov.ServerTime == "" {
		t.Fatal("server_time 为空")
	}
	st, err := time.Parse(time.RFC3339, ov.ServerTime)
	if err != nil {
		t.Fatalf("server_time %q 不是合法 RFC3339: %v", ov.ServerTime, err)
	}
	if d := math.Abs(time.Since(st).Seconds()); d > 2 {
		t.Errorf("server_time 与当前时刻差 %.1fs（>2s）", d)
	}
	// 时区偏移必须与服务器本地一致（UTC 序列化会在这里暴露）
	_, wantOff := time.Now().Zone()
	_, gotOff := st.Zone()
	if gotOff != wantOff {
		t.Errorf("server_time 时区偏移 = %ds, want 本地 %ds", gotOff, wantOff)
	}
}

// TestUpdateCheckAPI 验证 overview 暴露缓存状态，设置接口可持久化关闭版本检查。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：overview 缺少 version_check、关闭接口失败或配置没有保存 false 时测试失败。
func TestUpdateCheckAPI(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	a.ConfigureUpdateCheck("dev", nil)
	srv := New("127.0.0.1:0", a)

	overview := httptest.NewRecorder()
	srv.handleOverview(overview, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if overview.Code != http.StatusOK {
		t.Fatalf("overview status=%d body=%s", overview.Code, overview.Body.String())
	}
	var payload struct {
		Version app.VersionCheckStatus `json:"version_check"`
	}
	if err := json.Unmarshal(overview.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Version.Enabled || payload.Version.State != app.VersionCheckPending {
		t.Fatalf("初始版本状态异常: %+v", payload.Version)
	}

	rec := httptest.NewRecorder()
	srv.handleSetUpdateCheck(rec, httptest.NewRequest(http.MethodPost, "/api/update-check", strings.NewReader(`{"enabled":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if a.Config().UpdateCheckEnabled() || a.VersionStatus().State != app.VersionCheckDisabled {
		t.Fatalf("关闭版本检查未生效: cfg=%v status=%+v", a.Config().CheckUpdates, a.VersionStatus())
	}
}

// TestTUNStatusAPI 验证 TUN 状态接口返回配置状态、平台和权限检测结果。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：handler 非 200、JSON 无法解析、平台缺失或错误地报告已开启时测试失败。
// 开启成功路径需要真实 TUN 权限与运行中的 mihomo，由平台手工验收覆盖。
func TestTUNStatusAPI(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tun", nil)
	srv.handleTUNStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var status app.TUNStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Enabled {
		t.Error("空配置不应报告 TUN 已开启")
	}
	if status.Platform == "" {
		t.Error("TUN 权限状态缺少 platform")
	}
}

// TestPortMappingAPI 验证端口映射开关可通过 API 热切换，并在 overview 中暴露
// 当前有效状态，供概览快捷开关与设置页共享同一事实来源。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于创建隔离应用并报告 HTTP/JSON 断言失败。
//
// 返回值：无。
//
// 错误情况：请求体无效、热更新失败、响应不是 200，或 overview 仍报告开启时测试失败。
func TestPortMappingAPI(t *testing.T) {
	a, err := app.New(&config.Config{
		Listen:   "127.0.0.1",
		Mode:     "rule",
		LogLevel: "silent",
		StateDir: t.TempDir(),
		Rules:    []string{"MATCH,PROXY"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/port-mapping", strings.NewReader(`{"enabled":false}`))
	srv.handleSetPortMapping(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}

	overview := httptest.NewRecorder()
	srv.handleOverview(overview, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	var payload struct {
		Enabled bool `json:"port_mapping_enabled"`
	}
	if err := json.Unmarshal(overview.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析 overview 失败: %v", err)
	}
	if payload.Enabled {
		t.Fatal("关闭后 overview 仍报告端口映射已开启")
	}
}

// TestSubscriptionPortMappingAPI 验证订阅级端口映射开关可经订阅编辑接口热切换，
// 并在 overview 的订阅条目中暴露；请求体省略 port_mapping 时必须保留原值，
// 避免旧客户端的启用开关操作意外重置映射开关。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于创建隔离应用并报告 HTTP/JSON 断言失败。
//
// 返回值：无。
//
// 错误情况：切换未生效、省略字段时原值被重置，或 overview 未反映最新状态时测试失败。
func TestSubscriptionPortMappingAPI(t *testing.T) {
	a, err := app.New(&config.Config{
		Listen:   "127.0.0.1",
		Mode:     "rule",
		LogLevel: "silent",
		StateDir: t.TempDir(),
		Rules:    []string{"MATCH,PROXY"},
		Subscriptions: []config.Subscription{
			{Name: "sub-a", URL: "https://example.com/sub", Type: "auto"},
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)

	portMappingOf := func() bool {
		t.Helper()
		overview := httptest.NewRecorder()
		srv.handleOverview(overview, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
		var payload struct {
			Subs []SubEntry `json:"subscriptions"`
		}
		if err := json.Unmarshal(overview.Body.Bytes(), &payload); err != nil {
			t.Fatalf("解析 overview 失败: %v", err)
		}
		if len(payload.Subs) != 1 {
			t.Fatalf("overview 订阅条目数量异常: %#v", payload.Subs)
		}
		return payload.Subs[0].PortMapping
	}

	if !portMappingOf() {
		t.Fatal("订阅缺少 port-mapping 字段时 overview 应报告默认开启")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/subscriptions/sub-a",
		strings.NewReader(`{"name":"sub-a","url":"https://example.com/sub","type":"auto","port_mapping":false}`))
	req.SetPathValue("name", "sub-a")
	srv.handleUpdateSub(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if portMappingOf() {
		t.Fatal("关闭后 overview 仍报告该订阅参与端口映射")
	}

	// 省略 port_mapping 的更新（如只改启用状态）必须保留已关闭的映射开关。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/subscriptions/sub-a",
		strings.NewReader(`{"name":"sub-a","url":"https://example.com/sub","type":"auto","enabled":true}`))
	req.SetPathValue("name", "sub-a")
	srv.handleUpdateSub(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preserve status=%d body=%s", rec.Code, rec.Body.String())
	}
	if portMappingOf() {
		t.Fatal("省略 port_mapping 的更新不应重置已关闭的订阅级映射开关")
	}
}

// TestDNSPresetAPIRejectsInvalid 验证 DNS 预设接口在进入热更新前拒绝未知枚举。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：未知 preset 未返回 HTTP 400，或错误正文未包含 dns-preset 时测试失败。
func TestDNSPresetAPIRejectsInvalid(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/dns-preset", strings.NewReader(`{"preset":"unknown"}`))
	srv.handleSetDNSPreset(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "dns-preset") {
		t.Fatalf("错误正文缺少字段名: %s", rec.Body.String())
	}
}

// TestCustomRuleEditAndMoveAPI 验证 API 只对 custom-rules 提供编辑与重排能力，
// 并在每次事务完成后返回可直接刷新界面的最新列表。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于报告 HTTP 和规则顺序断言失败。
//
// 返回值：无。
//
// 错误情况：编辑/移动接口非 200，或最终顺序、内容不符合请求时测试失败。
func TestCustomRuleEditAndMoveAPI(t *testing.T) {
	a, err := app.New(&config.Config{
		Listen:      "127.0.0.1",
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
		CustomRules: []string{"DOMAIN-SUFFIX,one.example,DIRECT", "DOMAIN-SUFFIX,two.example,PROXY"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)
	srv := New("127.0.0.1:0", a)

	edit := httptest.NewRecorder()
	editRequest := httptest.NewRequest(http.MethodPut, "/api/rules/1", strings.NewReader(`{"rule":"DOMAIN-SUFFIX,edited.example,REJECT"}`))
	editRequest.SetPathValue("index", "1")
	srv.handleUpdateRule(edit, editRequest)
	if edit.Code != http.StatusOK {
		t.Fatalf("edit status=%d body=%s", edit.Code, edit.Body.String())
	}

	move := httptest.NewRecorder()
	moveRequest := httptest.NewRequest(http.MethodPost, "/api/rules/reorder", strings.NewReader(`{"from":1,"to":0}`))
	srv.handleMoveRule(move, moveRequest)
	if move.Code != http.StatusOK {
		t.Fatalf("move status=%d body=%s", move.Code, move.Body.String())
	}
	var rules []string
	if err := json.Unmarshal(move.Body.Bytes(), &rules); err != nil {
		t.Fatalf("解析规则响应失败: %v", err)
	}
	if len(rules) != 2 || rules[0] != "DOMAIN-SUFFIX,edited.example,REJECT" {
		t.Fatalf("最终规则顺序异常: %#v", rules)
	}
}

// TestConfigExportImportAPI 验证配置备份接口默认打码，以及导入只替换磁盘配置并要求重启。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：默认导出泄露凭据、显式完整备份被打码、导入未落盘，或导入提前修改
// 当前运行配置时测试失败。
func TestConfigExportImportAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	running, err := config.Parse([]byte(`
subscriptions:
  - name: old
    url: https://example.com/sub?token=private-token
secret: private-secret
port-range: [42000, 42010]
rules:
  - MATCH,PROXY
`))
	if err != nil {
		t.Fatalf("Parse running config: %v", err)
	}
	if err := running.Save(path); err != nil {
		t.Fatalf("Save running config: %v", err)
	}
	a, err := app.New(running, path)
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)

	masked := httptest.NewRecorder()
	srv.handleExportConfig(masked, httptest.NewRequest(http.MethodGet, "/api/config/export", nil))
	if masked.Code != http.StatusOK {
		t.Fatalf("masked export status=%d body=%s", masked.Code, masked.Body.String())
	}
	if strings.Contains(masked.Body.String(), "private-token") || strings.Contains(masked.Body.String(), "private-secret") {
		t.Fatalf("默认导出泄露凭据:\n%s", masked.Body.String())
	}
	if disposition := masked.Header().Get("Content-Disposition"); !strings.Contains(disposition, "masked") {
		t.Errorf("默认导出文件名未标记 masked: %q", disposition)
	}

	backup := httptest.NewRecorder()
	srv.handleExportConfig(backup, httptest.NewRequest(http.MethodGet, "/api/config/export?mask_tokens=false", nil))
	if backup.Code != http.StatusOK {
		t.Fatalf("backup export status=%d body=%s", backup.Code, backup.Body.String())
	}
	if !strings.Contains(backup.Body.String(), "private-token") || !strings.Contains(backup.Body.String(), "private-secret") {
		t.Fatalf("完整备份缺少原始凭据:\n%s", backup.Body.String())
	}

	importedYAML := `
subscriptions:
  - name: imported
    url: https://new.example.com/sub
port-range: [43000, 43010]
rules:
  - MATCH,PROXY
`
	previewRecorder := httptest.NewRecorder()
	previewRequest := httptest.NewRequest(http.MethodPost, "/api/config/import/preview", strings.NewReader(importedYAML))
	previewRequest.Header.Set("Content-Type", "application/yaml")
	srv.handlePreviewImportConfig(previewRecorder, previewRequest)
	if previewRecorder.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewRecorder.Code, previewRecorder.Body.String())
	}
	var preview app.ConfigImportPreview
	if err := json.Unmarshal(previewRecorder.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview response: %v", err)
	}
	if preview.Digest == "" || !preview.RestartRequired || preview.Counts["subscriptions"].After != 1 {
		t.Fatalf("导入预检摘要异常: %+v", preview)
	}
	beforeConfirm, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config after preview: %v", err)
	}
	if beforeConfirm.Subscriptions[0].Name != "old" {
		t.Fatalf("预检阶段不应写入配置: %+v", beforeConfirm.Subscriptions)
	}

	imported := httptest.NewRecorder()
	importRequest := httptest.NewRequest(http.MethodPost, "/api/config/import", strings.NewReader(importedYAML))
	importRequest.Header.Set("Content-Type", "application/yaml")
	importRequest.Header.Set("X-Proxyd-Config-Digest", preview.Digest)
	srv.handleImportConfig(imported, importRequest)
	if imported.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", imported.Code, imported.Body.String())
	}
	var result struct {
		RestartRequired bool `json:"restart_required"`
	}
	if err := json.Unmarshal(imported.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	if !result.RestartRequired {
		t.Fatal("导入成功后必须明确要求重启")
	}
	onDisk, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load imported config: %v", err)
	}
	if len(onDisk.Subscriptions) != 1 || onDisk.Subscriptions[0].Name != "imported" || onDisk.PortRange[0] != 43000 {
		t.Fatalf("磁盘配置未替换: %+v", onDisk)
	}
	if len(a.Config().Subscriptions) != 1 || a.Config().Subscriptions[0].Name != "old" || a.Config().PortRange[0] != 42000 {
		t.Fatalf("导入不应在重启前修改运行配置: %+v", a.Config())
	}
}

// TestConfigImportAPIRejectsInvalid 验证无效导入不会覆盖现有配置文件。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：非法配置未返回 400，或失败后磁盘配置内容发生变化时测试失败。
func TestConfigImportAPIRejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Parse([]byte(validAPIConfigYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	a, err := app.New(cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/config/import/preview", strings.NewReader("rules: []\n"))
	request.Header.Set("Content-Type", "application/yaml")
	srv.handlePreviewImportConfig(rec, request)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid import status=%d body=%s", rec.Code, rec.Body.String())
	}
	onDisk, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load preserved config: %v", err)
	}
	if len(onDisk.Subscriptions) != 1 || onDisk.Subscriptions[0].Name != "existing" {
		t.Fatalf("无效导入覆盖了磁盘配置: %+v", onDisk.Subscriptions)
	}
}

// TestConfigImportAPIRejectsUnconfirmedContent 验证正式导入必须携带与原始 YAML 完全匹配
// 的预检摘要，防止用户确认后文件内容或浏览器状态发生变化仍被写盘。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于创建隔离配置并报告 HTTP/磁盘断言失败。
//
// 返回值：无。
//
// 错误情况：摘要不匹配仍返回成功，或现有配置文件被覆盖时测试失败。
func TestConfigImportAPIRejectsUnconfirmedContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Parse([]byte(validAPIConfigYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	a, err := app.New(cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/import", strings.NewReader(validAPIConfigYAML))
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("X-Proxyd-Config-Digest", strings.Repeat("0", 64))
	srv.handleImportConfig(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("digest mismatch status=%d body=%s", rec.Code, rec.Body.String())
	}
	onDisk, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load preserved config: %v", err)
	}
	if onDisk.Subscriptions[0].Name != "existing" {
		t.Fatalf("摘要不匹配仍覆盖了配置: %+v", onDisk.Subscriptions)
	}
}

// TestConfigImportAPIRequiresYAMLContentType 验证导入拒绝浏览器跨站简单请求可用的 text/plain。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：非 YAML Content-Type 未返回 415 时测试失败；该约束用于让浏览器先执行
// CORS 预检，避免恶意网页向本机管理 API 静默提交配置。
func TestConfigImportAPIRequiresYAMLContentType(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/import", strings.NewReader(validAPIConfigYAML))
	req.Header.Set("Content-Type", "text/plain")
	srv.handleImportConfig(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain import status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// validAPIConfigYAML 是配置导入失败测试使用的最小合法基线。
// 它单独定义在 API 测试包内，避免测试依赖 config 包的私有 fixture。
const validAPIConfigYAML = `
subscriptions:
  - name: existing
    url: https://example.com/sub
port-range: [42000, 42010]
rules:
  - MATCH,PROXY
`

// TestStaticSPA 验证内嵌 Web 控制台的静态文件服务。
//
// 参数说明：
//   - t: *testing.T，记录入口页面和 API fallback 的断言结果。
//
// 返回值说明：无；失败通过 t.Fatalf 终止当前测试。
//
// 可能的错误情况：
//   - 根路径或前端路由未返回 React 挂载点、ES module 脚本或构建资源引用。
//   - 未知 `/api/*` 被错误地回退为 SPA 页面，而不是返回 404。
func TestStaticSPA(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)

	for _, path := range []string{"/", "/settings"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		srv.handleStatic(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		// Vite 构建后的入口 HTML 有意保持精简，真正页面逻辑位于哈希静态资源中；
		// 因此验证稳定的挂载与资源契约，不能继续依赖旧单文件控制台的正文文案或体积。
		body := rec.Body.String()
		for _, marker := range []string{`id="root"`, `type="module"`, `/assets/`} {
			if !strings.Contains(body, marker) {
				t.Fatalf("GET %s 未返回有效控制台入口，缺少 %q: %s", path, marker, body)
			}
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/not-found", nil)
	srv.handleStatic(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 API status=%d, want 404", rec.Code)
	}
}

// TestLogsAPI 验证日志 API 可以按等级返回尾部日志。
//
// 这里写入一个唯一 error 行，避免全局日志环形缓冲中已有日志影响断言；
// level 过滤必须在后端完成，前端和 CLI 才能共用同一个合约。
func TestLogsAPI(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)
	line := "[error] api-log-test-unique"
	logbuf.Default.Add(line)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/logs?tail=20&level=error", nil)
	srv.handleLogs(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out LogsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, entry := range out.Entries {
		if entry.Line == line && entry.Level == "error" {
			return
		}
	}
	t.Fatalf("日志响应未包含测试行: %+v", out.Entries)
}

// TestTrafficAPIProxiesMihomoStream 验证实时流量 API 会代理 mihomo `/traffic`。
//
// 重点覆盖两件事：后端替前端附加 secret 鉴权，且不重新包装响应体；
// 这样 UI 可以按 NDJSON 流消费速率数据，同时不会暴露 external-controller secret。
func TestTrafficAPIProxiesMihomoStream(t *testing.T) {
	upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/traffic" {
			t.Errorf("upstream path = %q, want /traffic", r.URL.Path)
			http.Error(w, "bad path", http.StatusInternalServerError)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-secret" {
			t.Errorf("Authorization = %q, want Bearer test-secret", got)
			http.Error(w, "bad auth", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"up":11,"down":22,"upTotal":33,"downTotal":44}`)
	}))
	defer upstream.Close()

	a, err := app.New(&config.Config{ExternalController: upstream.URL, Secret: "test-secret"}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/traffic", nil)
	srv.handleTraffic(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"up":11,"down":22,"upTotal":33,"downTotal":44}` {
		t.Fatalf("body = %q", body)
	}
}

// TestConnectionsAPIProxiesMihomoJSON 验证 `GET /api/connections` 会代理 mihomo `/connections`。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：
//   - 若查询参数未透传、Authorization 未注入、响应 JSON/Content-Type 未保留，测试失败。
//   - 该测试同时约束接口不能要求前端直接知道 external-controller 地址或 secret。
func TestConnectionsAPIProxiesMihomoJSON(t *testing.T) {
	upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/connections" {
			t.Errorf("upstream path = %q, want /connections", r.URL.Path)
		}
		if got := r.URL.Query().Get("keyword"); got != "example.com" {
			t.Errorf("keyword = %q, want example.com", got)
		}
		if got := r.URL.Query().Get("source"); got != "rule/1" {
			t.Errorf("source = %q, want rule/1", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-secret" {
			t.Errorf("Authorization = %q, want Bearer test-secret", got)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, `{"connections":[{"id":"c1","metadata":{"host":"example.com"}}]}`)
	}))
	defer upstream.Close()

	a, err := app.New(&config.Config{ExternalController: upstream.URL, Secret: "test-secret"}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/connections?keyword=example.com&source=rule%2F1", nil)
	srv.handleConnections(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"connections":[{"id":"c1","metadata":{"host":"example.com"}}]}` {
		t.Fatalf("body = %q", body)
	}
}

// TestConnectionsAPIInjectsMemory 验证 `GET /api/connections` 会附带缓存的内存占用。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：
//   - 响应顶层缺少 `memory` 字段、数值与缓存不一致，或连接数据被改写时，测试失败。
//   - 缓存尚未采集到（0）时必须退化为原样透传，不能因此拖垮连接列表主请求。
func TestConnectionsAPIInjectsMemory(t *testing.T) {
	t.Run("memory available", func(t *testing.T) {
		upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/connections" {
				t.Errorf("unexpected upstream path %q", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			fmt.Fprint(w, `{"connections":[{"id":"c1"}],"downloadTotal":3,"uploadTotal":2}`)
		}))
		defer upstream.Close()

		a, err := app.New(&config.Config{ExternalController: upstream.URL, Secret: "test-secret"}, "")
		if err != nil {
			t.Fatal(err)
		}
		srv := New("127.0.0.1:0", a)
		srv.memoryBytes.Store(1048576)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
		srv.handleConnections(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("body is not JSON: %v", err)
		}
		if got := payload["memory"]; got != float64(1048576) {
			t.Fatalf("memory = %v, want 1048576", got)
		}
		conns, ok := payload["connections"].([]any)
		if !ok || len(conns) != 1 {
			t.Fatalf("connections = %v", payload["connections"])
		}
	})

	t.Run("memory unavailable", func(t *testing.T) {
		upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			fmt.Fprint(w, `{"connections":[]}`)
		}))
		defer upstream.Close()

		a, err := app.New(&config.Config{ExternalController: upstream.URL, Secret: "test-secret"}, "")
		if err != nil {
			t.Fatal(err)
		}
		srv := New("127.0.0.1:0", a)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
		srv.handleConnections(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if body := strings.TrimSpace(rec.Body.String()); body != `{"connections":[]}` {
			t.Fatalf("body = %q", body)
		}
	})
}

// TestMemoryWatcherCachesInuse 验证后台 watcher 能从 mihomo `/memory` 流中刷新内存缓存。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：
//   - 约定时间内缓存未更新为流中的 inuse 值时，测试失败。
//   - 流首帧 inuse 为 0 时不得覆盖缓存，避免把启动瞬间的空采样当成真实值。
func TestMemoryWatcherCachesInuse(t *testing.T) {
	upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memory" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)
		for _, v := range []uint64{0, 1048576} {
			fmt.Fprintf(w, `{"inuse":%d,"oslimit":0}`+"\n", v)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// 模拟常驻流：保持连接直到客户端（watcher 取消）断开。
		<-r.Context().Done()
	}))
	defer upstream.Close()

	a, err := app.New(&config.Config{ExternalController: upstream.URL, Secret: "test-secret"}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)
	srv.startMemoryWatcher()
	defer srv.memCancel()

	deadline := time.Now().Add(3 * time.Second)
	for srv.memoryBytes.Load() != 1048576 {
		if time.Now().After(deadline) {
			t.Fatalf("memory cache = %d, want 1048576", srv.memoryBytes.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDeleteConnectionsAPIProxiesNoContent 验证 `DELETE /api/connections` 会请求 mihomo 删除全部连接。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：
//   - 若方法、路径、鉴权头或 204 透传语义错误，测试失败。
//   - 该测试确保 proxyd 在关闭全部连接时仍由后端掌握 secret，而不是把鉴权下放到调用方。
func TestDeleteConnectionsAPIProxiesNoContent(t *testing.T) {
	upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %q, want DELETE", r.Method)
		}
		if r.URL.Path != "/connections" {
			t.Errorf("upstream path = %q, want /connections", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-secret" {
			t.Errorf("Authorization = %q, want Bearer test-secret", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	a, err := app.New(&config.Config{ExternalController: upstream.URL, Secret: "test-secret"}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/connections", nil)
	srv.handleDeleteConnections(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 body 应为空，got %q", rec.Body.String())
	}
}

// TestDeleteConnectionAPIProxiesEscapedID 验证 `DELETE /api/connections/{id}` 会把单条连接 ID 作为安全 path segment 转发。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：
//   - 若未对带 `/`、空格或 `?` 的 id 做 PathEscape，RequestURI 会被拆坏，测试失败。
//   - 若 204 状态未透传，也会导致调用方把成功删除误判为失败。
func TestDeleteConnectionAPIProxiesEscapedID(t *testing.T) {
	upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %q, want DELETE", r.Method)
		}
		if r.RequestURI != "/connections/conn%2F1%20a%3Fb" {
			t.Errorf("RequestURI = %q, want /connections/conn%%2F1%%20a%%3Fb", r.RequestURI)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-secret" {
			t.Errorf("Authorization = %q, want Bearer test-secret", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	a, err := app.New(&config.Config{ExternalController: upstream.URL, Secret: "test-secret"}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/connections/conn%2F1%20a%3Fb", nil)
	req.SetPathValue("id", "conn/1 a?b")
	srv.handleDeleteConnection(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 body 应为空，got %q", rec.Body.String())
	}
}

// TestConnectionsAPIUpstreamErrors 验证 `/api/connections` 在上游异常时返回稳定、友好的 502。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：
//   - 上游返回非 2xx 时，不应把上游正文直接透传给调用方。
//   - 上游不可达时，错误信息应稳定且不包含 secret，避免把敏感数据或环境细节泄露到前端/CLI。
func TestConnectionsAPIUpstreamErrors(t *testing.T) {
	t.Run("status", func(t *testing.T) {
		upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer test-secret" {
				t.Errorf("Authorization = %q, want Bearer test-secret", got)
			}
			http.Error(w, "upstream exploded with secret test-secret", http.StatusInternalServerError)
		}))
		defer upstream.Close()

		a, err := app.New(&config.Config{ExternalController: upstream.URL, Secret: "test-secret"}, "")
		if err != nil {
			t.Fatal(err)
		}
		srv := New("127.0.0.1:0", a)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
		srv.handleConnections(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "connections upstream returned status 500") {
			t.Fatalf("body = %q", body)
		}
		if strings.Contains(body, "test-secret") || strings.Contains(body, "upstream exploded") {
			t.Fatalf("错误响应泄露上游内容或 secret: %q", body)
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		upstreamURL := upstream.URL
		upstream.Close()

		a, err := app.New(&config.Config{ExternalController: upstreamURL, Secret: "test-secret"}, "")
		if err != nil {
			t.Fatal(err)
		}
		srv := New("127.0.0.1:0", a)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
		srv.handleConnections(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "connections upstream unavailable") {
			t.Fatalf("body = %q", body)
		}
		if strings.Contains(body, "test-secret") {
			t.Fatalf("错误响应泄露 secret: %q", body)
		}
	})
}

// TestRestartEndpointTriggersRestarterOnce 验证 POST /api/restart 先返回受理响应、
// 再异步调用注入的 restarter，且重复请求不会重复触发重启。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于启动随机端口 API 服务并断言响应与调用次数。
//
// 返回值：无；通过响应 JSON、restarter 调用记录与第二次请求的 already 标记断言表达结果。
//
// 错误情况：响应非 200、restarter 超时未被调用、重复请求再次触发重启，
// 或未注入 restarter 时未返回 503 时测试失败。
func TestRestartEndpointTriggersRestarterOnce(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	server := New("127.0.0.1:0", a)
	called := make(chan struct{}, 2)
	server.SetRestarter(func() error {
		called <- struct{}{}
		return nil
	})
	if err := server.Start(); err != nil {
		t.Fatalf("启动 API 测试服务失败: %v", err)
	}
	t.Cleanup(func() { server.Shutdown(context.Background()) })

	post := func() (int, map[string]any) {
		t.Helper()
		resp, err := http.Post("http://"+server.Addr()+"/api/restart", "", nil)
		if err != nil {
			t.Fatalf("POST /api/restart 失败: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("解析重启响应失败: %v", err)
		}
		return resp.StatusCode, body
	}

	code, body := post()
	if code != http.StatusOK || body["restarting"] != true {
		t.Fatalf("首次重启应返回 200 且 restarting=true，得到 %d %v", code, body)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("restarter 未在响应后被异步调用")
	}

	code, body = post()
	if code != http.StatusOK || body["already"] != true {
		t.Fatalf("重复重启应返回 200 且 already=true，得到 %d %v", code, body)
	}
	select {
	case <-called:
		t.Fatal("重复请求不应再次触发 restarter")
	case <-time.After(500 * time.Millisecond):
	}
}

// TestRestartEndpointWithoutRestarter 验证未注入 restarter 时 POST /api/restart 返回 503，
// 而不是静默成功让客户端误以为进程即将重启。
func TestRestartEndpointWithoutRestarter(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	server := New("127.0.0.1:0", a)
	if err := server.Start(); err != nil {
		t.Fatalf("启动 API 测试服务失败: %v", err)
	}
	t.Cleanup(func() { server.Shutdown(context.Background()) })

	resp, err := http.Post("http://"+server.Addr()+"/api/restart", "", nil)
	if err != nil {
		t.Fatalf("POST /api/restart 失败: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未注入 restarter 时应返回 503，得到 %d", resp.StatusCode)
	}
}
