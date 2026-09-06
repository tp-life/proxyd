package api

// 本文件验证模块管理的 HTTP 输入约束与实际状态转换。
import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestModuleAPIValidationAndLifecycle 验证显式开关、错误输入拒绝与旧服务开关保留。
// 参数：t 为 *testing.T；返回无；错误状态码、意外修改或失效路由均导致失败。
func TestModuleAPIValidationAndLifecycle(t *testing.T) {
	server, _ := newRemoteTestServer(t)
	mux := http.NewServeMux()
	server.registerModuleRoutes(mux)
	for _, tc := range []struct {
		path, body string
		code       int
	}{
		{"remote", `{}`, 400}, {"remote", `{"enabled":null}`, 400}, {"remote", `{"enabled":true,"extra":1}`, 400},
		{"remote", `{"enabled":false} {}`, 400}, {"other", `{"enabled":false}`, 400},
		{"remote", `{"enabled":false}`, 200}, {"remote", `{"enabled":true}`, 200},
	} {
		req := httptest.NewRequest("POST", "/api/modules/"+tc.path, strings.NewReader(tc.body))
		result := httptest.NewRecorder()
		mux.ServeHTTP(result, req)
		if result.Code != tc.code {
			t.Fatalf("%s %s: code=%d want=%d", tc.path, tc.body, result.Code, tc.code)
		}
	}
	if server.app.Config().Remote.Enabled {
		t.Fatal("启用模块不应擅自开启隧道服务端")
	}
	result := httptest.NewRecorder()
	mux.ServeHTTP(result, httptest.NewRequest("GET", "/api/modules", nil))
	if result.Code != 200 || !strings.Contains(result.Body.String(), `"id":"proxy"`) {
		t.Fatal("模块列表不可用")
	}
}
