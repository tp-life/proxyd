package api

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"proxyd/internal/app"
	"proxyd/internal/config"
	"proxyd/internal/remote"
)

// TestRemoteSessionPipeAuthExemption 验证只有带会话凭据头的中转请求豁免 Basic 认证，
// 且伪造凭据在 handler 内以 401 拒绝、不触达远端拨号。
func TestRemoteSessionPipeAuthExemption(t *testing.T) {
	a, err := app.New(&config.Config{APISecret: "management-secret"}, "")
	if err != nil {
		t.Fatal(err)
	}
	server := New("127.0.0.1:0", a)
	mux := http.NewServeMux()
	server.registerRemoteRoutes(mux)
	handler := server.requireManagementAuth(mux)

	// 无凭据头：仍走管理面 Basic 认证。
	plain := httptest.NewRecorder()
	handler.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, remote.SessionPipePath+"?name=mac&port=22", nil))
	if plain.Code != http.StatusUnauthorized || plain.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("无凭据头应要求 Basic 认证: status=%d", plain.Code)
	}

	// 凭据头不能借道访问其他管理接口。
	other := httptest.NewRequest(http.MethodGet, "/api/remote/token", nil)
	other.Header.Set(remote.SessionHeader, "forged")
	otherRecorder := httptest.NewRecorder()
	handler.ServeHTTP(otherRecorder, other)
	if otherRecorder.Code != http.StatusUnauthorized || otherRecorder.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("会话凭据头不应豁免其他路由: status=%d", otherRecorder.Code)
	}

	// 伪造凭据：进入 handler 后以 401 拒绝（无 Basic challenge）。
	forged := httptest.NewRequest(http.MethodGet, remote.SessionPipePath+"?name=mac&port=22", nil)
	forged.Header.Set(remote.SessionHeader, "forged")
	forged.Header.Set("Upgrade", remote.SessionUpgrade)
	forgedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(forgedRecorder, forged)
	if forgedRecorder.Code != http.StatusUnauthorized || forgedRecorder.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("伪造凭据应由 handler 拒绝: status=%d body=%s", forgedRecorder.Code, forgedRecorder.Body.String())
	}

	// 缺少 Upgrade 头：400。
	noUpgrade := httptest.NewRequest(http.MethodGet, remote.SessionPipePath+"?name=mac&port=22", nil)
	noUpgrade.Header.Set(remote.SessionHeader, "forged")
	noUpgradeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(noUpgradeRecorder, noUpgrade)
	if noUpgradeRecorder.Code != http.StatusBadRequest {
		t.Fatalf("缺少 Upgrade 应为 400: status=%d", noUpgradeRecorder.Code)
	}
}

// TestBridgeSessionPipe 验证桥接优先转发 Hijack 缓冲中的首批数据，并在一端关闭后收尾。
func TestBridgeSessionPipe(t *testing.T) {
	clientOuter, clientInner := net.Pipe()
	upstreamOuter, upstreamInner := net.Pipe()
	buffered := bufio.NewReader(io.MultiReader(strings.NewReader("early:"), clientInner))
	done := make(chan struct{})
	go func() {
		bridgeSessionPipe(clientInner, buffered, upstreamInner)
		close(done)
	}()

	go func() { _, _ = clientOuter.Write([]byte("late")) }()
	got := make([]byte, len("early:late"))
	if _, err := io.ReadFull(upstreamOuter, got); err != nil || string(got) != "early:late" {
		t.Fatalf("上游收到 %q err=%v", got, err)
	}
	go func() { _, _ = upstreamOuter.Write([]byte("reply")) }()
	reply := make([]byte, len("reply"))
	if _, err := io.ReadFull(clientOuter, reply); err != nil || string(reply) != "reply" {
		t.Fatalf("客户端收到 %q err=%v", reply, err)
	}
	_ = upstreamOuter.Close()
	<-done
	if _, err := clientOuter.Write([]byte("x")); err == nil {
		t.Error("上游关闭后客户端连接应被关闭")
	}
}
