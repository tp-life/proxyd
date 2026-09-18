//go:build darwin

package tunperm

import (
	"os"
	"strings"
	"testing"
)

// TestCurrentStatusDarwin 验证 darwin 权限判定：root 直接允许；普通用户取决于
// tun-helper 可达性，不可达时给出 helper 安装指引。
func TestCurrentStatusDarwin(t *testing.T) {
	original := helperReachable
	t.Cleanup(func() { helperReachable = original })

	if os.Geteuid() == 0 {
		helperReachable = func() bool { return false }
		if status := currentStatus(); !status.Allowed {
			t.Fatal("root 应直接允许，与 helper 无关")
		}
		return
	}

	helperReachable = func() bool { return true }
	if status := currentStatus(); !status.Allowed {
		t.Fatal("helper 可达时普通用户应允许")
	}

	helperReachable = func() bool { return false }
	status := currentStatus()
	if status.Allowed {
		t.Fatal("helper 不可达时普通用户不应允许")
	}
	if !strings.Contains(status.Hint, "proxyd helper install") {
		t.Fatalf("指引应包含 helper 安装命令: %s", status.Hint)
	}
}
