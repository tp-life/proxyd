package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	urlpkg "net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/pool"
)

// subscriptionTestNode 构造订阅设置事务测试使用的最小 socks5 节点。
//
// 参数：
//   - name: string，节点展示名与 mihomo 出站名。
//   - subscription: string，节点所属订阅名。
//   - server: string，构成稳定身份的测试服务器地址。
//
// 返回值：*node.Node，标记为健康且包含可被 mihomo 解析的最小映射。
//
// 错误情况：无；该函数只生成内存测试数据，不访问网络或文件系统。
func subscriptionTestNode(name, subscription, server string) *node.Node {
	return &node.Node{
		Name:         name,
		Subscription: subscription,
		Alive:        true,
		Delay:        10,
		Mapping: map[string]any{
			"name":   name,
			"type":   "socks5",
			"server": server,
			"port":   1080,
		},
	}
}

// TestUpdateSubscriptionDisableAndRenameCommitsAtomically 验证禁用并重命名订阅时，
// 配置、策略组引用、节点运行态和磁盘文件作为一次事务提交；其它订阅节点继续可用。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于隔离配置/状态目录并报告断言失败。
//
// 返回值：无。
//
// 错误情况：事务返回错误、旧订阅节点仍在运行态、策略组引用未改名，或磁盘状态
// 与内存状态不一致时测试失败。
func TestUpdateSubscriptionDisableAndRenameCommitsAtomically(t *testing.T) {
	disabledPortMapping := false
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Subscriptions: []config.Subscription{
			{Name: "old", URL: "https://old.example/sub", Type: "auto"},
			{Name: "other", URL: "https://other.example/sub", Type: "auto"},
		},
		Listen:      "127.0.0.1",
		PortRange:   [2]int{42000, 42010},
		PortMapping: &disabledPortMapping,
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
		Groups: []config.NodeGroup{{
			Name: "old-group", Port: 43000, Subscription: "old",
		}},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("保存初始配置失败: %v", err)
	}
	a, err := New(cfg, cfgPath)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)
	oldNode := subscriptionTestNode("旧节点", "old", "10.0.0.1")
	otherNode := subscriptionTestNode("其它节点", "other", "10.0.0.2")
	a.nodes = []*node.Node{oldNode, otherNode}
	a.assigns = []pool.Assignment{{Port: 42000, Node: oldNode}, {Port: 42001, Node: otherNode}}
	disabled := false

	next, err := a.UpdateSubscription(context.Background(), "old", config.Subscription{
		Name: "renamed", URL: "https://old.example/sub", Type: "auto", Enabled: &disabled,
	})
	if err != nil {
		t.Fatalf("禁用并重命名订阅失败: %v", err)
	}
	if next.Name != "renamed" || next.IsEnabled() {
		t.Fatalf("返回订阅状态异常: %+v", next)
	}
	if len(a.nodes) != 1 || a.nodes[0].Subscription != "other" {
		t.Fatalf("禁用订阅的节点未从运行态移除: %+v", a.nodes)
	}
	if len(a.cfg.Groups) != 1 || a.cfg.Groups[0].Subscription != "renamed" {
		t.Fatalf("策略组订阅引用未同步改名: %+v", a.cfg.Groups)
	}
	onDisk, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("读取提交后的配置失败: %v", err)
	}
	if len(onDisk.Subscriptions) != 2 || onDisk.Subscriptions[0].Name != "renamed" || onDisk.Subscriptions[0].IsEnabled() {
		t.Fatalf("磁盘订阅状态未原子提交: %+v", onDisk.Subscriptions)
	}
	if onDisk.Groups[0].Subscription != "renamed" {
		t.Fatalf("磁盘策略组引用未同步提交: %+v", onDisk.Groups)
	}
}

// TestUpdateSubscriptionEnableDoesNotFetch 验证启用订阅只提交设置，不访问远端地址；
// 尚未手动同步时允许启用，但节点集合保持为空。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于创建隔离 HTTP 服务、配置和状态目录。
//
// 返回值：无。
//
// 错误情况：编辑期间产生 HTTP 请求、启用状态未持久化，或未同步订阅凭空产生节点时测试失败。
func TestUpdateSubscriptionEnableDoesNotFetch(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("proxies: []\n"))
	}))
	t.Cleanup(server.Close)

	disabled := false
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Subscriptions: []config.Subscription{{
			Name: "manual-only", URL: server.URL, Type: "auto", Enabled: &disabled,
		}},
		Listen:    "127.0.0.1",
		PortRange: [2]int{42000, 42010},
		Mode:      "rule",
		LogLevel:  "silent",
		StateDir:  t.TempDir(),
		Rules:     []string{"MATCH,PROXY"},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("保存初始配置失败: %v", err)
	}
	a, err := New(cfg, cfgPath)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)
	enabled := true
	_, err = a.UpdateSubscription(context.Background(), "manual-only", config.Subscription{
		Name: "manual-only", URL: server.URL, Type: "auto", Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("不访问远端的启用设置应提交成功: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("启用订阅不应自动下载，实际请求次数=%d", requests.Load())
	}
	if !a.cfg.Subscriptions[0].IsEnabled() {
		t.Fatal("内存订阅没有提交启用状态")
	}
	if len(a.Nodes()) != 0 {
		t.Fatalf("未手动同步前不应产生订阅节点: %+v", a.Nodes())
	}
	onDisk, loadErr := config.Load(cfgPath)
	if loadErr != nil {
		t.Fatalf("读取提交后的配置失败: %v", loadErr)
	}
	if !onDisk.Subscriptions[0].IsEnabled() {
		t.Fatal("磁盘订阅没有提交启用状态")
	}
}

// TestHealthRefreshNeverFetchesEmptySubscription 验证仅测速路径在没有内存节点时也不会
// 回退为订阅下载；这覆盖启动、定时健康检查、模块恢复和配置变更共用的安全边界。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于创建可计数的订阅服务与隔离应用。
//
// 返回值：无。
//
// 错误情况：Refresh(false) 意外访问订阅、空节点没有返回可诊断错误，或应用创建失败时测试失败。
func TestHealthRefreshNeverFetchesEmptySubscription(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("proxies: []\n"))
	}))
	t.Cleanup(server.Close)

	a, err := New(&config.Config{
		Subscriptions: []config.Subscription{{Name: "manual-only", URL: server.URL, Type: "clash"}},
		Listen:        "127.0.0.1",
		PortRange:     [2]int{42000, 42010},
		Mode:          "rule",
		LogLevel:      "silent",
		StateDir:      t.TempDir(),
		Rules:         []string{"MATCH,PROXY"},
	}, "")
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)

	if err := a.Refresh(context.Background(), false); err == nil {
		t.Fatal("没有缓存或手动节点时，仅测速应返回明确错误")
	}
	if requests.Load() != 0 {
		t.Fatalf("仅测速不应下载订阅，实际请求次数=%d", requests.Load())
	}
}


// TestHealthRefreshRestoresCachedUserInfo 验证仅测速路径在内存没有用量时会从用量
// sidecar 恢复：进程重启后节点来自快照、尚未拉取订阅时，控制台仍应展示流量与到期信息。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于构造隔离状态目录与计数订阅服务。
//
// 返回值：无。
//
// 错误情况：Refresh(false) 访问网络、未恢复 sidecar 用量，或应用创建失败时测试失败。
func TestHealthRefreshRestoresCachedUserInfo(t *testing.T) {
	// 探测目标与最小 CONNECT 代理：mihomo 的 http 出站 URLTest 固定走 CONNECT 隧道，
	// 建立隧道后双向转发到真实目标，使缓存节点能通过健康检查。
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(targetSrv.Close)
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			_ = upstream.Close()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		client, _, err := hijacker.Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() {
			_, _ = io.Copy(upstream, client)
			_ = upstream.Close()
		}()
		go func() {
			_, _ = io.Copy(client, upstream)
			_ = client.Close()
		}()
	}))
	t.Cleanup(proxySrv.Close)
	proxyURL, err := urlpkg.Parse(proxySrv.URL)
	if err != nil {
		t.Fatalf("解析代理地址失败: %v", err)
	}

	stateDir := t.TempDir()
	cacheDir := filepath.Join(stateDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("创建缓存目录失败: %v", err)
	}
	body := fmt.Sprintf("proxies:\n  - name: n1\n    type: http\n    server: %s\n    port: %s\n", proxyURL.Hostname(), proxyURL.Port())
	if err := os.WriteFile(filepath.Join(cacheDir, "cached.cache"), []byte(body), 0o600); err != nil {
		t.Fatalf("写入订阅正文缓存失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "cached.userinfo.json"),
		[]byte(`{"upload":1024,"download":2048,"total":4096,"expire":1893456000}`), 0o600); err != nil {
		t.Fatalf("写入用量缓存失败: %v", err)
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)

	disabledPortMapping := false
	a, err := New(&config.Config{
		Subscriptions: []config.Subscription{{Name: "cached", URL: server.URL, Type: "clash"}},
		Listen:        "127.0.0.1",
		PortRange:     [2]int{42000, 42010},
		PortMapping:   &disabledPortMapping, // 关闭一对一 listener，避免测试绑定真实端口
		Mode:          "rule",
		LogLevel:      "silent",
		StateDir:      stateDir,
		Rules:         []string{"MATCH,PROXY"},
		HealthURL:     targetSrv.URL + "/generate_204",
		HealthTimeout: config.Duration(5 * time.Second),
	}, "")
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)
	// 模拟进程重启后的状态：节点已由快照恢复到内存，但内存用量为空。
	proxyPort, err := strconv.Atoi(proxyURL.Port())
	if err != nil {
		t.Fatalf("解析代理端口失败: %v", err)
	}
	a.nodes = []*node.Node{connectProxyNode("n1", "cached", proxyURL.Hostname(), proxyPort)}

	if err := a.Refresh(context.Background(), false); err != nil {
		t.Fatalf("有缓存节点时仅测速不应失败: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("仅测速不应下载订阅，实际请求次数=%d", requests.Load())
	}
	info, ok := a.SubscriptionUserInfos()["cached"]
	if !ok || info.Total != 4096 || info.Expire != 1893456000 {
		t.Fatalf("用量信息应从 sidecar 恢复: %+v", a.SubscriptionUserInfos())
	}
}
