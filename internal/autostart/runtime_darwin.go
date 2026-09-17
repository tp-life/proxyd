//go:build darwin

package autostart

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// macOS 运行态查询只访问 launchd，不触发授权或修改服务；解析限定到服务顶层字段。

// queryLaunchd 执行有超时的只读查询。参数：args 为 []string，launchctl 参数。
// 返回：string 输出与 error。错误：两秒超时、执行失败或非零退出；变量便于隔离系统测试。
var queryLaunchd = func(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput()
	return string(out), err
}

// parseLaunchd 解析服务快照。参数：output 为 string，launchctl print 原始输出。
// 返回：RuntimeStatus；忽略嵌套 coalition 的 state，避免把失效服务误认成 active。
// 错误：字段缺失按未知处理，非法数字不会被当作有效 PID 或退出码。
func parseLaunchd(output string) RuntimeStatus {
	s := RuntimeStatus{Loaded: true, State: "unknown"}
	depth := 0
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "}" {
			depth--
			continue
		}
		if strings.HasSuffix(line, "= {") {
			depth++
			continue
		}
		if depth != 1 {
			continue
		}
		key, value, ok := strings.Cut(line, " = ")
		if !ok {
			continue
		}
		switch key {
		case "state":
			s.State = value
		case "pid":
			s.PID, _ = strconv.Atoi(launchdLeadingInt(value))
		case "last exit code":
			if code, err := strconv.Atoi(launchdLeadingInt(value)); err == nil {
				s.LastExitCode = &code
			}
		case "runs":
			if runs, err := strconv.Atoi(launchdLeadingInt(value)); err == nil {
				s.Runs = &runs
			}
		}
	}
	s.Running = s.State == "running" && s.PID > 0
	return s
}

// launchdLeadingInt 取数值字段的十进制前缀：新版 macOS 会在退出码后附符号名
// （如 "78: EX_CONFIG"），直接 Atoi 会整体失败。
//
// 参数说明：
//   - value: string，launchctl print 输出的字段值。
//
// 返回值说明：string，前导数字部分；无数字前缀时返回空串。
//
// 错误情况：无；空前缀由调用方的 Atoi 失败兜底。
func launchdLeadingInt(value string) string {
	value = strings.TrimSpace(value)
	end := 0
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	return value[:end]
}

// inspect 汇总文件注册状态和真实进程。参数：无。返回：RuntimeStatus。
// 错误：查询失败显示未知并保留错误语义；服务不存在与查询故障区别处理。
func inspect() RuntimeStatus {
	enabled, err := status()
	if err != nil {
		return RuntimeStatus{State: "unknown", Message: "无法读取自启注册状态"}
	}
	out, err := queryLaunchd("print", "system/"+plistLabel)
	if err != nil {
		s := RuntimeStatus{Enabled: enabled, State: "unknown", Message: "无法查询系统服务，请检查 launchctl 和系统日志"}
		if strings.Contains(out, "Could not find service") {
			s.State = "unloaded"
			s.Message = "系统服务未加载"
			if !enabled {
				s.Message = "开机自启未开启"
			}
		}
		return s
	}
	s := parseLaunchd(out)
	s.Enabled = enabled
	s.Message = "系统服务尚未运行：" + s.State
	if s.Running {
		s.Message = fmt.Sprintf("系统托管进程运行中（PID %d）", s.PID)
	}
	if !s.Running && s.LastExitCode != nil && *s.LastExitCode != 0 {
		s.Message += fmt.Sprintf("，最近退出码 %d；请检查启动日志；反复启动失败时可执行 proxyd autostart off && proxyd autostart on 重新注册修复", *s.LastExitCode)
	}
	if !enabled {
		s.Message += "；自启已关闭，已加载服务保留至本次关机"
	}
	return s
}
