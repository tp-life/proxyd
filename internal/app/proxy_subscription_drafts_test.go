package app

// 订阅刷新草稿应用层测试：验证预览隔离、基线失效与生命周期清理。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
)

// TestCreateSubscriptionDraftDoesNotMutateRuntimeOrCache 验证生成草稿只读运行态，
// 且预览后发生的健康状态变化会让确认请求失效。
//
// 参数：t 为 Go 测试上下文。
// 返回值：无。
// 错误情况：运行节点被 Merge 原地改写、缓存提前落盘、差异错误，或旧草稿仍可应用时失败。
func TestCreateSubscriptionDraftDoesNotMutateRuntimeOrCache(t *testing.T) {
	const body = `proxies:
  - name: 新名称
    type: http
    server: 127.0.0.1
    port: 1
`
	subscriptionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer subscriptionServer.Close()

	enabled, portMapping := true, false
	stateDir := t.TempDir()
	a, err := New(&config.Config{
		Subscriptions: []config.Subscription{{
			Name: "主订阅", URL: subscriptionServer.URL, Type: "clash", Enabled: &enabled,
		}},
		Listen: "127.0.0.1", PortRange: [2]int{42000, 42010}, PortMapping: &portMapping,
		Mode: "rule", LogLevel: "silent", StateDir: stateDir, Rules: []string{"MATCH,PROXY"},
		HealthURL: subscriptionServer.URL + "/health", HealthTimeout: config.Duration(20 * time.Millisecond),
	}, "")
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)
	current := &node.Node{
		Name: "旧名称", Subscription: "主订阅", Alive: true, Delay: 15,
		Mapping: map[string]any{"name": "旧名称", "type": "http", "server": "127.0.0.1", "port": 1},
	}
	a.nodes = []*node.Node{current}

	draft, err := a.CreateSubscriptionDraft(context.Background(), "主订阅")
	if err != nil {
		t.Fatalf("创建刷新草稿失败: %v", err)
	}
	if current.Name != "旧名称" || current.Mapping["name"] != "旧名称" || !current.Alive {
		t.Fatalf("预览修改了运行节点: %+v", current)
	}
	if draft.Diff.Updated != 1 || draft.Diff.BeforeAlive != 1 || draft.Diff.AfterAlive != 0 {
		t.Fatalf("草稿差异异常: %+v", draft.Diff)
	}
	cacheFiles, err := filepath.Glob(filepath.Join(stateDir, "cache", "*.cache"))
	if err != nil {
		t.Fatalf("检查缓存路径失败: %v", err)
	}
	if len(cacheFiles) != 0 {
		t.Fatalf("预览阶段提前写入缓存: %v", cacheFiles)
	}

	// 模拟预览后另一轮测速提交。候选节点仍带旧健康状态，必须通过完整基线拒绝覆盖。
	current.Delay = 99
	if err := a.ApplySubscriptionDraft(context.Background(), "主订阅", draft.ID); !errors.Is(err, ErrSubscriptionDraftStale) {
		t.Fatalf("运行基线变化后应拒绝草稿，得到: %v", err)
	}
	if _, err := a.SubscriptionDraft("主订阅"); !errors.Is(err, ErrSubscriptionDraftNotFound) {
		t.Fatalf("失效草稿应被清理，得到: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "cache")); err == nil {
		// pool.Check 可能创建其它状态目录，但不应因草稿失效提交订阅 cache 目录。
		if files, _ := filepath.Glob(filepath.Join(stateDir, "cache", "*.cache")); len(files) != 0 {
			t.Fatalf("失效草稿不应提交缓存: %v", files)
		}
	}
}

// TestSubscriptionDraftLifecycle 验证草稿 ID 防误删和过期清理语义。
//
// 参数：t 为 Go 测试上下文。
// 返回值：无。
// 错误情况：错误 ID 能删除草稿，或过期草稿仍可读取时测试失败。
func TestSubscriptionDraftLifecycle(t *testing.T) {
	a := &App{subscriptionDrafts: map[string]*subscriptionDraftState{
		"demo": {view: SubscriptionDraftView{ID: "current", Name: "demo", ExpiresAt: time.Now().Add(time.Minute)}},
	}}
	if err := a.DiscardSubscriptionDraft("demo", "old"); !errors.Is(err, ErrSubscriptionDraftNotFound) {
		t.Fatalf("旧 ID 应返回不存在，得到: %v", err)
	}
	if _, err := a.SubscriptionDraft("demo"); err != nil {
		t.Fatalf("旧 ID 不应误删当前草稿: %v", err)
	}
	a.mu.Lock()
	a.subscriptionDrafts["demo"].view.ExpiresAt = time.Now().Add(-time.Second)
	a.mu.Unlock()
	if _, err := a.SubscriptionDraft("demo"); !errors.Is(err, ErrSubscriptionDraftExpired) {
		t.Fatalf("过期草稿应返回 expired，得到: %v", err)
	}
}

// TestApplySubscriptionDraftCommitsRuntimeAndCache 验证确认动作先应用完整运行态，成功后
// 才保存已接受订阅缓存并清除内存草稿。
//
// 参数：t 为 Go 测试上下文。
// 返回值：无。
// 错误情况：mihomo 热更新失败、候选节点未生效、草稿残留或正文缓存未提交时测试失败。
func TestApplySubscriptionDraftCommitsRuntimeAndCache(t *testing.T) {
	const body = `proxies:
  - name: 已确认节点
    type: socks5
    server: 127.0.0.1
    port: 1080
`
	subscriptionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer subscriptionServer.Close()

	a := newSystemProxyTestApp(t, "")
	enabled, portMapping := true, false
	a.mu.Lock()
	a.cfg.Subscriptions = []config.Subscription{{
		Name: "确认订阅", URL: subscriptionServer.URL, Type: "clash", Enabled: &enabled,
	}}
	a.cfg.PortMapping = &portMapping
	a.cfg.HealthURL = subscriptionServer.URL + "/health"
	a.cfg.HealthTimeout = config.Duration(20 * time.Millisecond)
	a.mu.Unlock()

	draft, err := a.CreateSubscriptionDraft(context.Background(), "确认订阅")
	if err != nil {
		t.Fatalf("创建草稿失败: %v", err)
	}
	// 本测试关注确认事务而非真实代理连通性；草稿生成已经验证实际探测会标记不可用，
	// 此处模拟用户确认前候选已通过健康探测，避免依赖公网或额外代理服务器。
	a.mu.Lock()
	a.subscriptionDrafts["确认订阅"].nodes[0].Alive = true
	a.subscriptionDrafts["确认订阅"].nodes[0].Delay = 12
	a.mu.Unlock()
	if err := a.ApplySubscriptionDraft(context.Background(), "确认订阅", draft.ID); err != nil {
		t.Fatalf("应用草稿失败: %v", err)
	}
	nodes := a.Nodes()
	if len(nodes) != 1 || nodes[0].Name != "已确认节点" || !nodes[0].Alive {
		t.Fatalf("确认后运行节点异常: %+v", nodes)
	}
	if _, err := a.SubscriptionDraft("确认订阅"); !errors.Is(err, ErrSubscriptionDraftNotFound) {
		t.Fatalf("确认后草稿未清除: %v", err)
	}
	cacheFiles, err := filepath.Glob(filepath.Join(a.cfg.StateDir, "cache", "*.cache"))
	if err != nil || len(cacheFiles) != 1 {
		t.Fatalf("确认后缓存文件异常: files=%v err=%v", cacheFiles, err)
	}
}

// TestApplySubscriptionDraftRollsBackRuntimeOnReloadFailure 验证候选配置无法被 mihomo
// 接受时，应用层恢复确认前的节点集合与可生成运行配置，并保留草稿供诊断或重试。
//
// 参数：t 为 Go 测试上下文。
// 返回值：无。
// 错误情况：无效协议被错误接受、旧节点没有恢复、回滚也失败或草稿被提前删除时测试失败。
func TestApplySubscriptionDraftRollsBackRuntimeOnReloadFailure(t *testing.T) {
	a := newSystemProxyTestApp(t, "")
	enabled, portMapping := true, false
	target := config.Subscription{Name: "回滚订阅", URL: "https://example.com/sub", Type: "clash", Enabled: &enabled}
	current := subscriptionTestNode("当前节点", target.Name, "127.0.0.1")
	invalid := &node.Node{
		Name: "无效候选", Subscription: target.Name, Alive: true,
		Mapping: map[string]any{"name": "无效候选", "type": "not-a-real-proxy", "server": "127.0.0.1", "port": 1080},
	}
	a.mu.Lock()
	a.cfg.Subscriptions = []config.Subscription{target}
	a.cfg.PortMapping = &portMapping
	a.nodes = []*node.Node{current}
	a.mu.Unlock()
	view := SubscriptionDraftView{ID: "rollback", Name: target.Name, ExpiresAt: time.Now().Add(time.Minute)}
	baseline := a.subscriptionDraftBaseline()
	a.mu.Lock()
	a.subscriptionDrafts[target.Name] = &subscriptionDraftState{
		view: view, target: target, baseline: baseline, nodes: []*node.Node{invalid},
	}
	a.mu.Unlock()

	if err := a.ApplySubscriptionDraft(context.Background(), target.Name, view.ID); err == nil {
		t.Fatal("无效候选配置应触发热更新失败")
	}
	nodes := a.Nodes()
	if len(nodes) != 1 || nodes[0] != current || nodes[0].Name != "当前节点" {
		t.Fatalf("热更新失败后未恢复旧节点: %+v", nodes)
	}
	if _, err := a.SubscriptionDraft(target.Name); err != nil {
		t.Fatalf("热更新失败后应保留草稿供诊断: %v", err)
	}
}
