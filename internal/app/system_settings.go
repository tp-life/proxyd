package app

// 通用设置用例组合进程级服务，不依赖代理节点或远程连接的运行状态。

import "proxyd/internal/autostart"

// SystemSettings 提供公共设置页所需的已脱敏状态，不包含业务设置或管理凭据。
type SystemSettings struct {
	Autostart        bool                    `json:"autostart"`
	AutostartRuntime autostart.RuntimeStatus `json:"autostart_runtime"`
	VersionCheck     VersionCheckStatus      `json:"version_check"`
}

// SystemSettings 聚合自启状态与版本检查缓存，供纯远程实例使用同一通用设置页。
// 参数：无；返回 SystemSettings；自启查询有界且通过 RuntimeStatus 表达故障，不主动联网检查版本。
func (a *App) SystemSettings() SystemSettings {
	runtime := a.AutostartRuntime()
	return SystemSettings{Autostart: runtime.Enabled, AutostartRuntime: runtime, VersionCheck: a.VersionStatus()}
}
