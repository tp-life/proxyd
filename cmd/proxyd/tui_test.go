package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"proxyd/internal/api"
	"proxyd/internal/app"
)

// tuiRoundTripFunc 允许测试用纯内存函数替代真实 HTTP 连接。
type tuiRoundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip 实现 http.RoundTripper，并把请求交给测试函数。
//
// 参数说明：
//   - request: *http.Request，TUI 生成的只读 API 请求。
//
// 返回值说明：*http.Response 与 error，由测试函数构造。
//
// 错误情况：测试函数可返回网络错误，用于覆盖快照降级分支。
func (roundTrip tuiRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

// TestFetchTUISnapshotUsesGETOnly 验证 TUI 的完整快照周期不会产生任何写请求。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：缺少预期接口、解析失败或出现非 GET 方法时测试失败。
func TestFetchTUISnapshotUsesGETOnly(t *testing.T) {
	var mutex sync.Mutex
	methods := make([]string, 0, 11)
	oldTransport := http.DefaultTransport
	// 纯内存 transport 记录方法并返回与真实 API 同形的 JSON，避免测试依赖本机端口权限。
	http.DefaultTransport = tuiRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		mutex.Lock()
		methods = append(methods, request.Method+" "+request.URL.Path)
		mutex.Unlock()
		var payload any
		switch request.URL.Path {
		case "/api/overview":
			payload = map[string]any{"mode": "rule", "mixed_port": 41999, "nodes": []any{}, "subscriptions": []any{}}
		case "/api/connections":
			payload = map[string]any{"connections": []any{}, "memory": 1024}
		case "/api/logs":
			payload = map[string]any{"entries": []any{}}
		case "/api/remote":
			payload = map[string]any{"enabled": false, "running": false, "serve": []any{}, "allow": []any{}, "forwards": []any{}}
		case "/api/remote/remotes":
			payload = map[string]any{"remotes": []any{}}
		case "/api/rule-urls":
			payload = []any{}
		case "/api/system/status":
			payload = map[string]any{"uptime_seconds": 3600, "pending_restart": false}
		case "/api/modules":
			payload = []any{}
		case "/api/gateway":
			payload = map[string]any{"platform": "darwin", "supported": true, "devices": []any{}, "runner": "helper"}
		case "/api/desktop":
			payload = map[string]any{"services": []any{}, "connections": []any{}, "sessions": []any{}}
		case "/api/remote/audit":
			payload = map[string]any{"entries": []any{}}
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Status:     "404 Not Found",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("not found")),
				Request:    request,
			}, nil
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(encoded))),
			Request:    request,
		}, nil
	})
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
	})

	command := fetchTUISnapshotCmd(&apiClient{base: "http://proxyd.test"})
	message, ok := command().(tuiSnapshotMsg)
	if !ok {
		t.Fatalf("快照命令返回类型错误")
	}
	if message.Overview == nil || message.Connections == nil || message.Logs == nil || message.Remote == nil || message.Peers == nil || message.RuleURLs == nil {
		t.Fatalf("快照存在未加载的数据块: %#v", message)
	}
	if message.System == nil || message.Modules == nil || message.Gateway == nil || message.Desktop == nil || message.Audit == nil {
		t.Fatalf("快照存在未加载的新数据块: %#v", message)
	}
	if len(message.Warnings) != 0 {
		t.Fatalf("正常 GET 快照不应产生告警: %v", message.Warnings)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(methods) != 11 {
		t.Fatalf("请求数=%d，期望 11：%v", len(methods), methods)
	}
	for _, method := range methods {
		if !strings.HasPrefix(method, http.MethodGet+" ") {
			t.Fatalf("TUI 快照轮询发出了写请求: %s", method)
		}
	}
}

// TestTUIRenderIncludesAllSections 验证十个一级视图都能从同一快照稳定渲染。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：任一页面缺少其结构标题、View 未启用全屏模式或窗口标题残留“只读”时测试失败。
func TestTUIRenderIncludesAllSections(t *testing.T) {
	model := newTUIModel(&apiClient{})
	model.width = 120
	model.height = 36
	model.loading = false
	model.overview = &api.Overview{
		Mode:      "rule",
		MixedPort: 41999,
		PortRange: [2]int{42000, 42100},
		Nodes: []api.NodeEntry{
			{Name: "香港 01", Key: "node-key", Subscription: "airport", Type: "ss", Alive: true, Delay: 88, Port: 42000},
		},
		ManualNodes: []app.ManualNodeEntry{
			{Index: 0, Name: "办公室 SS", URL: "ss://secret@example.com:8388#office"},
		},
		Subs: []api.SubEntry{
			{Name: "airport", URL: "https://example.com/private/token", Type: "auto", Enabled: true, PortMapping: true, State: "healthy", Alive: 1, Total: 1},
		},
		Groups: []api.GroupEntry{
			{Selected: "香港 01"},
		},
		CustomRules: []string{"DOMAIN-SUFFIX,example.com,DIRECT"},
	}
	model.ruleURLs = []tuiRuleURLStat{{Name: "gfw", URL: "https://rules.example.com/secret", Count: 100}}
	model.connections = connListResponse{Connections: []connEntry{{ID: "0123456789", Rule: "MATCH"}}}
	model.peers = tuiRemotePeersResponse{Remotes: []tuiRemotePeer{{Name: "nas", Token: "tcomFw…DRYQ8u"}}}
	model.modules = []app.ModuleState{{ID: "proxy", Name: "代理", Enabled: true}}
	model.gateway = &app.GatewayOverview{RedirPort: 5300, Devices: nil}
	model.gateway.Supported = true
	model.gateway.Platform = "darwin"
	model.gateway.Enabled = true
	model.desktop = &app.DesktopStatus{
		Services: []app.DesktopServiceStatus{{Protocol: "rdp", Port: 3389, Listening: true}},
	}
	model.audit = []tuiAuditEntry{{Time: time.Now(), Action: "accept", ClientName: "nas", TargetPort: 22}}

	expected := []string{"模块状态", "手动节点", "端口映射", "选中出口", "自定义访问规则", "活动连接", "审计事件", "登记设备", "桌面服务", "运行日志"}
	for page, marker := range expected {
		model.page = tuiPage(page)
		rendered, _ := model.renderCurrentPage(118)
		if !strings.Contains(rendered, marker) {
			t.Errorf("页面 %d 缺少结构标题 %q", page+1, marker)
		}
	}
	view := model.View()
	if !view.AltScreen {
		t.Error("TUI 必须使用 alternate screen，退出后才能恢复原终端内容")
	}
	if view.WindowTitle == "" {
		t.Error("TUI 应设置可识别的终端窗口标题")
	}
	if strings.Contains(view.WindowTitle, "只读") {
		t.Errorf("交互式 TUI 的窗口标题不应再包含“只读”: %q", view.WindowTitle)
	}
}

// TestTUILayoutKeepsChromeSingleLine 验证页头与面板标题不会因样式边框或全角字符意外增高。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：页头超过一行、面板标题发生换行或完整导航超过可用宽度时测试失败。
func TestTUILayoutKeepsChromeSingleLine(t *testing.T) {
	model := newTUIModel(&apiClient{})
	model.overview = &api.Overview{MixedPort: 41999}
	model.width = 120
	header := model.renderHeader(118)
	if height := lipgloss.Height(header); height != 1 {
		t.Fatalf("页头高度=%d，期望单行；内容=%q", height, header)
	}
	tabs := model.renderTabs(118)
	if width := lipgloss.Width(tabs); width > 118 {
		t.Fatalf("导航宽度=%d，超过可用宽度 118", width)
	}
	panel := renderTUIPanel(118, "运行日志", "最近 0 条 · 每 3 秒刷新", "时间\n分隔线\n暂无数据")
	if height := lipgloss.Height(panel); height != 6 {
		t.Fatalf("面板高度=%d，期望标题、三行正文和两行边框共 6 行", height)
	}

	model.width = 40
	model.height = 18
	narrowView := model.View()
	for lineNumber, line := range strings.Split(narrowView.Content, "\n") {
		if width := lipgloss.Width(line); width > 38 {
			t.Fatalf("窄屏第 %d 行宽度=%d，超过可用宽度 38", lineNumber+1, width)
		}
	}
}

// TestTUISourceMaskAndUnicodeTruncation 验证凭据隐藏与中文宽度截断规则。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：来源摘要泄露路径/query token，或截断结果超过目标终端宽度时测试失败。
func TestTUISourceMaskAndUnicodeTruncation(t *testing.T) {
	masked := maskTUISourceURL("https://user:pass@example.com/private/token?access_token=secret")
	for _, secret := range []string{"user", "pass", "private", "token", "secret"} {
		if strings.Contains(masked, secret) {
			t.Fatalf("来源摘要泄露敏感片段 %q: %s", secret, masked)
		}
	}
	if masked != "example.com/…" {
		t.Fatalf("来源摘要=%q，期望 example.com/…", masked)
	}

	truncated := truncateTUIText("香港节点-very-long-name", 10)
	if got := lipgloss.Width(truncated); got > 10 {
		t.Fatalf("Unicode 截断宽度=%d，超过限制 10：%q", got, truncated)
	}
	if !strings.HasSuffix(truncated, "…") {
		t.Fatalf("超宽文本应带省略号: %q", truncated)
	}
}

// TestApplySnapshotPreservesSuccessfulOldBlocks 验证局部接口失败不会清空上一份成功数据。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：只更新 Overview 的快照导致连接或日志旧数据丢失时测试失败。
func TestApplySnapshotPreservesSuccessfulOldBlocks(t *testing.T) {
	model := newTUIModel(&apiClient{})
	model.connections = connListResponse{Connections: []connEntry{{ID: "keep-me"}}}
	model.logs = api.LogsResponse{}
	overview := &api.Overview{Mode: "direct", MixedPort: 41999}
	model.applySnapshot(tuiSnapshotMsg{Overview: overview, Warnings: []string{"连接数据暂不可用"}})
	if len(model.connections.Connections) != 1 || model.connections.Connections[0].ID != "keep-me" {
		t.Fatalf("局部失败清空了旧连接快照: %#v", model.connections)
	}
	if model.overview != overview {
		t.Fatal("成功的 Overview 数据块未应用")
	}
}

// tuiRecordedRequest 记录一次经过 mock transport 的请求方法与正文。
type tuiRecordedRequest struct {
	Method string
	Path   string
	Body   string
}

// installTUIRecorder 安装记录型内存 transport，返回可追加读取的请求列表。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文；清理钩子会在测试结束时还原默认 transport。
//
// 返回值说明：*[]tuiRecordedRequest，按请求发生顺序追加（本文件测试均在单协程内驱动）。
//
// 错误情况：未知 GET 路径返回空 JSON 对象，写操作路径一律 200，模拟服务端成功。
func installTUIRecorder(t *testing.T) *[]tuiRecordedRequest {
	t.Helper()
	recorded := &[]tuiRecordedRequest{}
	oldTransport := http.DefaultTransport
	http.DefaultTransport = tuiRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body []byte
		if request.Body != nil {
			body, _ = io.ReadAll(request.Body)
		}
		*recorded = append(*recorded, tuiRecordedRequest{Method: request.Method, Path: request.URL.Path, Body: string(body)})
		encoded, err := json.Marshal(tuiMockPayload(request.URL.Path))
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(encoded))),
			Request:    request,
		}, nil
	})
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
	})
	return recorded
}

// tuiMockPayload 为快照轮询涉及的 GET 端点返回同形空数据，其余路径返回空对象。
func tuiMockPayload(path string) any {
	switch path {
	case "/api/overview":
		return map[string]any{"mode": "rule", "mixed_port": 41999, "nodes": []any{}, "subscriptions": []any{}}
	case "/api/connections":
		return map[string]any{"connections": []any{}, "memory": 0}
	case "/api/logs":
		return map[string]any{"entries": []any{}}
	case "/api/remote":
		return map[string]any{"serve": []any{}, "allow": []any{}, "forwards": []any{}}
	case "/api/remote/remotes":
		return map[string]any{"remotes": []any{}}
	case "/api/rule-urls", "/api/modules":
		return []any{}
	case "/api/system/status":
		return map[string]any{"uptime_seconds": 1, "pending_restart": false}
	case "/api/gateway":
		return map[string]any{"platform": "darwin", "supported": true, "devices": []any{}}
	case "/api/desktop":
		return map[string]any{"services": []any{}, "connections": []any{}, "sessions": []any{}}
	case "/api/remote/audit":
		return map[string]any{"entries": []any{}}
	default:
		return map[string]any{}
	}
}

// tuiCountRequests 统计记录中匹配方法与路径的请求数。
func tuiCountRequests(requests []tuiRecordedRequest, method, path string) int {
	count := 0
	for _, request := range requests {
		if request.Method == method && request.Path == path {
			count++
		}
	}
	return count
}

// tuiKeyPress 构造指定按键的 KeyPressMsg；可打印字符需同时给出 Text。
func tuiKeyPress(code rune, text string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Text: text}
}

// pressTUIKey 驱动 handleKey 并把返回的 tea.Model 还原为 tuiModel。
func pressTUIKey(t *testing.T, model tuiModel, code rune, text string) (tuiModel, tea.Cmd) {
	t.Helper()
	updated, command := model.handleKey(tuiKeyPress(code, text))
	next, ok := updated.(tuiModel)
	if !ok {
		t.Fatalf("handleKey 返回类型错误: %T", updated)
	}
	return next, command
}

// newTUITestModel 创建带基础概览快照的测试模型。
func newTUITestModel() tuiModel {
	model := newTUIModel(&apiClient{base: "http://proxyd.test"})
	model.width = 120
	model.height = 36
	model.loading = false
	model.overview = &api.Overview{Mode: "rule", MixedPort: 41999, PortRange: [2]int{42000, 42100}}
	return model
}

// TestTUICursorMovesOnSelectablePages 验证可选中页的纵向导航移动光标而非滚动。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：光标未按 j/k/g/G 移动、越界未夹紧或纯滚动页语义改变时测试失败。
func TestTUICursorMovesOnSelectablePages(t *testing.T) {
	model := newTUITestModel()
	model.page = tuiPageConnections
	model.connections = connListResponse{Connections: []connEntry{{ID: "a"}, {ID: "b"}, {ID: "c"}}}

	next, _ := pressTUIKey(t, model, 'j', "j")
	if next.cursor != 1 || next.scroll != 0 {
		t.Fatalf("j 应移动光标到 1 且不改滚动: cursor=%d scroll=%d", next.cursor, next.scroll)
	}
	next, _ = pressTUIKey(t, next, 'k', "k")
	if next.cursor != 0 {
		t.Fatalf("k 应移动光标到 0: cursor=%d", next.cursor)
	}
	next, _ = pressTUIKey(t, next, 'G', "G")
	if next.cursor != 2 {
		t.Fatalf("G 应跳到尾行: cursor=%d", next.cursor)
	}
	next, _ = pressTUIKey(t, next, 'j', "j")
	if next.cursor != 2 {
		t.Fatalf("尾行继续 j 应被夹紧: cursor=%d", next.cursor)
	}
	next, _ = pressTUIKey(t, next, 'g', "g")
	if next.cursor != 0 {
		t.Fatalf("g 应回到首行: cursor=%d", next.cursor)
	}

	// 节点页统一索引：节点表 2 行 + 手动节点表 1 行，光标可越过表边界。
	next.page = tuiPageNodes
	next.overview.Nodes = []api.NodeEntry{{Name: "n1"}, {Name: "n2"}}
	next.overview.ManualNodes = []app.ManualNodeEntry{{Index: 0, Name: "m1"}}
	if rows := next.cursorRowCount(); rows != 3 {
		t.Fatalf("节点页统一索引行数=%d，期望 3", rows)
	}
	next, _ = pressTUIKey(t, next, 'G', "G")
	if next.cursor != 2 {
		t.Fatalf("G 应跳到最后一个手动节点: cursor=%d", next.cursor)
	}

	// 纯滚动页维持滚动语义，光标保持不动。
	scrollModel := newTUITestModel()
	scrollModel.height = 14
	scrollModel.page = tuiPageRules
	rules := make([]string, 0, 30)
	for index := 0; index < 30; index++ {
		rules = append(rules, "DOMAIN-SUFFIX,example.com,DIRECT")
	}
	scrollModel.overview.CustomRules = rules
	next, _ = pressTUIKey(t, scrollModel, 'j', "j")
	if next.scroll != 1 || next.cursor != 0 {
		t.Fatalf("纯滚动页 j 应滚动而非移动光标: scroll=%d cursor=%d", next.scroll, next.cursor)
	}
}

// TestTUICursorResetAndClamp 验证翻页归零与快照缩短后的光标夹紧。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：翻页后光标/滚动未归零，或列表缩短后光标越界时测试失败。
func TestTUICursorResetAndClamp(t *testing.T) {
	model := newTUITestModel()
	model.page = tuiPageSubscriptions
	model.overview.Subs = []api.SubEntry{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	next, _ := pressTUIKey(t, model, 'G', "G")
	if next.cursor != 2 {
		t.Fatalf("前置条件失败：cursor=%d，期望 2", next.cursor)
	}
	next, _ = pressTUIKey(t, next, 'l', "l")
	if next.page != tuiPagePorts || next.cursor != 0 || next.scroll != 0 {
		t.Fatalf("翻页应归零光标与滚动: page=%d cursor=%d scroll=%d", next.page, next.cursor, next.scroll)
	}

	// 快照把订阅从 3 条缩到 1 条，光标必须夹紧到新行数内。
	clamped := newTUITestModel()
	clamped.page = tuiPageSubscriptions
	clamped.overview.Subs = []api.SubEntry{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	clamped.cursor = 2
	shortened := &api.Overview{Mode: "rule", MixedPort: 41999, Subs: []api.SubEntry{{Name: "a"}}}
	clamped.applySnapshot(tuiSnapshotMsg{Overview: shortened, LoadedAt: time.Now()})
	if clamped.cursor != 0 {
		t.Fatalf("列表缩短后光标应夹紧到 0: cursor=%d", clamped.cursor)
	}
}

// TestTUIActionDispatchPostsMode 验证 m 键分派模式切换动作并完成反馈闭环。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：请求方法/路径/正文不符、结果消息未置反馈或未触发快照重取时测试失败。
func TestTUIActionDispatchPostsMode(t *testing.T) {
	recorded := installTUIRecorder(t)
	model := newTUITestModel()

	next, command := pressTUIKey(t, model, 'm', "m")
	if !next.actionBusy || command == nil {
		t.Fatalf("m 应启动模式切换动作: busy=%v cmd=%v", next.actionBusy, command == nil)
	}
	result, ok := command().(tuiActionResultMsg)
	if !ok || result.Err != nil {
		t.Fatalf("动作执行失败: %#v", result)
	}
	if tuiCountRequests(*recorded, http.MethodPost, "/api/mode") != 1 {
		t.Fatalf("应恰好调用一次 POST /api/mode: %#v", *recorded)
	}
	for _, request := range *recorded {
		if request.Path == "/api/mode" {
			var body map[string]string
			if err := json.Unmarshal([]byte(request.Body), &body); err != nil || body["mode"] != "global" {
				t.Fatalf("模式切换正文错误: body=%q err=%v", request.Body, err)
			}
		}
	}

	// 结果消息置反馈并立即触发一次快照重取。
	updated, refresh := next.Update(result)
	applied, ok := updated.(tuiModel)
	if !ok {
		t.Fatalf("Update 返回类型错误: %T", updated)
	}
	if applied.actionBusy || applied.actionNote != "代理模式 → 全局代理" || applied.actionErr {
		t.Fatalf("反馈行状态错误: busy=%v note=%q err=%v", applied.actionBusy, applied.actionNote, applied.actionErr)
	}
	if refresh == nil {
		t.Fatal("动作完成后应立即触发一次快照重取")
	}
	if _, ok := refresh().(tuiSnapshotMsg); !ok {
		t.Fatalf("动作后的重取命令应产生快照消息")
	}
}

// TestTUIConfirmFlowCloseAllConnections 验证危险操作的确认流。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：X 未进确认态、n 取消后仍发请求或 y 未执行 DELETE /api/connections 时测试失败。
func TestTUIConfirmFlowCloseAllConnections(t *testing.T) {
	recorded := installTUIRecorder(t)
	model := newTUITestModel()
	model.page = tuiPageConnections
	model.connections = connListResponse{Connections: []connEntry{{ID: "conn-1"}, {ID: "conn-2"}}}

	// X 进入确认态且不发请求；确认面板渲染确认文案。
	next, command := pressTUIKey(t, model, 'X', "X")
	if next.confirm == nil {
		t.Fatal("X 应进入确认态")
	}
	if command != nil {
		t.Fatal("确认前不应执行任何命令")
	}
	if !strings.Contains(next.View().Content, "确认") {
		t.Fatal("确认态应在屏幕中部渲染确认面板")
	}
	if tuiCountRequests(*recorded, http.MethodDelete, "/api/connections") != 0 {
		t.Fatal("确认前不应发出 DELETE 请求")
	}

	// n 取消：清除确认态，无 HTTP。
	next, command = pressTUIKey(t, next, 'n', "n")
	if next.confirm != nil || next.actionNote == "" || command != nil {
		t.Fatalf("n 应取消确认且无后续命令: confirm=%v note=%q", next.confirm != nil, next.actionNote)
	}
	if tuiCountRequests(*recorded, http.MethodDelete, "/api/connections") != 0 {
		t.Fatal("取消后不应发出 DELETE 请求")
	}

	// 再次 X 后 y：执行 DELETE /api/connections。
	next, _ = pressTUIKey(t, next, 'X', "X")
	next, command = pressTUIKey(t, next, 'y', "y")
	if next.confirm != nil || !next.actionBusy || command == nil {
		t.Fatalf("y 应离开确认态并执行动作: busy=%v", next.actionBusy)
	}
	result, ok := command().(tuiActionResultMsg)
	if !ok || result.Err != nil {
		t.Fatalf("关闭全部连接执行失败: %#v", result)
	}
	if tuiCountRequests(*recorded, http.MethodDelete, "/api/connections") != 1 {
		t.Fatalf("y 应恰好执行一次 DELETE /api/connections: %#v", *recorded)
	}
}

// TestTUISubscriptionTogglePutsFullBody 验证订阅启停 PUT 整体提交且只翻转 enabled。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：PUT 正文缺少整体字段或 enabled 未翻转时测试失败。
func TestTUISubscriptionTogglePutsFullBody(t *testing.T) {
	recorded := installTUIRecorder(t)
	model := newTUITestModel()
	model.page = tuiPageSubscriptions
	model.overview.Subs = []api.SubEntry{
		{Name: "airport", URL: "https://example.com/private/token", Type: "auto", Enabled: true, PortMapping: true},
	}

	_, command := pressTUIKey(t, model, 'e', "e")
	result, ok := command().(tuiActionResultMsg)
	if !ok || result.Err != nil {
		t.Fatalf("订阅启停执行失败: %#v", result)
	}
	if tuiCountRequests(*recorded, http.MethodPut, "/api/subscriptions/airport") != 1 {
		t.Fatalf("应恰好调用一次 PUT /api/subscriptions/airport: %#v", *recorded)
	}
	var body map[string]any
	for _, request := range *recorded {
		if request.Method == http.MethodPut {
			if err := json.Unmarshal([]byte(request.Body), &body); err != nil {
				t.Fatalf("PUT 正文不是合法 JSON: %v", err)
			}
		}
	}
	expected := map[string]any{
		"name":         "airport",
		"url":          "https://example.com/private/token",
		"type":         "auto",
		"enabled":      false,
		"port_mapping": true,
	}
	for key, want := range expected {
		if body[key] != want {
			t.Fatalf("PUT 字段 %s=%v，期望 %v（完整正文 %v）", key, body[key], want, body)
		}
	}
}

// TestTUIMainExitRunsMainNodeThenDisablesAuto 验证主出口动作的两步顺序调用。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：调用顺序、节点 key 或 main-auto 关闭值不符时测试失败。
func TestTUIMainExitRunsMainNodeThenDisablesAuto(t *testing.T) {
	recorded := installTUIRecorder(t)
	model := newTUITestModel()
	model.page = tuiPageNodes
	model.overview.Nodes = []api.NodeEntry{
		{Name: "香港 01", Key: "node-key", Subscription: "airport", Type: "ss", Alive: true, Delay: 88},
	}

	_, command := pressTUIKey(t, model, tea.KeyEnter, "")
	result, ok := command().(tuiActionResultMsg)
	if !ok || result.Err != nil {
		t.Fatalf("设置主出口执行失败: %#v", result)
	}
	posts := make([]tuiRecordedRequest, 0, 2)
	for _, request := range *recorded {
		if request.Method == http.MethodPost {
			posts = append(posts, request)
		}
	}
	if len(posts) != 2 || posts[0].Path != "/api/main-node" || posts[1].Path != "/api/main-auto" {
		t.Fatalf("主出口应先 main-node 再 main-auto: %#v", posts)
	}
	var mainNode map[string]string
	if err := json.Unmarshal([]byte(posts[0].Body), &mainNode); err != nil || mainNode["node"] != "node-key" {
		t.Fatalf("main-node 正文错误: %q", posts[0].Body)
	}
	var mainAuto map[string]bool
	if err := json.Unmarshal([]byte(posts[1].Body), &mainAuto); err != nil || mainAuto["enabled"] != false {
		t.Fatalf("main-auto 应显式关闭: %q", posts[1].Body)
	}
}

// TestTUITestingRejectsTestKey 验证测速进行中按 t 被本地拒绝并提示。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：仍发出 POST /api/test 或反馈未提示测速进行中时测试失败。
func TestTUITestingRejectsTestKey(t *testing.T) {
	recorded := installTUIRecorder(t)
	model := newTUITestModel()
	model.overview.Testing = true

	next, command := pressTUIKey(t, model, 't', "t")
	result, ok := command().(tuiActionResultMsg)
	if !ok || result.Err == nil {
		t.Fatalf("Testing 中按 t 应产生拒绝反馈: %#v", result)
	}
	if tuiCountRequests(*recorded, http.MethodPost, "/api/test") != 0 {
		t.Fatal("Testing 中不应发出 POST /api/test")
	}
	updated, _ := next.Update(result)
	applied := updated.(tuiModel)
	if !applied.actionErr || !strings.Contains(applied.actionNote, "测速进行中") {
		t.Fatalf("拒绝反馈文本错误: %q", applied.actionNote)
	}
}
