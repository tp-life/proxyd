package api

// 管理端点回归验证输入限制、脱敏响应与不触网的禁用诊断。
import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReliabilityRoutesValidateAndRedact 检查公共适配器契约；参数 t 为测试对象；无返回，接受缺失预检或泄露凭据时失败。
func TestReliabilityRoutesValidateAndRedact(t *testing.T) {
	s, _ := newRemoteTestServer(t)
	if err := s.app.SetModuleEnabled("remote", false); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.registerDiagnosticRoutes(mux)
	s.registerConfigHistoryRoutes(mux)
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"POST", "/api/diagnostics", `{"peer":"","token":"private"}`, 400},
		{"POST", "/api/diagnostics", `{"peer":"unknown"}`, 400},
		{"POST", "/api/diagnostics", `{"peer":""}`, 200},
		{"GET", "/api/config/history", "", 200},
		{"POST", "/api/config/history/invalid/restore", `{}`, 400},
		{"GET", "/api/config/history/invalid/export", "", 404},
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if response.Code != tc.code {
			t.Fatalf("%s: %d != %d", tc.path, response.Code, tc.code)
		}
		if strings.Contains(response.Body.String(), "private") {
			t.Fatal("输入凭据进入响应")
		}
	}
}
