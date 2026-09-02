package updatecheck

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// roundTripFunc 把函数适配为 HTTPDoer，避免单元测试监听真实端口。
type roundTripFunc func(*http.Request) (*http.Response, error)

// Do 执行测试预设的 HTTP 响应函数。
//
// 参数：
//   - req: *http.Request，检查器构造的请求。
//
// 返回值：
//   - *http.Response：测试响应。
//   - error：测试网络错误。
//
// 错误情况：完全由预设函数决定。
func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestCheckerLatest 验证 GitHub JSON 被转换为应用层 release 合约。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：请求方法/头错误、JSON 解析失败或关键字段丢失时测试失败。
func TestCheckerLatest(t *testing.T) {
	client := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.Header.Get("Accept") == "" || req.Header.Get("User-Agent") == "" {
			t.Errorf("请求属性异常: method=%s headers=%v", req.Method, req.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"tag_name":"v1.2.3","html_url":"https://github.com/tp-life/proxyd/releases/tag/v1.2.3","published_at":"2026-08-01T00:00:00Z"}`)),
			Header:     make(http.Header),
		}, nil
	})
	checker := NewWithClient("https://api.github.test/releases/latest", client)
	release, err := checker.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if release.Version != "v1.2.3" || release.URL == "" || release.PublishedAt.IsZero() {
		t.Fatalf("release = %+v", release)
	}
}

// TestCheckerLatestRejectsBadResponse 验证非 200 和缺失字段不会产生伪 release。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：错误响应或空 tag 被接受时测试失败。
func TestCheckerLatestRejectsBadResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{name: "rate limit", status: http.StatusForbidden, body: `{"message":"rate limit"}`},
		{name: "missing tag", status: http.StatusOK, body: `{"html_url":"https://example.com"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})
			if _, err := NewWithClient("https://api.github.test/releases/latest", client).Latest(context.Background()); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}
