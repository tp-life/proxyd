package pool

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"proxyd/internal/proxy/node"
)

// startSocks5Server 启动一个最小 SOCKS5 测试服务器（RFC1928 子集）：
// 无认证，仅支持 CONNECT(cmd=1)，支持 IPv4/域名/IPv6 三种地址类型。
// 返回监听地址端口。
func startSocks5Server(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks5 监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSocks5Conn(conn)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func handleSocks5Conn(conn net.Conn) {
	defer conn.Close()
	br := io.Reader(conn)

	// 握手：VER NMETHODS METHODS...
	head := make([]byte, 2)
	if _, err := io.ReadFull(br, head); err != nil || head[0] != 0x05 {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	// 选择无认证
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// 请求：VER CMD RSV ATYP DST...
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil || req[0] != 0x05 || req[1] != 0x01 {
		return
	}
	var host string
	switch req[3] {
	case 0x01: // IPv4
		buf := make([]byte, 4)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	case 0x03: // 域名
		lb := make([]byte, 1)
		if _, err := io.ReadFull(br, lb); err != nil {
			return
		}
		buf := make([]byte, int(lb[0]))
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		host = string(buf)
	case 0x04: // IPv6
		buf := make([]byte, 16)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	default:
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(br, pb); err != nil {
		return
	}
	target := net.JoinHostPort(host, fmt.Sprintf("%d", binary.BigEndian.Uint16(pb)))

	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		// REP=0x05 连接被拒绝
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()

	// REP=0x00 成功，BND 填 0.0.0.0:0
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	// 双向转发
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
	<-done
}

func socksNode(name string, port int) *node.Node {
	return &node.Node{
		Name: name,
		Mapping: map[string]any{
			"name":   name,
			"type":   "socks5",
			"server": "127.0.0.1",
			"port":   port,
			"udp":    true,
		},
	}
}

func TestCheck(t *testing.T) {
	// 本地 HTTP 服务，任意请求返回 204
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(httpSrv.Close)

	socksPort := startSocks5Server(t)

	alive := socksNode("alive", socksPort)
	// 指向一个（几乎必然）未监听的端口，探测应失败
	dead := socksNode("dead", 1)
	// Mapping 无法解析（缺 type）的节点
	broken := &node.Node{Name: "broken", Mapping: map[string]any{"name": "broken"}}

	Check(context.Background(), []*node.Node{alive, dead, broken}, httpSrv.URL, 5*time.Second, 4, "")

	if !alive.Alive {
		t.Fatalf("存活节点 Alive=false, Delay=%d", alive.Delay)
	}
	if alive.Delay == 0 {
		t.Errorf("存活节点 Delay=0, 期望 >0")
	}
	if dead.Alive || dead.Delay != 0 {
		t.Errorf("死亡节点 Alive=%v Delay=%d, 期望 false/0", dead.Alive, dead.Delay)
	}
	if broken.Alive || broken.Delay != 0 {
		t.Errorf("解析失败节点 Alive=%v Delay=%d, 期望 false/0", broken.Alive, broken.Delay)
	}
}

func TestCheckConcurrencyDefault(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(httpSrv.Close)

	socksPort := startSocks5Server(t)

	// concurrency<=0 走默认值，多个节点应全部检测成功
	nodes := make([]*node.Node, 0, 8)
	for i := 0; i < 8; i++ {
		nodes = append(nodes, socksNode(fmt.Sprintf("n%d", i), socksPort))
	}
	Check(context.Background(), nodes, httpSrv.URL, 5*time.Second, 0, "")
	for _, n := range nodes {
		if !n.Alive {
			t.Errorf("节点 %s Alive=false", n.Name)
		}
	}
}

// TestCheckLimitsWorkerGoroutines 验证大批节点排队时，健康检查只保留固定数量的
// worker，而不会为每个节点预先创建一个等待信号量的 goroutine。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于启动本地 SOCKS/HTTP 服务并报告资源上限断言。
//
// 返回值：无；通过 goroutine 增量和最终节点状态断言表达结果。
//
// 错误情况：若排队节点导致 goroutine 数量随节点数线性增长，或释放阻塞请求后有
// 节点未完成检测，测试失败。阈值为运行时及测试服务器预留了充足余量，避免把正常
// 的 HTTP/SOCKS 转发协程误判为泄漏。
func TestCheckLimitsWorkerGoroutines(t *testing.T) {
	const (
		concurrency = 4
		nodeCount   = 256
	)
	started := make(chan struct{}, concurrency)
	release := make(chan struct{})
	var releaseOnce sync.Once
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(httpSrv.Close)

	// 失败断言也必须先释放服务端 handler，否则 httptest.Close 会等待连接结束并让
	// 测试看似挂死。sync.Once 同时允许正常路径和 Cleanup 安全调用同一清理动作。
	unblock := func() {
		releaseOnce.Do(func() { close(release) })
	}
	t.Cleanup(unblock)

	socksPort := startSocks5Server(t)
	nodes := make([]*node.Node, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		nodes = append(nodes, socksNode(fmt.Sprintf("bounded-%d", i), socksPort))
	}

	baseline := runtime.NumGoroutine()
	done := make(chan struct{})
	go func() {
		Check(context.Background(), nodes, httpSrv.URL, 5*time.Second, concurrency, "")
		close(done)
	}()

	for i := 0; i < concurrency; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("等待第 %d 个并发探测启动超时", i+1)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if delta := runtime.NumGoroutine() - baseline; delta > 64 {
		t.Fatalf("排队节点创建了过多 goroutine: delta=%d, nodeCount=%d", delta, nodeCount)
	}

	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("释放探测请求后健康检查未及时结束")
	}
	for _, n := range nodes {
		if !n.Alive {
			t.Fatalf("节点 %s 未完成健康检查: reason=%s", n.Name, n.FailReason)
		}
	}
}

// TestProbeTimeoutRelaxesTunnelNodes 验证隧道类节点的探测超时按全局超时的
// tunnelTimeoutFactor 倍放宽，普通节点保持原值。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：隧道节点未放宽、普通节点被放大，或 nil 节点未按普通节点处理时测试失败。
func TestProbeTimeoutRelaxesTunnelNodes(t *testing.T) {
	base := 5 * time.Second
	if got := ProbeTimeout(socksNode("plain", 1080), base); got != base {
		t.Errorf("普通节点超时 = %v, 期望 %v", got, base)
	}
	tunnel := &node.Node{Mapping: map[string]any{"type": "tailscale", "auth-key": "k"}}
	if got := ProbeTimeout(tunnel, base); got != base*tunnelTimeoutFactor {
		t.Errorf("隧道节点超时 = %v, 期望 %v", got, base*tunnelTimeoutFactor)
	}
	if got := ProbeTimeout(nil, base); got != base {
		t.Errorf("nil 节点超时 = %v, 期望 %v", got, base)
	}
}

// TestCheckDefersTailscaleNetworkToMihomo 验证预健康检查只让 mihomo 解析 Tailscale
// 出站配置，不会在正式核心之外启动 tsnet 并访问控制面。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无；通过候选状态与未知延迟断言表达结果。
//
// 错误情况：若预检查错误地执行 URLTest，指向本机拒绝端口的 control-url 会导致
// 节点失败；正确实现应只完成配置解析并把节点交给正式 mihomo 运行时。
func TestCheckDefersTailscaleNetworkToMihomo(t *testing.T) {
	tailscale := &node.Node{
		Name: "mihomo-tsnet",
		Mapping: map[string]any{
			"name":        "mihomo-tsnet",
			"type":        "tailscale",
			"auth-key":    "tskey-auth-test",
			"control-url": "http://127.0.0.1:1",
		},
	}
	// stateDir 留空可让 mihomo 使用其安全默认目录；本测试只验证解析阶段不拨号，
	// 不调用进程级 SetHomeDir，避免与同包并行测试共享全局路径状态。
	Check(context.Background(), []*node.Node{tailscale}, "http://127.0.0.1:1", 50*time.Millisecond, 1, "")
	if !tailscale.Alive {
		t.Fatalf("Tailscale 配置候选不应在预检查阶段拨号: reason=%q", tailscale.FailReason)
	}
	if tailscale.Delay != 0 || tailscale.FailReason != "" {
		t.Fatalf("Tailscale 预检查状态异常: delay=%d reason=%q", tailscale.Delay, tailscale.FailReason)
	}
}

// TestCheckCancelledTailscaleCandidate 验证刷新取消后不会把尚未交给 mihomo 的
// Tailscale 配置错误标记为可用。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无；通过 Alive 与 FailReason 断言表达结果。
//
// 错误情况：取消信号未传播、候选仍被标记为可用或没有失败原因时测试失败。
func TestCheckCancelledTailscaleCandidate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tailscale := &node.Node{
		Name: "cancelled-tsnet",
		Mapping: map[string]any{
			"name":     "cancelled-tsnet",
			"type":     "tailscale",
			"auth-key": "tskey-auth-test",
		},
	}
	Check(ctx, []*node.Node{tailscale}, "https://example.com", time.Second, 1, "")
	if tailscale.Alive || tailscale.FailReason == "" {
		t.Fatalf("已取消 Tailscale 候选状态异常: %+v", tailscale)
	}
}

// TestCheckBuildsDialerProxyCandidates 验证首次加载前不会直接拨测链式节点，而是
// 根据已完成测速的上游节点建立候选状态，供应用层加载完整 mihomo 代理表。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：上游可用时链式节点未进入候选、上游不可用时仍被放行，或循环引用
// 未被拒绝时测试失败。
func TestCheckBuildsDialerProxyCandidates(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(httpSrv.Close)

	exit := socksNode("出口", startSocks5Server(t))
	entry := socksNode("入口", 2)
	entry.Mapping["dialer-proxy"] = "出口"
	Check(context.Background(), []*node.Node{entry, exit}, httpSrv.URL, 5*time.Second, 2, "")
	if !exit.Alive || !entry.Alive {
		t.Fatalf("有效链路应进入候选状态: entry=%+v exit=%+v", entry, exit)
	}
	if entry.Delay != exit.Delay {
		t.Fatalf("链式候选应继承上游延迟: entry=%d exit=%d", entry.Delay, exit.Delay)
	}

	deadExit := socksNode("失效出口", 1)
	deadEntry := socksNode("失效入口", 2)
	deadEntry.Mapping["dialer-proxy"] = "失效出口"
	Check(context.Background(), []*node.Node{deadEntry, deadExit}, httpSrv.URL, time.Second, 2, "")
	if deadEntry.Alive || deadEntry.FailReason == "" {
		t.Fatalf("上游失效时链式节点必须不可用并给出原因: %+v", deadEntry)
	}

	cycleA := socksNode("循环 A", 2)
	cycleB := socksNode("循环 B", 3)
	cycleA.Mapping["dialer-proxy"] = "循环 B"
	cycleB.Mapping["dialer-proxy"] = "循环 A"
	Check(context.Background(), []*node.Node{cycleA, cycleB}, httpSrv.URL, time.Second, 2, "")
	if cycleA.Alive || cycleB.Alive {
		t.Fatalf("循环 dialer-proxy 不得进入候选: A=%+v B=%+v", cycleA, cycleB)
	}

	missing := socksNode("缺少依赖", 2)
	missing.Mapping["dialer-proxy"] = "不存在的节点"
	Check(context.Background(), []*node.Node{missing}, httpSrv.URL, time.Second, 2, "")
	if missing.Alive || missing.FailReason == "" {
		t.Fatalf("未知链路目标不得进入候选: %+v", missing)
	}

	groupEntry := socksNode("分组入口", 2)
	groupEntry.Mapping["dialer-proxy"] = "上游策略组"
	Check(context.Background(), []*node.Node{groupEntry}, httpSrv.URL, time.Second, 2, "", "上游策略组")
	if !groupEntry.Alive {
		t.Fatalf("已配置策略组应允许进入候选: %+v", groupEntry)
	}
}

// TestCheckPublishesIncrementalDisplay 验证逐节点测速进度：
//
//   - 节点被投递探测前先发布「测速中」展示行，行内保留本轮开始前的存活状态与延迟，
//     即使权威字段已被 checkOne 清零；
//   - 排队等待的节点同样是「测速中」，结果落定的节点立即换成最终展示行；
//   - 读侧并发读取展示行，配合 -race 覆盖概览读取与 worker 写回不产生数据竞争。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用闸门把第一个节点保持在探测中。
//
// 返回值：无；通过展示行与权威字段的对比断言表达结果。
//
// 错误情况：节点被探测前未标记、测速中丢失轮前稳定值、或结束后仍显示「测速中」时失败。
func TestCheckPublishesIncrementalDisplay(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	started := make(chan struct{}, 1)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(httpSrv.Close)

	first := socksNode("first", startSocks5Server(t))
	// 本轮开始前的稳定值：标记后仍应可读，权威字段则在探测开始时被清零。
	// 生产路径上节点诞生即发布展示行（见 subscribe.newNode），这里显式补上。
	first.Alive = true
	first.Delay = 123
	first.PublishResult()
	second := socksNode("second", first.Mapping["port"].(int))
	second.PublishResult()

	nodes := []*node.Node{first, second}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if d := first.Display(); d.Testing && (!d.Alive || d.Delay != 123) {
				t.Errorf("测速中的展示行不再是本轮开始前的稳定值: %+v", d)
				return
			}
		}
	}()

	done := make(chan struct{})
	// 并发度 1：第二个节点在第一个探测结束前保持排队，应一直是「测速中」。
	go func() {
		Check(context.Background(), nodes, httpSrv.URL, 5*time.Second, 1, "")
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("等待探测进入目标服务超时")
	}
	if d := first.Display(); !d.Testing || !d.Alive || d.Delay != 123 {
		t.Fatalf("探测中的节点应发布轮前稳定值 + 测速中: %+v", d)
	}
	if first.Alive || first.Delay != 0 {
		t.Fatalf("权威字段应已按本轮探测清零: alive=%v delay=%d", first.Alive, first.Delay)
	}
	if d := second.Display(); !d.Testing {
		t.Fatalf("排队中的节点应显示「测速中」: %+v", d)
	}

	unblock()
	<-done
	<-readerDone
	for _, n := range nodes {
		d := n.Display()
		if d.Testing {
			t.Fatalf("测速结束后不应残留「测速中」: %s=%+v", n.Name, d)
		}
		// 展示行必须落到最终权威字段（本机探测的延迟可能取整为 0，不比较具体数值）。
		if d.Alive != n.Alive || d.Delay != n.Delay || d.FailReason != n.FailReason {
			t.Fatalf("展示行应等于权威字段: %s=%+v node=%+v", n.Name, d, n)
		}
	}
	if d := first.Display(); !d.Alive {
		t.Fatalf("完成节点应发布最终结果: %+v", d)
	}
}

// TestCheckOnResultPerNode 验证结果回调只在单个节点本轮结果落定后触发。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无；通过回调次数、回调时节点状态与链式候选残留标记断言表达结果。
//
// 错误情况：回调漏触发、重复触发、在节点仍是「测速中」时触发，或链式候选被当作
// 已完成结果发布时失败。
func TestCheckOnResultPerNode(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(httpSrv.Close)

	exit := socksNode("出口", startSocks5Server(t))
	entry := socksNode("入口", 2)
	entry.Mapping["dialer-proxy"] = "出口"

	var mu sync.Mutex
	seen := map[string]int{}
	CheckWithOptions(context.Background(), []*node.Node{entry, exit}, httpSrv.URL, 5*time.Second, 2, "",
		CheckOptions{OnResult: func(n *node.Node) {
			mu.Lock()
			seen[n.Name]++
			mu.Unlock()
			if d := n.Display(); d.Testing {
				t.Errorf("回调时节点结果应已落定: %s=%+v", n.Name, d)
			}
		}})

	mu.Lock()
	defer mu.Unlock()
	if seen["出口"] != 1 {
		t.Fatalf("普通节点的结果回调应恰好触发一次: %v", seen)
	}
	if seen["入口"] != 0 {
		t.Fatalf("链式候选不在预检查阶段出结果，不应触发回调: %v", seen)
	}
	if d := entry.Display(); !d.Testing {
		t.Fatalf("链式候选的真实结果由应用层探测，Check 返回后应保持「测速中」: %+v", d)
	}
}
