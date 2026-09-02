package logbuf

import "testing"

// TestRingTailAndLevel 验证环形缓冲覆盖顺序与日志等级过滤。
func TestRingTailAndLevel(t *testing.T) {
	r := New(3)
	r.Add("first")
	r.Add("[warning] second")
	r.Add("[error] third")
	r.Add("fourth")

	got := r.Tail(2, "")
	if len(got) != 2 || got[0].Line != "[error] third" || got[1].Line != "fourth" {
		t.Fatalf("Tail(2) = %+v", got)
	}
	warnings := r.Tail(10, "warning")
	if len(warnings) != 1 || warnings[0].Line != "[warning] second" {
		t.Fatalf("warning filter = %+v", warnings)
	}
}

// TestWriterSplitsLines 验证 Writer 能把跨 Write 调用的半行日志合并成完整行。
func TestWriterSplitsLines(t *testing.T) {
	r := New(10)
	w := NewWriter(r)
	if _, err := w.Write([]byte("a\nb")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("c\n")); err != nil {
		t.Fatal(err)
	}
	got := r.Tail(10, "")
	if len(got) != 2 || got[0].Line != "a" || got[1].Line != "bc" {
		t.Fatalf("writer lines = %+v", got)
	}
}
