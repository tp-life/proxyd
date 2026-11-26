//go:build darwin

package tunperm

import (
	"os"
	"strings"
	"testing"
)

// TestCurrentStatusDarwin 验证 darwin 权限判定：root 直接允许；普通用户不允许并给出 sudo/自启指引。
func TestCurrentStatusDarwin(t *testing.T) {
	status := currentStatus()
	if os.Geteuid() == 0 {
		if !status.Allowed {
			t.Fatal("root 应直接允许")
		}
		return
	}
	if status.Allowed {
		t.Fatal("非 root 普通用户不应允许直接创建 TUN")
	}
	if !strings.Contains(status.Hint, "sudo proxyd serve") {
		t.Fatalf("指引应包含 sudo 启动命令: %s", status.Hint)
	}
}
