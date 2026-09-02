package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"proxyd/internal/config"
)

// fakeReleaseChecker 是版本检查应用层测试使用的可控端口实现。
type fakeReleaseChecker struct {
	release LatestRelease
	err     error
	calls   int
}

// blockingReleaseChecker 用通道控制请求完成时机，验证版本检查的并发代际规则。
type blockingReleaseChecker struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
	result  LatestRelease
}

// Latest 记录调用并阻塞，直到测试释放结果或 context 被取消。
//
// 参数：
//   - ctx: context.Context，用于模拟服务退出或十秒检查超时。
//
// 返回值：
//   - LatestRelease：release 通道关闭后返回的预设结果。
//   - error：ctx 先取消时返回 ctx.Err()。
//
// 错误情况：started 只在第一次调用时关闭，避免重复调用导致 close panic；calls 用原子量
// 统计，使 `go test -race` 可以验证应用层没有重复请求或测试数据竞争。
func (b *blockingReleaseChecker) Latest(ctx context.Context) (LatestRelease, error) {
	if b.calls.Add(1) == 1 {
		close(b.started)
	}
	select {
	case <-b.release:
		return b.result, nil
	case <-ctx.Done():
		return LatestRelease{}, ctx.Err()
	}
}

// Latest 返回预设 release 或错误，并记录调用次数。
//
// 参数：
//   - ctx: context.Context，测试实现不阻塞，但仍验证调用方传入非 nil context。
//
// 返回值：
//   - LatestRelease：预设最新版本。
//   - error：预设错误。
//
// 错误情况：ctx 为 nil 时返回错误，防止应用层绕过取消语义。
func (f *fakeReleaseChecker) Latest(ctx context.Context) (LatestRelease, error) {
	f.calls++
	if ctx == nil {
		return LatestRelease{}, errors.New("context 不能为空")
	}
	return f.release, f.err
}

// TestVersionCheckFindsUpdate 验证应用层比较版本并缓存可用更新。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：检查器未调用一次、状态未变为 available 或版本元数据丢失时测试失败。
func TestVersionCheckFindsUpdate(t *testing.T) {
	a, err := New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	checker := &fakeReleaseChecker{release: LatestRelease{Version: "v1.3.0", URL: "https://github.com/tp-life/proxyd/releases/tag/v1.3.0"}}
	a.ConfigureUpdateCheck("v1.2.0", checker)
	a.startVersionCheck(context.Background())
	status := a.VersionStatus()
	if checker.calls != 1 || status.State != VersionCheckAvailable || status.Latest != "v1.3.0" || status.URL == "" {
		t.Fatalf("版本检查状态异常: calls=%d status=%+v", checker.calls, status)
	}
}

// TestVersionCheckSkipsUncomparableBuild 验证 dev/裸哈希构建不会发请求或误报更新。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：不可比较版本仍调用外部检查器，或状态不是 unsupported 时测试失败。
func TestVersionCheckSkipsUncomparableBuild(t *testing.T) {
	a, err := New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	checker := &fakeReleaseChecker{release: LatestRelease{Version: "v9.9.9", URL: "https://example.com/release"}}
	a.ConfigureUpdateCheck("dev", checker)
	a.startVersionCheck(context.Background())
	status := a.VersionStatus()
	if checker.calls != 0 || status.State != VersionCheckUnsupported {
		t.Fatalf("不可比较版本未正确降级: calls=%d status=%+v", checker.calls, status)
	}
}

// TestVersionCheckDeduplicatesAndDiscardsStaleResult 验证在途请求去重，且关闭后旧结果不能覆盖状态。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：同一 generation 发出多个请求、关闭开关后旧结果写回 available，或后台任务
// 未在释放通道后结束时测试失败。
func TestVersionCheckDeduplicatesAndDiscardsStaleResult(t *testing.T) {
	a, err := New(&config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	checker := &blockingReleaseChecker{
		started: make(chan struct{}),
		release: make(chan struct{}),
		result:  LatestRelease{Version: "v2.0.0", URL: "https://example.com/v2.0.0"},
	}
	a.ConfigureUpdateCheck("v1.0.0", checker)
	done := make(chan struct{})
	go func() {
		a.startVersionCheck(context.Background())
		close(done)
	}()
	<-checker.started

	// 同一代际已有请求时，第二次启动必须立即返回，不能访问外部服务。
	a.startVersionCheck(context.Background())
	if calls := checker.calls.Load(); calls != 1 {
		t.Fatalf("同一代际重复请求次数 = %d, want 1", calls)
	}
	if err := a.SetUpdateCheck(false); err != nil {
		t.Fatalf("SetUpdateCheck(false): %v", err)
	}
	close(checker.release)
	<-done

	status := a.VersionStatus()
	if status.State != VersionCheckDisabled || status.Latest != "" || status.URL != "" {
		t.Fatalf("旧请求结果覆盖了关闭状态: %+v", status)
	}
}

// TestNormalizeBuildVersion 验证 release tag 与 git describe 版本的保守规范化规则。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：合法版本未规范化、git describe 被误当 prerelease，或 dev/hash 被接受时测试失败。
func TestNormalizeBuildVersion(t *testing.T) {
	cases := map[string]string{
		"v1.2.3":                 "v1.2.3",
		"1.2.3":                  "v1.2.3",
		"v1.2.3-dirty":           "v1.2.3",
		"v1.2.3-4-gabcdef":       "v1.2.3",
		"v1.2.3-4-gabcdef-dirty": "v1.2.3",
		"v1.3.0-rc.1":            "v1.3.0-rc.1",
		"dev":                    "",
		"abcdef123":              "",
	}
	for input, want := range cases {
		if got := normalizeBuildVersion(input); got != want {
			t.Errorf("normalizeBuildVersion(%q) = %q, want %q", input, got, want)
		}
	}
}
