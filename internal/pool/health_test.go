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

	"proxyd/internal/node"
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

	Check(context.Background(), []*node.Node{alive, dead, broken}, httpSrv.URL, 5*time.Second, 4)

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
	Check(context.Background(), nodes, httpSrv.URL, 5*time.Second, 0)
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
		Check(context.Background(), nodes, httpSrv.URL, 5*time.Second, concurrency)
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
	Check(context.Background(), []*node.Node{entry, exit}, httpSrv.URL, 5*time.Second, 2)
	if !exit.Alive || !entry.Alive {
		t.Fatalf("有效链路应进入候选状态: entry=%+v exit=%+v", entry, exit)
	}
	if entry.Delay != exit.Delay {
		t.Fatalf("链式候选应继承上游延迟: entry=%d exit=%d", entry.Delay, exit.Delay)
	}

	deadExit := socksNode("失效出口", 1)
	deadEntry := socksNode("失效入口", 2)
	deadEntry.Mapping["dialer-proxy"] = "失效出口"
	Check(context.Background(), []*node.Node{deadEntry, deadExit}, httpSrv.URL, time.Second, 2)
	if deadEntry.Alive || deadEntry.FailReason == "" {
		t.Fatalf("上游失效时链式节点必须不可用并给出原因: %+v", deadEntry)
	}

	cycleA := socksNode("循环 A", 2)
	cycleB := socksNode("循环 B", 3)
	cycleA.Mapping["dialer-proxy"] = "循环 B"
	cycleB.Mapping["dialer-proxy"] = "循环 A"
	Check(context.Background(), []*node.Node{cycleA, cycleB}, httpSrv.URL, time.Second, 2)
	if cycleA.Alive || cycleB.Alive {
		t.Fatalf("循环 dialer-proxy 不得进入候选: A=%+v B=%+v", cycleA, cycleB)
	}

	missing := socksNode("缺少依赖", 2)
	missing.Mapping["dialer-proxy"] = "不存在的节点"
	Check(context.Background(), []*node.Node{missing}, httpSrv.URL, time.Second, 2)
	if missing.Alive || missing.FailReason == "" {
		t.Fatalf("未知链路目标不得进入候选: %+v", missing)
	}

	groupEntry := socksNode("分组入口", 2)
	groupEntry.Mapping["dialer-proxy"] = "上游策略组"
	Check(context.Background(), []*node.Node{groupEntry}, httpSrv.URL, time.Second, 2, "上游策略组")
	if !groupEntry.Alive {
		t.Fatalf("已配置策略组应允许进入候选: %+v", groupEntry)
	}
}
