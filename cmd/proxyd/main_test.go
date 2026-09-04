package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFirstRunGuide 验证首次运行错误严格提供三行可执行引导。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：行数不是三行、缺少订阅快捷启动命令或配置路径时测试失败。
func TestFirstRunGuide(t *testing.T) {
	const configPath = "/tmp/proxyd-test/config.yaml"
	lines := strings.Split(firstRunGuide(configPath).Error(), "\n")
	if len(lines) != 3 {
		t.Fatalf("首次运行引导行数=%d，期望 3：%q", len(lines), lines)
	}
	if !strings.Contains(lines[1], "proxyd serve <订阅地址>") {
		t.Errorf("第二行缺少快捷启动命令: %q", lines[1])
	}
	if !strings.Contains(lines[2], configPath) {
		t.Errorf("第三行缺少配置路径: %q", lines[2])
	}
}

// TestHealthEndpointRespondsHasBoundedTimeout 验证占用 API 端口但不返回
// HTTP 响应头的异常服务，不会让 start/status 命令永久卡住。
//
// 参数说明：
//   - t: *testing.T，提供回环监听、清理与超时断言。
//
// 返回值说明：无。
//
// 错误情况：探测误判为成功，或没有在客户端超时后及时返回时测试失败。
func TestHealthEndpointRespondsHasBoundedTimeout(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("创建健康探测测试监听失败: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()

	client := &http.Client{Timeout: 30 * time.Millisecond}
	started := time.Now()
	if healthEndpointResponds(client, server.URL, "") {
		t.Fatal("未返回响应头的服务不应被判定为健康")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("健康探测超时未生效，耗时 %s", elapsed)
	}
}

// TestHealthEndpointRespondsRequiresOKAndAuthentication 验证健康检查只有在携带正确
// 管理口令并收到 HTTP 200 时才判定服务就绪。
//
// 参数说明：
//   - t: *testing.T，提供隔离 HTTP 服务和断言。
//
// 返回值说明：无。
//
// 错误情况：错误凭据导致的 401 被误判为健康，或正确凭据不能通过时测试失败。
func TestHealthEndpointRespondsRequiresOKAndAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "proxyd" || password != "management-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &http.Client{Timeout: time.Second}
	if healthEndpointResponds(client, server.URL, "wrong-secret") {
		t.Fatal("HTTP 401 不应被判定为健康")
	}
	if !healthEndpointResponds(client, server.URL, "management-secret") {
		t.Fatal("携带正确 Basic Auth 的 HTTP 200 应判定为健康")
	}
}
