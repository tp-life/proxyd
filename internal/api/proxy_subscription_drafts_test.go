package api

// 订阅草稿 HTTP 边界测试：验证 REST 路由和应用层语义错误映射。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"proxyd/internal/app"
	"proxyd/internal/config"
)

// TestSubscriptionDraftRoutesMapMissingState 验证草稿读取、应用和丢弃的不存在状态均
// 经真实 ServeMux 路由返回 404，而创建未知订阅返回业务参数错误 400。
//
// 参数：t 为 Go 测试上下文。
// 返回值：无。
// 错误情况：路由未注册、路径变量未解析或状态码映射错误时测试失败。
func TestSubscriptionDraftRoutesMapMissingState(t *testing.T) {
	a, err := app.New(&config.Config{StateDir: t.TempDir()}, "")
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)
	server := New("127.0.0.1:0", a)
	mux := http.NewServeMux()
	server.registerProxySubscriptionRoutes(mux)

	tests := []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodGet, path: "/api/subscriptions/missing/draft", want: http.StatusNotFound},
		{method: http.MethodPost, path: "/api/subscriptions/missing/draft", want: http.StatusBadRequest},
		{method: http.MethodPost, path: "/api/subscriptions/missing/draft/id/apply", want: http.StatusNotFound},
		{method: http.MethodDelete, path: "/api/subscriptions/missing/draft/id", want: http.StatusNotFound},
	}
	for _, test := range tests {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
		if recorder.Code != test.want {
			t.Errorf("%s %s status=%d want=%d body=%s", test.method, test.path, recorder.Code, test.want, recorder.Body.String())
		}
	}
}
