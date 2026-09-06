package lifecycle

import (
	"testing"
	"time"
)

// TestLateResultAndRetry 验证迟到结果与外部指针修改无法污染新状态。
// 参数 t 为测试上下文；返回无；世代隔离、重试时间或恢复失败时报告错误。
func TestLateResultAndRetry(t *testing.T) {
	var s Tracker
	old := s.Begin()
	current := s.Begin()
	s.Complete(current, "retrying", false, "网络不可达", 30*time.Second)
	s.Complete(old, "running", true, "", 0)
	got := s.Snapshot()
	if got.Phase != "retrying" || got.NextRetryAt == nil {
		t.Fatal(got)
	}
	*got.NextRetryAt = time.Time{}
	if s.Snapshot().NextRetryAt.IsZero() {
		t.Fatal("指针共享")
	}
	id := s.Begin()
	s.Complete(id, "running", true, "", 0)
	if s.Snapshot().NextRetryAt != nil || s.Snapshot().Error != "" {
		t.Fatal("恢复后残留错误")
	}
}
