package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"charm.land/lipgloss/v2"

	"proxyd/internal/api"
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
	methods := make([]string, 0, 6)
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
	if len(message.Warnings) != 0 {
		t.Fatalf("正常 GET 快照不应产生告警: %v", message.Warnings)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(methods) != 6 {
		t.Fatalf("请求数=%d，期望 6：%v", len(methods), methods)
	}
	for _, method := range methods {
		if !strings.HasPrefix(method, http.MethodGet+" ") {
			t.Fatalf("TUI 发出了写请求: %s", method)
		}
	}
}

// TestTUIRenderIncludesAllReadOnlySections 验证八个一级视图都能从同一概览快照稳定渲染。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：任一页面缺少其结构标题或 View 未启用全屏模式时测试失败。
func TestTUIRenderIncludesAllReadOnlySections(t *testing.T) {
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
		Subs: []api.SubEntry{
			{Name: "airport", URL: "https://example.com/private/token", Type: "auto", Enabled: true, State: "healthy", Alive: 1, Total: 1},
		},
		CustomRules: []string{"DOMAIN-SUFFIX,example.com,DIRECT"},
	}
	model.ruleURLs = []tuiRuleURLStat{{Name: "gfw", URL: "https://rules.example.com/secret", Count: 100}}
	model.connections = connListResponse{Connections: []connEntry{{ID: "0123456789", Rule: "MATCH"}}}
	model.peers = tuiRemotePeersResponse{Remotes: []tuiRemotePeer{{Name: "nas", Token: "tcomFw…DRYQ8u"}}}

	expected := []string{"当前有效路由", "代理节点", "订阅资源", "代理入口", "自定义访问规则", "活动连接", "远程服务", "运行日志"}
	for page, marker := range expected {
		model.page = tuiPage(page)
		rendered := model.renderCurrentPage(118)
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
