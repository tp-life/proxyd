package subscribe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
)

// mkNode 构造测试节点。
func mkNode(name, sub, typ, server string, port int, credKey, cred string) *node.Node {
	m := map[string]any{
		"name":   name,
		"type":   typ,
		"server": server,
		"port":   port,
	}
	if credKey != "" {
		m[credKey] = cred
	}
	return &node.Node{Name: name, Subscription: sub, Mapping: m}
}

func TestMergeDedup(t *testing.T) {
	a := mkNode("节点1", "subA", "ss", "1.1.1.1", 1000, "password", "pw")
	dup := mkNode("节点1-别名", "subB", "ss", "1.1.1.1", 1000, "password", "pw") // Key 相同
	other := mkNode("节点2", "subB", "ss", "1.1.1.1", 1001, "password", "pw")

	out := Merge(map[string][]*node.Node{
		"subA": {a},
		"subB": {dup, other},
	}, nil)
	if len(out) != 2 {
		t.Fatalf("期望去重后 2 个节点，得到 %d", len(out))
	}
	if out[0].Name != "节点1" || out[0].Subscription != "subA" {
		t.Errorf("应保留先出现的节点: %+v", out[0])
	}
}

// TestFetchAllSkipsDisabledSubscription 验证禁用订阅不会进入拉取或合并流水线。
// 测试使用无法解析的 URL；如果实现误发请求，对应错误槽位就会出现错误。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于报告节点与错误槽位断言失败。
//
// 返回值：无。
//
// 错误情况：禁用订阅仍产生拉取错误或节点，或静态手动节点被连带过滤时测试失败。
func TestFetchAllSkipsDisabledSubscription(t *testing.T) {
	disabled := false
	subs := []config.Subscription{{
		Name:    "disabled",
		URL:     "://invalid",
		Type:    "auto",
		Enabled: &disabled,
	}}
	manual := mkNode("手动节点", ManualSubscription, "socks5", "127.0.0.1", 1080, "", "")
	nodes, _, errs := FetchAllWithInfoAndFilters(
		context.Background(),
		subs,
		t.TempDir(),
		nil,
		nil,
		map[string][]*node.Node{ManualSubscription: {manual}},
	)
	if len(errs) != 1 || errs[0] != nil {
		t.Fatalf("禁用订阅不应发起拉取或产生错误: %#v", errs)
	}
	if len(nodes) != 1 || nodes[0].Name != "手动节点" || nodes[0].Subscription != ManualSubscription {
		t.Fatalf("禁用订阅不应影响手动节点: %#v", nodes)
	}
}

// TestFetchAllLimitsConcurrentSources 验证订阅数量很大时只启动固定数量的上游拉取，
// 同时保持所有订阅最终都会被处理且错误切片仍与输入一一对应。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于启动受控 HTTP 上游并检查并发峰值。
//
// 返回值：无；通过最大在途请求数、节点数和错误槽位断言表达结果。
//
// 错误情况：上游并发超过限制、任务队列丢失订阅、解析失败或错误槽位错位时测试失败。
// 测试响应为互不重复的 Shadowsocks 分享链接，避免节点去重掩盖漏拉取问题。
func TestFetchAllLimitsConcurrentSources(t *testing.T) {
	const sourceCount = maxConcurrentSubscriptionFetches + 8
	var active atomic.Int32
	var peak atomic.Int32
	started := make(chan struct{}, sourceCount)
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for observed := peak.Load(); current > observed && !peak.CompareAndSwap(observed, current); observed = peak.Load() {
		}
		started <- struct{}{}
		select {
		case <-release:
			var index int
			_, _ = fmt.Sscanf(r.URL.Path, "/%d", &index)
			_, _ = fmt.Fprintf(w, "ss://aes-128-gcm:password@127.0.0.1:%d#node-%d\n", 10000+index, index)
		case <-r.Context().Done():
			return
		}
	}))
	t.Cleanup(server.Close)

	// 无论断言从哪个分支退出，都先解除 handler 阻塞，避免 HTTP 服务清理等待在途请求。
	unblock := func() {
		releaseOnce.Do(func() { close(release) })
	}
	t.Cleanup(unblock)

	subs := make([]config.Subscription, 0, sourceCount)
	for i := 0; i < sourceCount; i++ {
		subs = append(subs, config.Subscription{
			Name: fmt.Sprintf("sub-%d", i),
			URL:  fmt.Sprintf("%s/%d", server.URL, i),
			Type: "share",
		})
	}
	type fetchOutcome struct {
		nodes []*node.Node
		errs  []error
	}
	stateDir := t.TempDir()
	outcome := make(chan fetchOutcome, 1)
	go func() {
		nodes, _, errs := FetchAllWithInfoAndFilters(context.Background(), subs, stateDir, nil, nil)
		outcome <- fetchOutcome{nodes: nodes, errs: errs}
	}()

	for i := 0; i < maxConcurrentSubscriptionFetches; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("等待第 %d 个订阅拉取启动超时", i+1)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := peak.Load(); got > maxConcurrentSubscriptionFetches {
		t.Fatalf("订阅拉取并发峰值=%d，期望不超过 %d", got, maxConcurrentSubscriptionFetches)
	}

	unblock()
	select {
	case got := <-outcome:
		if len(got.nodes) != sourceCount {
			t.Fatalf("拉取完成节点数=%d，期望 %d", len(got.nodes), sourceCount)
		}
		if len(got.errs) != sourceCount {
			t.Fatalf("错误槽位数=%d，期望 %d", len(got.errs), sourceCount)
		}
		for i, err := range got.errs {
			if err != nil {
				t.Fatalf("订阅 %d 意外失败: %v", i, err)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("解除上游阻塞后订阅批量拉取未及时结束")
	}
}

func TestMergeExclude(t *testing.T) {
	re := regexp.MustCompile(`官网|套餐|剩余流量`)
	out := Merge(map[string][]*node.Node{
		"subA": {
			mkNode("香港01", "subA", "ss", "1.1.1.1", 1000, "password", "pw"),
			mkNode("官网地址", "subA", "ss", "2.2.2.2", 2000, "password", "pw2"),
			mkNode("剩余流量 100G", "subA", "ss", "3.3.3.3", 3000, "password", "pw3"),
		},
	}, re)
	if len(out) != 1 || out[0].Name != "香港01" {
		t.Fatalf("exclude 过滤不正确: %+v", out)
	}
}

// TestMergeIncludeExclude 验证 include 白名单与 exclude 黑名单的组合优先级。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：未命中 include 的节点被保留，或同时命中 exclude 的节点未被剔除时失败。
func TestMergeIncludeExclude(t *testing.T) {
	includeRe := regexp.MustCompile(`香港|日本`)
	excludeRe := regexp.MustCompile(`日本`)
	out := MergeFiltered(map[string][]*node.Node{
		"subA": {
			mkNode("香港01", "subA", "ss", "1.1.1.1", 1000, "password", "pw1"),
			mkNode("日本01", "subA", "ss", "2.2.2.2", 2000, "password", "pw2"),
			mkNode("美国01", "subA", "ss", "3.3.3.3", 3000, "password", "pw3"),
		},
	}, includeRe, excludeRe)
	if len(out) != 1 || out[0].Name != "香港01" {
		t.Fatalf("include/exclude 组合过滤不正确: %+v", out)
	}
}

func TestMergeUniqueName(t *testing.T) {
	out := Merge(map[string][]*node.Node{
		"subA": {mkNode("同名", "subA", "ss", "1.1.1.1", 1000, "password", "pw1")},
		"subB": {
			mkNode("同名", "subB", "ss", "2.2.2.2", 2000, "password", "pw2"),
			mkNode("同名", "subB", "ss", "3.3.3.3", 3000, "password", "pw3"),
		},
	}, nil)
	if len(out) != 3 {
		t.Fatalf("期望 3 个节点，得到 %d", len(out))
	}
	want := []string{"同名", "同名 (subB)", "同名 (subB) 2"}
	for i, w := range want {
		if out[i].Name != w {
			t.Errorf("节点 %d 名字期望 %q，得到 %q", i, w, out[i].Name)
		}
		if out[i].Mapping["name"] != out[i].Name {
			t.Errorf("节点 %d Mapping[name] 未同步: %v", i, out[i].Mapping["name"])
		}
	}
	if out[1].Subscription != "subB" || out[2].Subscription != "subB" {
		t.Errorf("Subscription 未设置: %+v", out)
	}
}

// TestMergeRewritesDialerProxyAfterNameCollision 验证节点全局重命名后，链式代理引用
// 仍然指向同一订阅中的出口节点，而不会误连到另一个订阅的同名节点。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：入口节点或被引用节点重命名后，dialer-proxy 未同步更新时测试失败；
// 这类错误会让 mihomo 在配置自检阶段报告引用不存在，导致整轮刷新无法应用。
func TestMergeRewritesDialerProxyAfterNameCollision(t *testing.T) {
	first := mkNode("出口", "subA", "socks5", "1.1.1.1", 1000, "password", "pw1")
	exit := mkNode("出口", "subB", "socks5", "2.2.2.2", 2000, "password", "pw2")
	entry := mkNode("入口", "subB", "socks5", "3.3.3.3", 3000, "password", "pw3")
	entry.Mapping["dialer-proxy"] = "出口"

	out := Merge(map[string][]*node.Node{
		"subA": {first},
		"subB": {exit, entry},
	}, nil)
	if len(out) != 3 {
		t.Fatalf("期望保留 3 个节点，得到 %d", len(out))
	}
	if exit.Name != "出口 (subB)" {
		t.Fatalf("被引用节点未按预期重命名: %q", exit.Name)
	}
	if got := entry.Mapping["dialer-proxy"]; got != exit.Name {
		t.Fatalf("dialer-proxy = %v，期望同步为 %q", got, exit.Name)
	}
}

// TestMergeRewritesDialerProxyFromDeduplicatedAlias 验证被节点身份去重掉的别名仍可作为
// dialer-proxy 引用，并会解析到实际保留的等价节点。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：订阅内同一服务器存在多个名称时，链式节点引用被丢弃别名却无法重写，
// 会导致后续健康检查误报依赖不存在，此时测试失败。
func TestMergeRewritesDialerProxyFromDeduplicatedAlias(t *testing.T) {
	exit := mkNode("出口主名", "subA", "socks5", "1.1.1.1", 1000, "password", "pw")
	alias := mkNode("出口别名", "subA", "socks5", "1.1.1.1", 1000, "password", "pw")
	entry := mkNode("入口", "subA", "socks5", "2.2.2.2", 2000, "password", "entry-pw")
	entry.Mapping["dialer-proxy"] = alias.Name

	out := Merge(map[string][]*node.Node{"subA": {exit, alias, entry}}, nil)
	if len(out) != 2 {
		t.Fatalf("等价出口应去重，实际保留 %d 个节点", len(out))
	}
	if got := entry.Mapping["dialer-proxy"]; got != exit.Name {
		t.Fatalf("被去重别名应重写到保留节点: got=%v want=%q", got, exit.Name)
	}
}
