package api

// 「当前生效规则」接口的 API 级验证：分页/筛选参数解析与错误码。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"proxyd/internal/app"
	"proxyd/internal/config"
)

// TestEffectiveRulesAPI 验证 GET /api/rules/effective 的分页、筛选与参数校验。
//
// 覆盖点：
//   - 未应用配置时返回 200 与 available=false（代理停用不是请求错误）。
//   - 应用配置后返回规则、来源统计与来源中文名。
//   - source/q/limit 生效，未知来源与非法分页参数返回 400。
//
// 参数：t 为 Go 测试上下文。
// 返回值：无；状态码、字段缺失或筛选结果不符时测试失败。
// 错误情况：无。
func TestEffectiveRulesAPI(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		StateDir:    dir,
		APIListen:   "127.0.0.1:19093",
		Listen:      "127.0.0.1",
		Mode:        "rule",
		LogLevel:    "silent",
		Rules:       []string{"MATCH,PROXY"},
		CustomRules: []string{"DOMAIN-SUFFIX,first.example,DIRECT"},
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
	server.registerProxyRuleRoutes(mux)

	get := func(path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}
	decode := func(path string) app.EffectiveRulesStatus {
		t.Helper()
		recorder := get(path)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s 应返回 200: %d %s", path, recorder.Code, recorder.Body.String())
		}
		var status app.EffectiveRulesStatus
		if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
			t.Fatalf("%s 响应不是合法 JSON: %v", path, err)
		}
		return status
	}

	// 尚未应用任何配置：明确报告不可用，而不是让前端把空列表误当成"没有规则"。
	if status := decode("/api/rules/effective"); status.Available || status.Total != 0 {
		t.Fatalf("未应用配置时应返回 available=false: %#v", status)
	}

	if err := instance.AddRule("DOMAIN-SUFFIX,second.example,PROXY"); err != nil {
		t.Fatal(err)
	}
	status := decode("/api/rules/effective")
	if !status.Available || status.Total != 3 || len(status.Rules) != 3 {
		t.Fatalf("生效规则接口返回异常: %#v", status)
	}
	if status.Rules[0].SourceLabel == "" || status.Rules[0].Source != "custom" {
		t.Fatalf("规则应带来源键与中文名: %#v", status.Rules[0])
	}
	if len(status.Sources) == 0 {
		t.Fatalf("响应应包含来源统计: %#v", status)
	}

	// 分页与筛选：limit=1 只回一条；关键字命中第二条并保留全局位置。
	page := decode("/api/rules/effective?limit=1&offset=1")
	if len(page.Rules) != 1 || page.Rules[0].Position != 2 || page.Limit != 1 {
		t.Fatalf("分页参数未生效: %#v", page)
	}
	filtered := decode("/api/rules/effective?source=" + "custom" + "&q=second")
	if filtered.Matched != 1 || filtered.Rules[0].Position != 2 {
		t.Fatalf("筛选参数未生效: %#v", filtered)
	}

	// 参数错误必须是 400：CLI 与前端都依赖状态码区分"没有数据"和"参数写错"。
	for _, path := range []string{
		"/api/rules/effective?offset=-1",
		"/api/rules/effective?limit=abc",
		"/api/rules/effective?source=bogus",
	} {
		recorder := get(path)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s 应返回 400: %d %s", path, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "bad request") {
			t.Fatalf("%s 错误信息格式不符合约定: %s", path, recorder.Body.String())
		}
	}
}
