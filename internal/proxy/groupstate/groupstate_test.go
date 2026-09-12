package groupstate

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRoundTrip 验证选中项的保存/加载往返，以及文件不存在时的空映射语义。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：往返内容不一致、缺失文件报错或临时文件残留时测试失败。
func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load(不存在) 出错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Load(不存在) = %v, 期望空映射", got)
	}

	want := map[string]string{"vpn 组": "tailscale 节点", "media": "香港 01"}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save 出错: %v", err)
	}
	got, err = Load(path)
	if err != nil {
		t.Fatalf("Load 出错: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("往返后长度 = %d, 期望 %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("selected[%q] = %q, 期望 %q", k, got[k], v)
		}
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("临时文件未被 rename 清理: err=%v", err)
	}
}

// TestLoadCorrupted 验证损坏文件返回错误而不返回部分数据。
func TestLoadCorrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err == nil {
		t.Fatal("损坏文件应返回错误")
	}
	if got != nil {
		t.Errorf("损坏文件不应返回部分映射: %v", got)
	}
}
