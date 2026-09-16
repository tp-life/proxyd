package core

import (
	"testing"

	mihomolog "github.com/metacubex/mihomo/log"
)

// TestTailscaleEnrollmentTracker 验证 mihomo UserLogf 的注册链接和 Running 状态会
// 被准确归属到对应节点，且不会泄漏到其它同时存在的注册会话。
//
// 参数说明：t 是 Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：日志前缀解析错误、注册链接提取失败、状态迁移错误或会话串线时失败。
func TestTailscaleEnrollmentTracker(t *testing.T) {
	var tracker tailscaleEnrollmentTracker
	tracker.Begin("campone", "approval")
	tracker.Begin("other", "auth-key")
	t.Cleanup(tracker.Close)

	tracker.consumeEvent(mihomolog.Event{
		LogLevel: mihomolog.INFO,
		Payload:  "[Tailscale](campone) To start this tsnet server, restart with TS_AUTHKEY set, or go to: https://hs.campone.cc/register/hskey-authreq-example",
	})
	item, ok := tracker.Snapshot("campone")
	if !ok || item.State != TailscaleEnrollmentWaiting || item.RegistrationURL != "https://hs.campone.cc/register/hskey-authreq-example" || item.AuthID != "hskey-authreq-example" {
		t.Fatalf("等待审批状态异常: %+v exists=%v", item, ok)
	}
	// mihomo 的底层 Tailscale Logf 会产生包含控制面 API 地址的 DEBUG 日志，且
	// observable 在控制台日志等级过滤前就能收到它。该地址不是用户注册链接，绝不能
	// 覆盖先前已经取得的 Auth URL，否则管理员最终只能看到 /machine/register。
	tracker.consumeEvent(mihomolog.Event{
		LogLevel: mihomolog.DEBUG,
		Payload:  `[Tailscale](campone) request={"URL":"https://hs.campone.cc/machine/register":true}`,
	})
	item, _ = tracker.Snapshot("campone")
	if item.RegistrationURL != "https://hs.campone.cc/register/hskey-authreq-example" || item.AuthID != "hskey-authreq-example" {
		t.Fatalf("控制面 DEBUG URL 不应覆盖注册链接: %+v", item)
	}
	other, _ := tracker.Snapshot("other")
	if other.State != TailscaleEnrollmentStarting || other.RegistrationURL != "" {
		t.Fatalf("其它会话不应接收到 campone 的注册链接: %+v", other)
	}

	tracker.consumeEvent(mihomolog.Event{
		LogLevel: mihomolog.INFO,
		Payload:  "[Tailscale](campone) AuthLoop: state is Running; done",
	})
	item, _ = tracker.Snapshot("campone")
	if item.State != TailscaleEnrollmentConnected {
		t.Fatalf("批准后状态未进入 connected: %+v", item)
	}
}
