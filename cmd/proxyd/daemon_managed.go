package main

import (
	"fmt"
	"time"

	"proxyd/internal/autostart"
	"proxyd/internal/config"
)

// 本文件提供系统自启服务的只读观察与诊断提示。
// start/stop/restart 与开机自启相互独立（见 daemon.go），系统服务状态只用于提示，
// 不参与启动所有权协调。

// inspectService 是只读适配器入口，测试可替换以避免操作本机服务。
var inspectService = autostart.Inspect

// autostartPoll 是 on 后等待系统服务进入 running 的轮询参数，测试可替换缩短。
var autostartPollInterval = 300 * time.Millisecond
var autostartPollRounds = 10

// warnBrokenSystemService 检测系统自启服务崩溃循环（已注册但未运行且最近非零退出）
// 并打印修复指引；仅提示，不影响本次独立启动。
//
// 参数说明：无。
//
// 返回值说明：无；检测结果直接打印到标准输出。
//
// 错误情况：系统查询失败时快照字段为空，条件不成立，静默跳过提示。
func warnBrokenSystemService() {
	s := inspectService()
	if s.Loaded && !s.Running && s.LastExitCode != nil && *s.LastExitCode != 0 {
		fmt.Printf("提示：系统自启服务反复启动失败（最近退出码 %d）；本次将以独立进程启动。\n", *s.LastExitCode)
		fmt.Println("      建议执行 proxyd autostart off && proxyd autostart on 重新注册修复系统服务。")
	}
}

// warnIfSystemServiceRespawned 在 stop 成功后检测系统服务是否立刻拉起了新实例
// （旧版 plist 的 KeepAlive=true 策略会让 stop 不生效），是则提示修复路径。
//
// 参数说明：
//   - stoppedPID: int，本次停止的进程 PID。
//   - wasManaged: bool，停止前该 PID 是否为系统服务实例；不是则跳过检测。
//
// 返回值说明：无；检测结果直接打印到标准输出。
//
// 错误情况：系统查询失败时快照为零值，条件不成立，静默跳过提示。
func warnIfSystemServiceRespawned(stoppedPID int, wasManaged bool) {
	if !wasManaged {
		return
	}
	s := inspectService()
	if s.Running && s.PID > 0 && s.PID != stoppedPID {
		fmt.Printf("提示：系统自启服务又拉起了新实例 (pid %d)——旧版 KeepAlive 策略会在停止后立即重启。\n", s.PID)
		fmt.Println("      如需真正停止：proxyd autostart off；或重注册（proxyd autostart off && proxyd autostart on）后再 stop。")
	}
}

// printAutostartGuidance 在「实例未运行」场景补充开机自启上下文与下一步指引，
// 避免「刚开了开机自启却报未运行」的困惑：系统服务向独立实例让位是一次性的，
// 独立实例停止后系统服务不会自动接管，需明确告知交回方式。
//
// 参数说明：无。
//
// 返回值说明：无；状态与指引直接打印到标准输出。
//
// 错误情况：自启未开启、服务未注册或运行态查询失败（非 darwin 恒为 unknown）时
// 快照字段不足以下结论，静默跳过指引。
func printAutostartGuidance() {
	s := inspectService()
	if !s.Enabled || !s.Loaded {
		return
	}
	switch {
	case s.Running:
		fmt.Printf("开机自启：已开启，系统服务运行中 (pid %d)\n", s.PID)
	case s.State == "spawn scheduled":
		fmt.Println("开机自启：已开启，系统服务已排队等待拉起，稍后可再查 proxyd status")
	default:
		fmt.Println("开机自启：已开启（系统服务当前未运行）")
		fmt.Println("      立即运行：proxyd start（独立进程）；交回系统服务：proxyd autostart on；否则下次开机自动拉起")
	}
}

// reportAutostartServiceState 在 autostart on 后报告系统服务实际状态。
// 独立实例占用 pidfile 时系统实例会按单实例守卫干净退出让位（ensureSingleInstance），
// 必须明确告知接管方式，避免误以为开启失败。
//
// 参数说明：
//   - cfg: *config.Config，提供 pid 文件路径。
//
// 返回值说明：无；状态与提示直接打印到标准输出。
//
// 错误情况：系统查询失败时快照为零值，退化为打印快照消息；bootstrap 后到 running
// 有短暂窗口，无独立实例占用时先轮询等待再下结论。
func reportAutostartServiceState(cfg *config.Config) {
	if pid, alive := readPIDFile(pidPath(cfg)); alive {
		fmt.Printf("提示：独立实例 (pid %d) 正在运行，系统服务实例已让位（避免双实例）。\n", pid)
		fmt.Println("      开机自启已注册，下次开机自动接管；如需立即交回系统服务：proxyd stop && proxyd autostart on。")
		return
	}
	for i := 0; i < autostartPollRounds; i++ {
		s := inspectService()
		if s.Running {
			fmt.Printf("系统服务已启动 (pid %d)\n", s.PID)
			return
		}
		time.Sleep(autostartPollInterval)
	}
	if s := inspectService(); s.Message != "" {
		fmt.Println("系统服务状态：" + s.Message)
	}
}
