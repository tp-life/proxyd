package app

// 系统摘要是管理控制台的只读用例，不包含代理或远程访问的核心业务规则。

import "time"

// SystemStatus 描述应用实例的运行时长及待重启状态，不携带配置内容或凭据。
type SystemStatus struct {
	UptimeSeconds  int64 `json:"uptime_seconds"`
	PendingRestart bool  `json:"pending_restart"`
}

// SystemStatus 读取当前实例摘要，供总览页独立于业务模块展示管理进程状态。
// 参数：无；返回 SystemStatus 值对象；无外部 I/O 或预期错误。
// startedAt 构造后不可变，待重启字段通过已有读锁查询；测试零值实例返回零时长。
func (a *App) SystemStatus() SystemStatus {
	var uptime int64
	if !a.startedAt.IsZero() {
		uptime = max(0, int64(time.Since(a.startedAt).Seconds()))
	}
	return SystemStatus{UptimeSeconds: uptime, PendingRestart: a.ConfigRestartPending()}
}
