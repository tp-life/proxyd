package api

// 网关模块 handler 级测试：请求校验、错误透传、状态/列表结构与 degraded 语义。
// 事务回滚与 proxy 联动由 internal/app 的 gateway_test.go 覆盖。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"proxyd/internal/app"
	"proxyd/internal/config"
)

// gatewayTestServer 构造带最小可用配置的测试服务（不落盘、不拉订阅）。
func gatewayTestServer(t *testing.T) *Server {
	t.Helper()
	a, err := app.New(&config.Config{
		ManualNodes: []any{"socks5://127.0.0.1:1080#self"},
		Listen:      "127.0.0.1",
		PortRange:   [2]int{42000, 42010},
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)
	srv := New("127.0.0.1:0", a)
	t.Cleanup(func() { srv.Shutdown(t.Context()) })
	return srv
}

func TestGetGatewayStructure(t *testing.T) {
	srv := gatewayTestServer(t)
	rec := httptest.NewRecorder()
	srv.handleGetGateway(rec, httptest.NewRequest(http.MethodGet, "/api/gateway", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var overview app.GatewayOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if overview.Platform != runtime.GOOS {
		t.Errorf("platform = %q", overview.Platform)
	}
	if overview.RedirPort != config.DefaultGatewayRedirPort || overview.TProxyPort != config.DefaultGatewayTProxyPort {
		t.Errorf("生效端口 = %d/%d", overview.RedirPort, overview.TProxyPort)
	}
	if overview.DNSListenPort != config.DefaultGatewayDNSListenPort {
		t.Errorf("dns 端口 = %d", overview.DNSListenPort)
	}
	if overview.Devices == nil {
		t.Error("devices 应序列化为数组而非 null")
	}
}

func TestGatewayPrecheckAPI(t *testing.T) {
	srv := gatewayTestServer(t)
	rec := httptest.NewRecorder()
	srv.handleGatewayPrecheck(rec, httptest.NewRequest(http.MethodGet, "/api/gateway/precheck", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result struct {
		Platform  string `json:"platform"`
		Supported bool   `json:"supported"`
		Ready     bool   `json:"ready"`
		Detail    string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Platform != runtime.GOOS {
		t.Errorf("platform = %q", result.Platform)
	}
	switch runtime.GOOS {
	case "darwin":
		if result.Supported && !result.Ready && !strings.Contains(result.Detail, "helper") {
			t.Errorf("darwin 未就绪指引应提及 helper: %q", result.Detail)
		}
	case "linux":
		if !result.Supported {
			t.Error("linux 应支持")
		}
	default:
		if result.Supported {
			t.Error("未适配平台应报告不支持")
		}
	}
}

func TestGatewayDeviceAPIValidation(t *testing.T) {
	srv := gatewayTestServer(t)

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleAddGatewayDevice(rec, httptest.NewRequest(http.MethodPost, "/api/gateway/devices", strings.NewReader(body)))
		return rec
	}
	if rec := post("{invalid"); rec.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON status=%d, want 400", rec.Code)
	}
	if rec := post(`{"name":"电视","ip":"999.1.1.1"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("非法 IP status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec := post(`{"name":"","ip":"192.168.1.10"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("空名称 status=%d, want 400", rec.Code)
	}
	if rec := post(`{"name":"电视","ip":"192.168.1.10","policy":"bogus"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("非法策略 status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}

	rec := post(`{"name":"电视","ip":"192.168.1.10","policy":"proxy","mac":"aa:bb:cc:dd:ee:ff"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("添加设备 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"name":"电视","ip":"192.168.1.11"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("重名 status=%d, want 400", rec.Code)
	}

	// 更新：改名拒绝、不存在拒绝、正常更新 200。
	put := func(name, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/gateway/devices/"+name, strings.NewReader(body))
		req.SetPathValue("name", name)
		srv.handleUpdateGatewayDevice(rec, req)
		return rec
	}
	if rec := put("电视", `{"name":"新名字","ip":"192.168.1.10"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("改名 status=%d, want 400", rec.Code)
	}
	if rec := put("不存在", `{"name":"不存在","ip":"192.168.1.20"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("更新不存在设备 status=%d, want 400", rec.Code)
	}
	if rec := put("电视", `{"name":"电视","ip":"192.168.1.10","policy":"direct"}`); rec.Code != http.StatusOK {
		t.Fatalf("更新设备 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := srv.app.Config().Gateway.Devices[0].Policy; got != "direct" {
		t.Errorf("policy = %q", got)
	}

	// 删除：不存在 404、存在 204。
	del := func(name string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/api/gateway/devices/"+name, nil)
		req.SetPathValue("name", name)
		srv.handleDeleteGatewayDevice(rec, req)
		return rec
	}
	if rec := del("不存在"); rec.Code != http.StatusNotFound {
		t.Errorf("删除不存在设备 status=%d, want 404", rec.Code)
	}
	if rec := del("电视"); rec.Code != http.StatusNoContent {
		t.Fatalf("删除设备 status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSetGatewayAPI(t *testing.T) {
	srv := gatewayTestServer(t)

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleSetGateway(rec, httptest.NewRequest(http.MethodPost, "/api/gateway", strings.NewReader(body)))
		return rec
	}
	if rec := post("{invalid"); rec.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON status=%d, want 400", rec.Code)
	}

	// 启用（设备表为空）：事务提交，状态与 GET 同构。
	rec := post(`{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("启用 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var overview app.GatewayOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if overview.Phase == "" {
		t.Error("响应应携带生命周期相位")
	}

	if rec := post(`{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("停用 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !srv.app.Config().Gateway.Disabled {
		t.Error("停用未写入配置")
	}
}
