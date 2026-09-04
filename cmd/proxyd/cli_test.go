package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"proxyd/internal/api"
	"proxyd/internal/app"
)

// TestAPIClientSendsManagementCredentials 验证 CLI 从配置加载的 api-secret
// 会以固定用户名 proxyd 的 HTTP Basic 凭据附加到每次管理请求。
//
// 参数说明：
//   - t: *testing.T，提供 HTTP 模拟服务、清理与断言报告。
//
// 返回值说明：无。
//
// 错误情况：Authorization 缺失、用户名/口令错误，或 CLI 无法读取响应时测试失败。
func TestAPIClientSendsManagementCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "proxyd" || password != "management-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	client := &apiClient{base: server.URL, secret: "management-secret"}
	text, err := client.getText("/api/test")
	if err != nil || text != "ok" {
		t.Fatalf("CLI 认证请求结果 text=%q err=%v", text, err)
	}
}

func TestParseAutoPortArg(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"off", 0, true},
		{"OFF", 0, true},
		{"0", 0, true},
		{"41998", 41998, true},
		{"65535", 65535, true},
		{"65536", 0, false},
		{"-1", 0, false},
		{"abc", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, err := parseAutoPortArg(c.in)
		if c.ok != (err == nil) || got != c.want {
			t.Errorf("parseAutoPortArg(%q) = %d, %v; want %d, ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

func TestResolveManualIndex(t *testing.T) {
	entries := []app.ManualNodeEntry{
		{Index: 0, URL: "http://h:8080", Name: "节点甲"},
		{Index: 1, URL: "socks5://h:1080", Name: "h:1080"},
	}
	if idx, err := resolveManualIndex(entries, "1"); err != nil || idx != 1 {
		t.Errorf("by index: %d, %v", idx, err)
	}
	if idx, err := resolveManualIndex(entries, "节点甲"); err != nil || idx != 0 {
		t.Errorf("by name: %d, %v", idx, err)
	}
	if _, err := resolveManualIndex(entries, "5"); err == nil {
		t.Error("下标越界应报错")
	}
	if _, err := resolveManualIndex(entries, "不存在"); err == nil {
		t.Error("未知名称应报错")
	}
}

func TestParseCFlag(t *testing.T) {
	cfg, rest, err := parseCFlag("t", []string{"-c", "/tmp/x.yaml", "add", "u"})
	if err != nil || cfg != "/tmp/x.yaml" || len(rest) != 2 || rest[0] != "add" {
		t.Errorf("cfg=%q rest=%v err=%v", cfg, rest, err)
	}
	// flag 在位置参数之后不再解析（Go flag 语义）；后位置 -c 会静默落到默认配置，
	// 极易误操作其它实例，因此直接报错而不是放行
	if _, _, err := parseCFlag("t", []string{"add", "-c", "/tmp/x.yaml"}); err == nil {
		t.Error("后位置 -c 应报错")
	}
	// 普通位置参数不受影响
	if _, rest, err := parseCFlag("t", []string{"add", "u"}); err != nil || len(rest) != 2 {
		t.Errorf("rest=%v err=%v", rest, err)
	}
}

func TestResolveNodeKey(t *testing.T) {
	ov := &api.Overview{Nodes: []api.NodeEntry{
		{Name: "香港 01", Key: "key-a", Subscription: "sub1", Port: 42001},
		{Name: "香港 01", Key: "key-b", Subscription: "sub2", Port: 42002},
		{Name: "日本 01", Key: "key-c", Subscription: "sub1", Port: 42003},
	}}
	// key 精确匹配优先
	if key, err := resolveNodeKey(ov, "key-b"); err != nil || key != "key-b" {
		t.Errorf("by key: %q, %v", key, err)
	}
	// 名称唯一时按名称解析
	if key, err := resolveNodeKey(ov, "日本 01"); err != nil || key != "key-c" {
		t.Errorf("by name: %q, %v", key, err)
	}
	// 重名时报错并列出候选 key
	if _, err := resolveNodeKey(ov, "香港 01"); err == nil ||
		!strings.Contains(err.Error(), "key-a") || !strings.Contains(err.Error(), "key-b") {
		t.Errorf("重名应报错并列出候选: %v", err)
	}
	// 不存在
	if _, err := resolveNodeKey(ov, "美国 01"); err == nil {
		t.Error("不存在节点应报错")
	}
}

func TestSubStateText(t *testing.T) {
	cases := map[string]string{
		"disabled": "已禁用",
		"empty":    "无节点",
		"error":    "全部失效",
		"degraded": "部分可用",
		"healthy":  "正常",
		"其他":       "其他",
	}
	for in, want := range cases {
		if got := subStateText(in); got != want {
			t.Errorf("subStateText(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("0123456789abcdef"); got != "01234567" {
		t.Errorf("long id: %q", got)
	}
	if got := shortID("abc"); got != "abc" {
		t.Errorf("short id: %q", got)
	}
}

func TestConnAge(t *testing.T) {
	if got := connAge("not-a-time"); got != "-" {
		t.Errorf("非法时间: %q", got)
	}
	if got := connAge(time.Now().Add(-90 * time.Second).Format(time.RFC3339Nano)); got != "1m30s" {
		t.Errorf("90s: %q", got)
	}
	if got := connAge(time.Now().Add(-2*time.Hour - 3*time.Minute).Format(time.RFC3339Nano)); got != "2h3m" {
		t.Errorf("2h3m: %q", got)
	}
}
