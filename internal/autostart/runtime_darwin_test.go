//go:build darwin

package autostart

import (
	"fmt"
	"testing"
)

// 本文件以实际 launchctl 输出形状复现托管进程退出、配置区分和嵌套状态误判。

// TestParseLaunchdNestedState 验证反复退出的服务不会因 coalition 活跃而误报运行。
// 参数：t 为 *testing.T。返回：无。错误：顶层状态、退出码或含空格路径解析错误时失败。
func TestParseLaunchdNestedState(t *testing.T) {
	s := parseLaunchd(`system/com.proxyd = {
	state = spawn scheduled
	arguments = {
		/opt/proxy d/proxyd
		serve
		-c
		/Users/test/config dir/config.yaml
	}
	last exit code = 1
	resource coalition = {
		state = active
	}
}`)
	if s.Running || s.State != "spawn scheduled" || s.LastExitCode == nil || *s.LastExitCode != 1 || s.ConfigPath != "/Users/test/config dir/config.yaml" {
		t.Fatalf("错误的退出服务快照: %+v", s)
	}
}

// TestManagedUsesLoadedConfiguration 验证启动项即使本轮被关闭，仍按 launchd 内存中的配置协调。
// 参数：t 为 *testing.T。返回：无。错误：同配置未托管、不同配置误托管或查询错误被吞掉时失败。
func TestManagedUsesLoadedConfiguration(t *testing.T) {
	previous := queryLaunchd
	// 清理闭包恢复只读查询适配器；无参数和返回值，无错误。
	defer func() { queryLaunchd = previous }()
	// 查询替身不访问系统；参数为 launchctl 参数，返回含实际字段的快照，无错误。
	queryLaunchd = func(...string) (string, error) {
		return "system/com.proxyd = {\n state = running\n pid = 42\n arguments = {\n /opt/proxyd\n serve\n -c\n /tmp/managed.yaml\n }\n }", nil
	}
	for path, want := range map[string]bool{"/tmp/managed.yaml": true, "/tmp/other.yaml": false} {
		if got, err := Managed(path); err != nil || got != want {
			t.Fatalf("Managed(%s) = %v, %v", path, got, err)
		}
	}
	if s := Inspect(); !s.Running || s.PID != 42 {
		t.Fatalf("未识别托管进程: %+v", s)
	}
	// 查询故障替身：参数未使用，返回模拟超时，禁止把查询失败当作允许派生独立实例。
	queryLaunchd = func(...string) (string, error) { return "", fmt.Errorf("query timeout") }
	if _, err := Managed("/tmp/managed.yaml"); err == nil {
		t.Fatal("查询故障不能回退到独立启动")
	}
}
