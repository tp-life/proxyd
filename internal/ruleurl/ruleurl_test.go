package ruleurl

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"proxyd/internal/config"
)

func TestParseRuleText(t *testing.T) {
	body := `# 注释
// 另一种注释

DOMAIN-SUFFIX,example.com,PROXY
IP-CIDR,10.0.0.0/8,DIRECT,no-resolve
INVALID-LINE
DOMAIN-SUFFIX,example.com,PROXY
DOMAIN-SUFFIX,,DIRECT
`
	rules := Parse([]byte(body))
	want := []string{
		"DOMAIN-SUFFIX,example.com,PROXY",
		"IP-CIDR,10.0.0.0/8,DIRECT,no-resolve",
	}
	if !slices.Equal(rules, want) {
		t.Errorf("Parse = %v, want %v", rules, want)
	}
}

func TestParseGFWList(t *testing.T) {
	gfw := `[AutoProxy 0.2.9]
! 注释行
||example.com
@@||direct.cn
||*.wildcard.com
||path.com/some
|https://urlprefix.com
@@|http://excluded-prefix.com
.plain-domain.org
||dup.com
||dup.com
`
	body := base64.StdEncoding.EncodeToString([]byte(gfw))
	rules := Parse([]byte(body))
	want := []string{
		"DOMAIN-SUFFIX,example.com,PROXY",
		"DOMAIN-SUFFIX,direct.cn,DIRECT",
		"DOMAIN-SUFFIX,dup.com,PROXY", // 去重
	}
	if !slices.Equal(rules, want) {
		t.Errorf("Parse(gfwlist) = %v, want %v", rules, want)
	}
}

func TestParsePlainTextNotBase64(t *testing.T) {
	// 普通规则文本不应被误判为 gfwlist（MATCH 只有 2 段，不满足 ≥3 段跳过）
	body := "DOMAIN-SUFFIX,example.com,PROXY\nMATCH,PROXY\n"
	rules := Parse([]byte(body))
	if len(rules) != 1 || rules[0] != "DOMAIN-SUFFIX,example.com,PROXY" {
		t.Errorf("Parse = %v", rules)
	}
}

func TestFetchAndCache(t *testing.T) {
	stateDir := t.TempDir()
	ru := config.RuleURL{Name: "test-src"}

	// 在线拉取
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "DOMAIN-SUFFIX,example.com,PROXY")
	}))
	ru.URL = srv.URL
	res := Fetch(context.Background(), ru, stateDir)
	if res.Err != nil || res.Warn != nil || len(res.Rules) != 1 {
		t.Fatalf("Fetch: %+v", res)
	}
	// 缓存已写入
	if _, err := os.Stat(cachePath(stateDir, ru.Name)); err != nil {
		t.Fatalf("缓存未写入: %v", err)
	}

	// 源挂掉后降级用缓存
	srv.Close()
	res = Fetch(context.Background(), ru, stateDir)
	if res.Err != nil || res.Warn == nil || len(res.Rules) != 1 {
		t.Errorf("缓存降级: %+v", res)
	}

	// 缓存也没有时报错
	res = Fetch(context.Background(), config.RuleURL{Name: "no-such", URL: "http://127.0.0.1:1/x"}, stateDir)
	if res.Err == nil {
		t.Error("无缓存时应返回错误")
	}
}

func TestFetchAllConcurrent(t *testing.T) {
	stateDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dead" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintln(w, "DOMAIN-SUFFIX,example.com,PROXY")
	}))
	defer srv.Close()
	rus := []config.RuleURL{
		{Name: "a", URL: srv.URL},
		{Name: "b", URL: srv.URL + "/dead"},
		{Name: "c", URL: srv.URL},
	}
	results := FetchAll(context.Background(), rus, stateDir)
	if len(results) != 3 {
		t.Fatalf("results = %d", len(results))
	}
	if results[0].Err != nil || len(results[0].Rules) != 1 {
		t.Errorf("a: %+v", results[0])
	}
	// b 404 → 无缓存 → 报错；不影响 c
	if results[1].Err == nil {
		t.Errorf("b: 应报错: %+v", results[1])
	}
	if results[2].Err != nil {
		t.Errorf("c: %+v", results[2])
	}
}

func TestCachePathSanitize(t *testing.T) {
	p := cachePath("/state", "我的规则/源:1")
	want := filepath.Join("/state", "cache", "rules-我的规则_源_1.cache")
	if p != want {
		t.Errorf("cachePath = %q, want %q", p, want)
	}
}

func TestContent(t *testing.T) {
	stateDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "DOMAIN-SUFFIX,example.com,PROXY")
	}))
	ru := config.RuleURL{Name: "test-src", URL: srv.URL}

	// 无缓存时现场拉取（原文，未解析），并写入缓存
	body, err := Content(context.Background(), ru, stateDir)
	if err != nil {
		t.Fatalf("Content(现场拉取): %v", err)
	}
	if string(body) != "DOMAIN-SUFFIX,example.com,PROXY\n" {
		t.Errorf("Content = %q", body)
	}
	if _, err := os.Stat(cachePath(stateDir, ru.Name)); err != nil {
		t.Fatalf("现场拉取后应写缓存: %v", err)
	}

	// 源挂掉后走缓存
	srv.Close()
	body, err = Content(context.Background(), ru, stateDir)
	if err != nil || string(body) != "DOMAIN-SUFFIX,example.com,PROXY\n" {
		t.Errorf("Content(缓存命中) = %q, %v", body, err)
	}

	// 无缓存且拉取失败时报错
	if _, err := Content(context.Background(),
		config.RuleURL{Name: "no-such", URL: "http://127.0.0.1:1/x"}, stateDir); err == nil {
		t.Error("无缓存且拉取失败时应返回错误")
	}
}

// TestContentDecodesGFWListForDisplay 验证内容查看链路会把整体 Base64 编码的
// AutoProxy/gfwlist 转换为人类可读原文，同时不改变普通规则源的既有行为。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于创建隔离缓存目录并报告断言失败。
//
// 返回值：无；通过 testing 断言表达验证结果。
//
// 错误情况：现场拉取失败、返回值仍为 Base64，或解码后内容与上游原文不一致时测试失败。
// 该测试直接覆盖 Web 内容接口调用的 ruleurl.Content seam，防止解析规则正常但展示仍泄漏编码文本。
func TestContentDecodesGFWListForDisplay(t *testing.T) {
	stateDir := t.TempDir()
	gfw := "[AutoProxy 0.2.9]\n! display fixture\n||example.com\n@@||direct.example\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(gfw))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, encoded)
	}))
	defer server.Close()

	body, err := Content(context.Background(), config.RuleURL{Name: "gfwlist", URL: server.URL}, stateDir)
	if err != nil {
		t.Fatalf("Content(gfwlist): %v", err)
	}
	if string(body) != gfw {
		t.Fatalf("Content(gfwlist) = %q, want decoded %q", body, gfw)
	}

	server.Close()
	cachedBody, err := Content(context.Background(), config.RuleURL{Name: "gfwlist", URL: server.URL}, stateDir)
	if err != nil {
		t.Fatalf("Content(gfwlist cache): %v", err)
	}
	if string(cachedBody) != gfw {
		t.Fatalf("Content(gfwlist cache) = %q, want decoded %q", cachedBody, gfw)
	}
}
