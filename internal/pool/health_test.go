package pool

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
