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
