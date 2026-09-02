//go:build !darwin && !linux && !windows

package tunperm

import "runtime"

// currentStatus 为未实现权限适配器的平台返回明确的不支持结果。
//
// 参数：无。
//
// 返回值：
//   - Status：Allowed 固定为 false，并包含当前 GOOS 和支持平台列表。
//
// 错误情况：无；未知平台不会尝试创建 TUN，避免产生不可恢复的路由副作用。
func currentStatus() Status {
	return Status{
		Platform: runtime.GOOS,
		Hint:     "当前平台尚未实现 TUN 权限检测；proxyd 目前支持 macOS、Linux 和 Windows",
	}
}
