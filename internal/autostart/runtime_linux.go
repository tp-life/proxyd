//go:build linux

package autostart

// Linux 平台自启托管运行态查询：通过 `systemctl --user show` 获取 systemd user unit
// 的加载状态与 MainPID，并在无 D-Bus 环境下回退识别 INVOCATION_ID，确保受 systemd
// 托管的实例触发重启时走受控非零退出由 systemd 拉起，而非在同 cgroup 内派生子进程被杀。

import (
	"os"
	"strconv"
	"strings"
)

func inspect() RuntimeStatus {
	enabled, err := status()
	s := RuntimeStatus{Enabled: enabled, State: "stopped"}
	if err != nil {
		s.State = "unknown"
		s.Message = err.Error()
		return s
	}
	out, runErr := run("systemctl", "--user", "show", "-p", "LoadState", "-p", "ActiveState", "-p", "MainPID", "-p", "ExecMainStatus", unitName)
	if runErr != nil {
		// 容器/无 user session bus 场景下，如果当前进程由 systemd 拉起（存在 INVOCATION_ID），
		// 仍需准确报告托管 PID，避免 API 重启误走 spawnRestarter 导致停服。
		if os.Getenv("INVOCATION_ID") != "" {
			s.Loaded = true
			s.Running = true
			s.PID = os.Getpid()
			s.State = "running"
			s.Message = "systemd 托管运行中"
			return s
		}
		s.State = "unknown"
		s.Message = runErr.Error()
		return s
	}
	return parseSystemdShow(out, enabled)
}

// parseSystemdShow 解析 `systemctl --user show` 的 key=value 输出。
func parseSystemdShow(out string, enabled bool) RuntimeStatus {
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			kv[k] = v
		}
	}
	s := RuntimeStatus{
		Enabled: enabled,
		Loaded:  kv["LoadState"] == "loaded",
		State:   kv["ActiveState"],
	}
	if s.State == "" {
		s.State = "unknown"
	}
	if pid, err := strconv.Atoi(kv["MainPID"]); err == nil && pid > 0 {
		s.PID = pid
	}
	if code, err := strconv.Atoi(kv["ExecMainStatus"]); err == nil {
		s.LastExitCode = &code
	}
	if s.State == "active" && s.PID > 0 {
		s.Running = true
		s.State = "running"
		s.Message = "systemd user unit 运行中"
	} else if !s.Loaded {
		s.State = "stopped"
		s.Message = "systemd user unit 未加载"
	}
	return s
}
