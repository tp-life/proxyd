package subscribe

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"proxyd/internal/config"
)

// TestReadLimitedResponseBodyRejectsOverflow 验证订阅读取上限不会把超长正文
// 静默截断成看似成功的数据，同时允许刚好到达上限的完整正文。
//
// 参数说明：
//   - t: *testing.T，负责输入构造和边界断言。
//
// 返回值说明：无。
//
// 错误情况：limit 字节被误拒绝，或 limit+1 字节未返回错误时测试失败。
func TestReadLimitedResponseBodyRejectsOverflow(t *testing.T) {
	body, err := readLimitedResponseBody(bytes.NewBufferString("1234"), 4)
	if err != nil || string(body) != "1234" {
		t.Fatalf("上限内正文 body=%q err=%v", body, err)
	}
	if body, err := readLimitedResponseBody(bytes.NewBufferString("12345"), 4); err == nil || body != nil {
		t.Fatalf("超限正文应被完整拒绝，body=%q err=%v", body, err)
	}
}

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

// TestFetchWithInfoCachesSubscriptionUserInfo 验证订阅拉取会解析并缓存 subscription-userinfo。
//
// 首次请求从 HTTP header 读取用量信息；上游关闭后再次拉取应走 body 缓存，
// 同时读取 userinfo sidecar，避免网络抖动时 UI/CLI 的流量信息突然消失。
func TestFetchWithInfoCachesSubscriptionUserInfo(t *testing.T) {
	stateDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Subscription-Userinfo", "upload=1024; download=2048; total=4096; expire=1893456000")
		w.Write([]byte(clashFixture))
	}))
	sub := config.Subscription{Name: "带用量", URL: srv.URL, Type: "clash"}

	nodes, info, err := FetchWithInfo(context.Background(), sub, stateDir)
	if err != nil {
		t.Fatalf("FetchWithInfo 失败: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("期望 3 个节点，得到 %d", len(nodes))
	}
	if info.Upload != 1024 || info.Download != 2048 || info.Total != 4096 || info.Expire != 1893456000 {
		t.Fatalf("userinfo 解析异常: %+v", info)
	}

	srv.Close()
	nodes, info, err = FetchWithInfo(context.Background(), sub, stateDir)
	var w *FetchWarning
	if !errors.As(err, &w) {
		t.Fatalf("缓存降级期望 FetchWarning，得到 %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("缓存降级期望 3 个节点，得到 %d", len(nodes))
	}
	if info.Upload != 1024 || info.Download != 2048 || info.Total != 4096 || info.Expire != 1893456000 {
		t.Fatalf("缓存 userinfo 读取异常: %+v", info)
	}
}

// TestParseUserInfo 验证 subscription-userinfo 的宽松解析规则。
//
// 订阅服务端经常返回部分字段或混入无效字段；解析器应保留有效字段，
// 跳过非法值，避免因为展示字段异常影响节点订阅主体流程。
func TestParseUserInfo(t *testing.T) {
	info := ParseUserInfo("upload=1; download=2; total=3; expire=4; ignored=x; upload=-1")
	if info.Upload != 1 || info.Download != 2 || info.Total != 3 || info.Expire != 4 {
		t.Fatalf("ParseUserInfo = %+v", info)
	}
	if !ParseUserInfo("").IsZero() {
		t.Fatal("空 header 应解析为空 userinfo")
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

func TestFetchRetryOn5xx(t *testing.T) {
	stateDir := t.TempDir()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(clashFixture))
	}))
	defer srv.Close()

	sub := config.Subscription{Name: "flaky", URL: srv.URL, Type: "clash"}
	nodes, err := Fetch(context.Background(), sub, stateDir)
	if err != nil {
		t.Fatalf("前两次 502 后应重试成功: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("期望 3 个节点，得到 %d", len(nodes))
	}
	if calls != 3 {
		t.Fatalf("期望重试到第 3 次成功，实际请求 %d 次", calls)
	}
}

func TestFetchNoRetryOn4xx(t *testing.T) {
	stateDir := t.TempDir()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	sub := config.Subscription{Name: "404", URL: srv.URL, Type: "clash"}
	if _, err := Fetch(context.Background(), sub, stateDir); err == nil {
		t.Fatal("404 应返回错误")
	}
	if calls != 1 {
		t.Fatalf("4xx 不应重试，实际请求 %d 次", calls)
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
