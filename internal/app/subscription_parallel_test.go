package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
)

// TestTestSubscriptionRunsConcurrentlyAcrossSubscriptions 验证不同订阅的测速可以并行：
// 订阅 A 的测速被阻塞期间，订阅 B 的测速必须能够进入健康检测阶段，而不是等待
// 全局 refreshing 锁。同一订阅的并发操作仍由按订阅名的操作锁串行（见 lockSubOp）。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：若实现退回到全局锁串行，B 的探测请求无法在 A 阻塞期间到达目标，
// overlap 等待超时使测试失败；提交阶段节点结果回填错误也会使最终存活断言失败。
func TestTestSubscriptionRunsConcurrentlyAcrossSubscriptions(t *testing.T) {
	gate := make(chan struct{})
	overlap := make(chan struct{})
	var inFlight atomic.Int32
	var once sync.Once
	// 探测目标：收到经代理转发来的请求后阻塞在 gate 上，把测速阶段保持在进行中。
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if inFlight.Add(1) >= 2 {
			once.Do(func() { close(overlap) })
		}
		defer inFlight.Add(-1)
		<-gate
		w.WriteHeader(http.StatusNoContent)
	}))
	defer targetSrv.Close()
	// 最小 CONNECT 代理：mihomo 的 http 出站 URLTest 固定走 CONNECT 隧道，
	// 这里建立隧道后双向转发到真实目标。
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
	defer proxySrv.Close()
	proxyURL, err := url.Parse(proxySrv.URL)
	if err != nil {
		t.Fatalf("解析代理地址失败: %v", err)
	}
	proxyPort, err := strconv.Atoi(proxyURL.Port())
	if err != nil {
		t.Fatalf("解析代理端口失败: %v", err)
	}

	disabledPortMapping := false
	cfgPath := ""
	cfg := &config.Config{
		Subscriptions: []config.Subscription{
			{Name: "a", URL: "http://127.0.0.1:1/sub-a"},
			{Name: "b", URL: "http://127.0.0.1:1/sub-b"},
		},
		Listen:        "127.0.0.1",
		PortRange:     [2]int{42000, 42010},
		PortMapping:   &disabledPortMapping, // 关闭一对一 listener，避免测试绑定真实端口
		Mode:          "rule",
		LogLevel:      "silent",
		StateDir:      t.TempDir(),
		Rules:         []string{"MATCH,PROXY"},
		HealthURL:     targetSrv.URL + "/generate_204",
		HealthTimeout: config.Duration(5 * time.Second),
	}
	a, err := New(cfg, cfgPath)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)
	// server 分别写 127.0.0.1 与 localhost，保证两节点 Key 不同、不会在回填时互相覆盖。
	a.nodes = []*node.Node{
		connectProxyNode("node-a", "a", "127.0.0.1", proxyPort),
		connectProxyNode("node-b", "b", "localhost", proxyPort),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	errA := make(chan error, 1)
	go func() { errA <- a.TestSubscription(ctx, "a") }()

	// 等 A 的探测请求真正到达目标（已进入检测阶段），再启动 B。
	deadline := time.Now().Add(5 * time.Second)
	for inFlight.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if inFlight.Load() < 1 {
		t.Fatal("订阅 a 的测速未进入检测阶段")
	}
	if !a.Testing() {
		t.Fatal("测速期间 Testing() 应为 true")
	}

	errB := make(chan error, 1)
	go func() { errB <- a.TestSubscription(ctx, "b") }()

	select {
	case <-overlap:
		// B 的探测请求在 A 仍阻塞时到达目标：跨订阅并行生效。
	case <-time.After(5 * time.Second):
		close(gate)
		t.Fatal("订阅 b 的测速在订阅 a 阻塞期间未能并行执行（疑似仍被全局锁串行）")
	}
	close(gate)

	if err := <-errA; err != nil {
		for _, n := range a.Nodes() {
			t.Logf("节点 %s: alive=%v delay=%d fail=%q", n.Name, n.Alive, n.Delay, n.FailReason)
		}
		t.Fatalf("订阅 a 测速失败: %v", err)
	}
	if err := <-errB; err != nil {
		t.Fatalf("订阅 b 测速失败: %v", err)
	}
	for _, n := range a.Nodes() {
		if !n.Alive {
			t.Fatalf("节点 %s 测速结果未回填为可用: %+v", n.Name, n)
		}
	}
	if a.Testing() {
		t.Fatal("测速结束后 Testing() 应为 false")
	}
}

// connectProxyNode 构造一个指向最小 CONNECT 代理的 http 出站节点，用于并发测速测试。
func connectProxyNode(name, subscription, server string, port int) *node.Node {
	return &node.Node{
		Name:         name,
		Subscription: subscription,
		Mapping: map[string]any{
			"name":   name,
			"type":   "http",
			"server": server,
			"port":   port,
		},
	}
}
