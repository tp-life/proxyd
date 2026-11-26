package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"proxyd/internal/remote"
)

// TestIsSafeRelayName 验证拼入 ProxyCommand 的远端名只放行安全字符。
func TestIsSafeRelayName(t *testing.T) {
	for _, ok := range []string{"mac", "nas-01", "a.b_c"} {
		if !isSafeRelayName(ok) {
			t.Errorf("%q 应放行", ok)
		}
	}
	for _, bad := range []string{"", "a b", "x;rm", "$(id)", "a'b", "名"} {
		if isSafeRelayName(bad) {
			t.Errorf("%q 应拒绝", bad)
		}
	}
}

// TestOpenSessionPipeProtocol 用最小假守护进程验证升级请求格式、凭据头与升级后字节流。
func TestOpenSessionPipeProtocol(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		if req.URL.Path != remote.SessionPipePath || req.URL.Query().Get("name") != "mac" ||
			req.URL.Query().Get("port") != "22" || req.Header.Get(remote.SessionHeader) != "s3cret" ||
			req.Header.Get("Upgrade") != remote.SessionUpgrade {
			_, _ = io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 3\r\n\r\nbad")
			return
		}
		// 101 与首批上游数据同包写出，验证客户端从缓冲读端取数据而不丢字节。
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: proxyd-pipe\r\nConnection: Upgrade\r\n\r\nSSH-2.0-test\r\n")
		line, _ := reader.ReadString('\n')
		_, _ = io.WriteString(conn, "echo:"+line)
	}()

	conn, reader, err := openSessionPipe(ln.Addr().String(), "s3cret", "mac", 22)
	if err != nil {
		t.Fatalf("openSessionPipe: %v", err)
	}
	defer conn.Close()
	br := bufio.NewReader(reader)
	banner, _ := br.ReadString('\n')
	if banner != "SSH-2.0-test\r\n" {
		t.Fatalf("首批数据丢失: %q", banner)
	}
	_, _ = io.WriteString(conn, "hello\n")
	echo, _ := br.ReadString('\n')
	if echo != "echo:hello\n" {
		t.Fatalf("回显 %q", echo)
	}
}

// TestOpenSessionPipeReportsDaemonError 验证非 101 响应把守护进程错误原文带给用户。
func TestOpenSessionPipeReportsDaemonError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = http.ReadRequest(bufio.NewReader(conn))
		body := "未找到远端 \"nope\""
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	}()
	_, _, err = openSessionPipe(ln.Addr().String(), "s", "nope", 22)
	if err == nil || !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "502") {
		t.Fatalf("应返回守护进程错误: %v", err)
	}
}
