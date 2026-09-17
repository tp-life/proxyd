//go:build darwin

package autostart

import (
	"fmt"
	"testing"
)

// 本文件以实际 launchctl 输出形状复现托管进程退出与嵌套状态误判。

// TestParseLaunchdNestedState 验证反复退出的服务不会因 coalition 活跃而误报运行。
// 参数：t 为 *testing.T。返回：无。错误：顶层状态、退出码或拉起次数解析错误时失败。
func TestParseLaunchdNestedState(t *testing.T) {
	s := parseLaunchd(`system/com.proxyd = {
	state = spawn scheduled
	arguments = {
		/opt/proxy d/proxyd
		serve
		-c
		/Users/test/config dir/config.yaml
	}
	runs = 7
	last exit code = 78: EX_CONFIG
	resource coalition = {
		state = active
	}
}`)
	if s.Running || s.State != "spawn scheduled" || s.LastExitCode == nil || *s.LastExitCode != 78 {
		t.Fatalf("错误的退出服务快照: %+v", s)
	}
	if s.Runs == nil || *s.Runs != 7 {
		t.Fatalf("未解析拉起次数: %+v", s)
	}
}

// TestInspectReportsServiceState 验证运行中、查询失败与未加载三种快照的区分。
// 参数：t 为 *testing.T。返回：无。错误：运行态误判、查询故障被吞或未加载误报时失败。
func TestInspectReportsServiceState(t *testing.T) {
	previous := queryLaunchd
	// 清理闭包恢复只读查询适配器；无参数和返回值，无错误。
	defer func() { queryLaunchd = previous }()
	// 运行中替身：无参数，返回 running 快照，不访问本机服务。
	queryLaunchd = func(...string) (string, error) {
		return "system/com.proxyd = {\n state = running\n pid = 42\n }", nil
	}
	if s := Inspect(); !s.Running || s.PID != 42 {
		t.Fatalf("未识别托管进程: %+v", s)
	}
	// 查询故障替身：参数未使用，返回模拟超时，运行态必须保持未知而非未运行。
	queryLaunchd = func(...string) (string, error) { return "", fmt.Errorf("query timeout") }
	if s := Inspect(); s.State != "unknown" || s.Running {
		t.Fatalf("查询故障不得误报未运行: %+v", s)
	}
	// 未加载替身：返回 launchd 的服务缺失文本，应识别为 unloaded。
	queryLaunchd = func(...string) (string, error) {
		return "Could not find service \"com.proxyd\" in domain for system", fmt.Errorf("exit status 113")
	}
	if s := Inspect(); s.State != "unloaded" || s.Loaded {
		t.Fatalf("未识别服务缺失: %+v", s)
	}
}
