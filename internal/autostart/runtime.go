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
	Message      string `json:"message"`
	ConfigPath   string `json:"-"`
}

// Inspect 查询自启项与托管进程。参数：无。返回：RuntimeStatus 快照。
// 错误：查询失败保留为快照消息，不把系统查询故障误报为关闭，也不阻断概览接口。
func Inspect() RuntimeStatus { return inspect() }

// Managed 判断指定配置是否应交给系统服务启动。参数：configPath 为 string 绝对配置路径。
// 返回：bool 表示应等待系统服务，error 表示启动项存在但无法安全确认其配置或运行态。
// 错误：查询失败时禁止悄悄派生独立实例，避免与稍后恢复的系统服务竞争。
func Managed(configPath string) (bool, error) { return managed(configPath) }
