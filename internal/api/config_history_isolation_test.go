package api

// 配置历史隔离回归必须执行真实导入测试，防止临时配置文件仍引用用户默认状态目录。

import (
	"os"
	"path/filepath"
	"testing"

	"proxyd/internal/config"
)

// TestConfigImportDoesNotWriteUserHistory 验证导入测试不会向用户默认目录归档。
// 参数：t 为 *testing.T；返回无；测试执行后默认历史目录出现或检查失败时报告错误。
// HOME 与 USERPROFILE 只在该测试范围内改成临时目录，覆盖 Unix/Windows，禁止并行执行。
func TestConfigImportDoesNotWriteUserHistory(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome)
	defaultHistory := filepath.Join(config.DefaultStateDir(), "config-history")
	t.Run("执行真实配置导入测试", TestConfigExportImportAPI)
	if _, err := os.Stat(defaultHistory); !os.IsNotExist(err) {
		t.Fatalf("导入测试向用户默认配置历史写入了数据，检查结果: %v", err)
	}
}
