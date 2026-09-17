package autostart

// 本文件定义平台自启状态的只读快照；注册状态与进程状态分离，避免安装成功被误报为运行成功。

// RuntimeStatus 表示启动项和系统托管进程的独立状态，不包含配置凭据。
type RuntimeStatus struct {
	Enabled      bool   `json:"enabled"`
	Loaded       bool   `json:"loaded"`
	Running      bool   `json:"running"`
	PID          int    `json:"pid,omitempty"`
	State        string `json:"state"`
	LastExitCode *int   `json:"last_exit_code,omitempty"`
	// Runs 是 launchd 累计拉起次数（仅 macOS 解析），用于识别反复拉起即退的崩溃循环。
	Runs *int `json:"runs,omitempty"`
	Message      string `json:"message"`
}

// Inspect 查询自启项与托管进程。参数：无。返回：RuntimeStatus 快照。
// 错误：查询失败保留为快照消息，不把系统查询故障误报为关闭，也不阻断概览接口。
func Inspect() RuntimeStatus { return inspect() }
