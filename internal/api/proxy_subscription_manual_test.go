package api

// 代理订阅 HTTP 边界测试：确保配置操作不会被误当成手动同步。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"proxyd/internal/app"
	"proxyd/internal/config"
)

// TestAddSubscriptionDoesNotTriggerRefresh 验证新增订阅只保存配置，不产生后台下载任务。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于构造应用、请求记录器和短暂观察窗口。
//
// 返回值：无。
//
// 错误情况：新增接口未返回 201，或 runRefresh 收到任何任务时测试失败；后者能防止
// 未来把“新增且启用”再次错误实现为自动同步。
func TestAddSubscriptionDoesNotTriggerRefresh(t *testing.T) {
	a, err := app.New(&config.Config{}, "")
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	server := New("127.0.0.1:0", a)
	started := make(chan bool, 1)
	server.runRefresh = func(_ context.Context, fetch bool) error {
		started <- fetch
		return nil
	}
	t.Cleanup(func() { server.Shutdown(context.Background()) })

	request := httptest.NewRequest(http.MethodPost, "/api/subscriptions", strings.NewReader(
		`{"name":"manual-only","url":"https://example.com/sub","type":"auto","enabled":true}`,
	))
	recorder := httptest.NewRecorder()
	server.handleAddSub(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("新增订阅状态码=%d，响应=%s", recorder.Code, recorder.Body.String())
	}
	select {
	case fetch := <-started:
		t.Fatalf("新增订阅不应触发后台任务，实际 fetch=%t", fetch)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestDeleteSubscriptionTriggersCachedRebuild 验证删除订阅只排队缓存重建任务，
// 不会用完整刷新去下载其它订阅。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于设置路径参数并等待异步 worker。
//
// 返回值：无。
//
// 错误情况：删除未返回 204、后台任务未执行，或任务 fetch=true 时测试失败。
func TestDeleteSubscriptionTriggersCachedRebuild(t *testing.T) {
	a, err := app.New(&config.Config{Subscriptions: []config.Subscription{
		{Name: "remove", URL: "https://remove.example/sub", Type: "auto"},
		{Name: "keep", URL: "https://keep.example/sub", Type: "auto"},
	}}, "")
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	server := New("127.0.0.1:0", a)
	started := make(chan bool, 1)
	server.runRefresh = func(_ context.Context, fetch bool) error {
		started <- fetch
		return nil
	}
	t.Cleanup(func() { server.Shutdown(context.Background()) })

	request := httptest.NewRequest(http.MethodDelete, "/api/subscriptions/remove", nil)
	request.SetPathValue("name", "remove")
	recorder := httptest.NewRecorder()
	server.handleDelSub(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("删除订阅状态码=%d，响应=%s", recorder.Code, recorder.Body.String())
	}
	select {
	case fetch := <-started:
		if fetch {
			t.Fatal("删除订阅只能重建缓存节点，不应触发远端下载")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("删除订阅后缓存重建任务未执行")
	}
}
