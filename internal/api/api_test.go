package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"proxyd/internal/app"
	"proxyd/internal/config"
)

// TestRuleURLContent 验证 GET /api/rule-urls/{name}/content：
// 存在的规则源返回原始文本（text/plain），不存在的返回 404。
func TestRuleURLContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "DOMAIN-SUFFIX,example.com,PROXY")
	}))
	defer upstream.Close()

	a, err := app.New(&config.Config{
		StateDir: t.TempDir(),
		RuleURLs: []config.RuleURL{{Name: "src1", URL: upstream.URL}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)

	// 存在的规则源：200 + 原文
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/rule-urls/src1/content", nil)
	req.SetPathValue("name", "src1")
	srv.handleRuleURLContent(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Body.String() != "DOMAIN-SUFFIX,example.com,PROXY\n" {
		t.Errorf("body = %q", rec.Body.String())
	}

	// 不存在的规则源：404
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/rule-urls/nope/content", nil)
	req.SetPathValue("name", "nope")
	srv.handleRuleURLContent(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的规则源 status=%d, want 404", rec.Code)
	}
}

// TestOverviewServerTime 验证 overview 的 server_time 字段：
// 必须是服务器本地时间（RFC3339 带本地时区偏移），且与当前时刻一致。
// 防止"存成 UTC 但前端按本地显示"这类时区错配回归。
func TestOverviewServerTime(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/overview", nil)
	srv.handleOverview(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var ov struct {
		ServerTime string `json:"server_time"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	if ov.ServerTime == "" {
		t.Fatal("server_time 为空")
	}
	st, err := time.Parse(time.RFC3339, ov.ServerTime)
	if err != nil {
		t.Fatalf("server_time %q 不是合法 RFC3339: %v", ov.ServerTime, err)
	}
	if d := math.Abs(time.Since(st).Seconds()); d > 2 {
		t.Errorf("server_time 与当前时刻差 %.1fs（>2s）", d)
	}
	// 时区偏移必须与服务器本地一致（UTC 序列化会在这里暴露）
	_, wantOff := time.Now().Zone()
	_, gotOff := st.Zone()
	if gotOff != wantOff {
		t.Errorf("server_time 时区偏移 = %ds, want 本地 %ds", gotOff, wantOff)
	}
}
