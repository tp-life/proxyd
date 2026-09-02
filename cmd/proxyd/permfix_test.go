package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubRepairHooks 替换交互/提权钩子并登记恢复；返回 sudo 调用记录。
func stubRepairHooks(t *testing.T, terminal, confirm bool, sudo func(args ...string) error) *[]string {
	t.Helper()
	oldTerminal, oldConfirm, oldSudo := isTerminal, confirmRepair, sudoRunner
	t.Cleanup(func() {
		isTerminal, confirmRepair, sudoRunner = oldTerminal, oldConfirm, oldSudo
	})
	isTerminal = func() bool { return terminal }
	confirmRepair = func([]string) bool { return confirm }
	var calls []string
	sudoRunner = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if sudo != nil {
			return sudo(args...)
		}
		return nil
	}
	return &calls
}

func TestProbeWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下权限位不生效，跳过")
	}
	dir := t.TempDir()

	if err := probeWritable(dir); err != nil {
		t.Fatalf("可写目录应通过探测: %v", err)
	}
	if err := probeWritable(filepath.Join(dir, "not-exist.yaml")); err != nil {
		t.Fatalf("不存在文件应探测父目录并通过: %v", err)
	}

	lockedFile := filepath.Join(dir, "locked.yaml")
	if err := os.WriteFile(lockedFile, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := probeWritable(lockedFile); !isPermission(err) {
		t.Fatalf("只读文件应报权限错误，得到 %v", err)
	}

	lockedDir := filepath.Join(dir, "locked")
	if err := os.Mkdir(lockedDir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := probeWritable(lockedDir); !isPermission(err) {
		t.Fatalf("只读目录应报权限错误，得到 %v", err)
	}
}

func TestOfferOwnershipRepairAllWritable(t *testing.T) {
	calls := stubRepairHooks(t, true, true, nil)
	if err := offerOwnershipRepair(t.TempDir()); err != nil {
		t.Fatalf("全部可写时不应报错: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("无需修复时不应调用 sudo: %v", *calls)
	}
}

func TestOfferOwnershipRepairFixesWithSudo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下权限位不生效，跳过")
	}
	base := t.TempDir()
	locked := filepath.Join(base, "state")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	calls := stubRepairHooks(t, true, true, func(args ...string) error {
		// 模拟 sudo chown 的效果：恢复目录写权限
		return os.Chmod(locked, 0o755)
	})
	if err := offerOwnershipRepair(locked); err != nil {
		t.Fatalf("修复后应通过: %v", err)
	}
	if len(*calls) != 1 || !strings.Contains((*calls)[0], "chown -R") || !strings.Contains((*calls)[0], locked) {
		t.Fatalf("sudo 调用不符合预期: %v", *calls)
	}
}

func TestOfferOwnershipRepairDeclined(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下权限位不生效，跳过")
	}
	base := t.TempDir()
	locked := filepath.Join(base, "state")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	stubRepairHooks(t, true, false, nil)
	err := offerOwnershipRepair(locked)
	if err == nil || !strings.Contains(err.Error(), "已取消") {
		t.Fatalf("用户取消时应返回带指引的错误，得到 %v", err)
	}
}

func TestOfferOwnershipRepairNonTerminal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下权限位不生效，跳过")
	}
	base := t.TempDir()
	locked := filepath.Join(base, "state")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	stubRepairHooks(t, false, true, nil)
	err := offerOwnershipRepair(locked)
	if err == nil || !strings.Contains(err.Error(), "sudo chown") {
		t.Fatalf("非终端环境应返回手动修复指引，得到 %v", err)
	}
}

func TestConfigPathFromArgs(t *testing.T) {
	if got := configPathFromArgs(nil); got == "" {
		t.Fatal("无参数时应返回默认配置路径")
	}
	if got := configPathFromArgs([]string{"-c", "/tmp/a.yaml"}); got != "/tmp/a.yaml" {
		t.Fatalf("应解析 -c 后的路径，得到 %s", got)
	}
	// flag 解析遇位置参数停止：-c 在订阅 URL 之后不生效
	if got := configPathFromArgs([]string{"https://a.com/sub", "-c", "/tmp/b.yaml"}); got == "/tmp/b.yaml" {
		t.Fatal("位置参数之后的 -c 不应被当作配置文件路径")
	}
}
