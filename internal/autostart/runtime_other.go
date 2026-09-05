//go:build !darwin

package autostart

// 非 macOS 平台保持原有启动行为，仅展示已经实现的平台注册状态。

// inspect 查询注册状态。参数：无。返回：RuntimeStatus；尚未实现进程查询时明确标记未知。
// 错误：平台注册查询错误作为消息返回。
func inspect() RuntimeStatus {
	enabled, err := status()
	s := RuntimeStatus{Enabled: enabled, State: "unknown", Message: "未查询系统托管进程状态"}
	if err != nil {
		s.Message = err.Error()
	}
	return s
}

// managed 保持其他平台现有的后台派生语义。参数：string 配置路径，当前未使用。
// 返回：false、nil。错误：无；本次协调只适用于 macOS LaunchDaemon。
func managed(string) (bool, error) { return false, nil }
