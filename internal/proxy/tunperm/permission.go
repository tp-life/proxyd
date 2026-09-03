// Package tunperm 提供 TUN 运行权限的跨平台检测与操作指引。
//
// 该包只处理操作系统能力，不理解 proxyd 配置或 mihomo 业务语义；应用层在开启
// TUN 用例前调用它，从而让权限错误在热更新之前以可执行的中文指引返回。
package tunperm

import "fmt"

// Status 是当前进程创建并配置 TUN 设备所需权限的检测结果。
type Status struct {
	Allowed  bool   `json:"allowed"`
	Platform string `json:"platform"`
	Hint     string `json:"hint,omitempty"`
}

// Current 检测当前进程是否具备启用 TUN 的系统权限。
//
// 参数：无。
//
// 返回值：
//   - Status：包含是否允许、平台名以及权限不足时的修复指引。
//
// 错误情况：检测过程不向外返回 error；无法读取平台能力状态时按权限不足处理，
// 并把下一步操作写入 Hint，避免误判后进入更难理解的 mihomo 启动错误。
func Current() Status {
	return currentStatus()
}

// Require 要求当前进程具备启用 TUN 的权限。
//
// 参数：无。
//
// 返回值：
//   - error：权限满足时返回 nil；权限不足或平台不支持时返回带修复指引的错误。
//
// 错误情况：不同平台的具体检测失败会统一表现为权限不足，详细操作见错误文本。
func Require() error {
	status := Current()
	if status.Allowed {
		return nil
	}
	return fmt.Errorf("TUN 权限不足（%s）：%s", status.Platform, status.Hint)
}
