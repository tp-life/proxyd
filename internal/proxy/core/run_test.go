package core

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureStateDirWritableRemovesUnreadableGeoFile 验证目录可写但 geo 文件无权限时，
// NewRunner 会删除该文件让 mihomo 重新下载，而不是让 GEO 规则一直降级运行。
//
// 参数：
//   - t: *testing.T，Go 测试上下文；root 运行测试时权限位不生效，用例直接跳过。
//
// 返回值：无；通过文件是否被删除断言表达结果。
//
// 错误情况：无权限 geo 文件残留、或正常文件被误删时测试失败。
func TestEnsureStateDirWritableRemovesUnreadableGeoFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下权限位不生效，跳过")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir) // 避免 SetHomeDir 影响其他用例的默认路径解析

	blocked := filepath.Join(dir, "GeoSite.dat")
	if err := os.WriteFile(blocked, []byte("stale"), 0o000); err != nil {
		t.Fatal(err)
	}
	readable := filepath.Join(dir, "GeoIP.dat")
	if err := os.WriteFile(readable, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	NewRunner(dir)

	if _, err := os.Stat(blocked); !os.IsNotExist(err) {
		t.Fatalf("无权限 geo 文件应被删除以便重新下载，stat err=%v", err)
	}
	if _, err := os.Stat(readable); err != nil {
		t.Fatalf("正常 geo 文件不应被动，stat err=%v", err)
	}
}
