package api

// 订阅策略组兜底出口策略的 API 级验证：默认 auto、可切换、非法值被拒。

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"proxyd/internal/app"
	"proxyd/internal/config"
)

// TestSubscriptionGroupFallbackAPI 验证兜底策略接口的默认值、成功切换与非法取值拒绝。
func TestSubscriptionGroupFallbackAPI(t *testing.T) {
	dir := t.TempDir()
	// 兜底策略切换会重新生成并热应用配置，因此这里直接构造一份可生成的完整配置。
	cfg := &config.Config{
		StateDir:  dir,
		APIListen: "127.0.0.1:19091",
		Listen:    "127.0.0.1",
		Mode:      "rule",
		LogLevel:  "silent",
		Rules:     []string{"MATCH,PROXY"},
	}
	instance, err := app.New(cfg, filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	server := New("127.0.0.1:0", instance)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		server.Shutdown(t.Context())
		instance.Shutdown()
	})

	mux := http.NewServeMux()
	server.registerProxyGroupRoutes(mux)

	// 默认应为 auto：未配置时也让「只有直连成员的订阅组」跟随主端口默认出口。
	if got := instance.Config().EffectiveGroupFallback(); got != config.GroupFallbackAuto {
		t.Fatalf("默认兜底策略应为 auto: %q", got)
	}

	call := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/subscription-group-fallback", strings.NewReader(body)))
		return recorder
	}

	if recorder := call(`{"mode":"off"}`); recorder.Code != http.StatusOK {
		t.Fatalf("切换为 off 失败: %d %s", recorder.Code, recorder.Body.String())
	}
	if got := instance.Config().EffectiveGroupFallback(); got != config.GroupFallbackOff {
		t.Fatalf("切换后取值异常: %q", got)
	}
	if recorder := call(`{"mode":"proxy"}`); recorder.Code != http.StatusOK {
		t.Fatalf("切换为 proxy 失败: %d %s", recorder.Code, recorder.Body.String())
	}
	if got := instance.Config().EffectiveGroupFallback(); got != config.GroupFallbackProxy {
		t.Fatalf("切换后取值异常: %q", got)
	}
	if recorder := call(`{"mode":"不合法"}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("非法取值应返回 400: %d", recorder.Code)
	}
	if got := instance.Config().EffectiveGroupFallback(); got != config.GroupFallbackProxy {
		t.Fatalf("非法取值不应改变运行态: %q", got)
	}
	if recorder := call(`{}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("缺少 mode 应返回 400: %d", recorder.Code)
	}
}
