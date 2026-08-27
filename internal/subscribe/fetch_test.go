package subscribe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"proxyd/internal/config"
)

func TestFetchCacheFallback(t *testing.T) {
	stateDir := t.TempDir()

	// 先用正常服务器拉取并写缓存
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != userAgent {
			t.Errorf("User-Agent 期望 %q，得到 %q", userAgent, ua)
		}
		w.Write([]byte(clashFixture))
	}))
	sub := config.Subscription{Name: "测试机场", URL: srv.URL, Type: "clash"}

	nodes, err := Fetch(context.Background(), sub, stateDir)
	if err != nil {
		t.Fatalf("Fetch 失败: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("期望 3 个节点，得到 %d", len(nodes))
	}
	if _, err := os.Stat(cachePath(stateDir, sub.Name)); err != nil {
		t.Fatalf("缓存文件未写入: %v", err)
	}

	// 关掉服务器，再次拉取应降级用缓存并返回 FetchWarning
	srv.Close()
	nodes2, err := Fetch(context.Background(), sub, stateDir)
	var w *FetchWarning
	if !errors.As(err, &w) {
		t.Fatalf("期望 FetchWarning，得到 %v", err)
	}
	if w.Sub != sub.Name {
		t.Errorf("FetchWarning.Sub 不正确: %q", w.Sub)
	}
	if len(nodes2) != 3 {
		t.Fatalf("缓存降级期望 3 个节点，得到 %d", len(nodes2))
	}
}

func TestFetchNoCache(t *testing.T) {
	stateDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sub := config.Subscription{Name: "无缓存", URL: srv.URL, Type: "clash"}
	if _, err := Fetch(context.Background(), sub, stateDir); err == nil {
		t.Fatal("拉取失败且无缓存时应返回错误")
	} else {
		var w *FetchWarning
		if errors.As(err, &w) {
			t.Fatal("无缓存可用时不应返回 FetchWarning")
		}
	}
}

func TestFetchAutoSniff(t *testing.T) {
	stateDir := t.TempDir()
	links := buildShareLinks()
	shareBody := encodeShareBody(links["ss-sip002"], links["trojan"])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(shareBody))
	}))
	defer srv.Close()

	sub := config.Subscription{Name: "auto", URL: srv.URL, Type: "auto"}
	nodes, err := Fetch(context.Background(), sub, stateDir)
	if err != nil {
		t.Fatalf("Fetch(auto) 失败: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("auto 嗅探期望 2 个节点，得到 %d", len(nodes))
	}
}

func TestFetchAll(t *testing.T) {
	stateDir := t.TempDir()
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(clashFixture))
	}))
	defer okSrv.Close()
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer badSrv.Close()

	subs := []config.Subscription{
		{Name: "good", URL: okSrv.URL, Type: "clash"},
		{Name: "bad", URL: badSrv.URL, Type: "clash"},
	}
	nodes, errs := FetchAll(context.Background(), subs, stateDir, nil)
	if len(errs) != 2 {
		t.Fatalf("期望 2 个错误槽位，得到 %d", len(errs))
	}
	if errs[0] != nil {
		t.Errorf("good 订阅不应报错: %v", errs[0])
	}
	if errs[1] == nil {
		t.Error("bad 订阅应报错")
	}
	if len(nodes) != 3 {
		t.Fatalf("一个订阅失败不应影响其他订阅，期望 3 个节点，得到 %d", len(nodes))
	}
}

func TestSanitizeFileName(t *testing.T) {
	cases := map[string]string{
		"普通名字":       "普通名字",
		"a/b\\c:d*e": "a_b_c_d_e",
		"..":         "unnamed",
		"":           "unnamed",
	}
	for in, want := range cases {
		if got := sanitizeFileName(in); got != want {
			t.Errorf("sanitizeFileName(%q) = %q，期望 %q", in, got, want)
		}
	}
}
