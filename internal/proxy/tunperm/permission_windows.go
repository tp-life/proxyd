//go:build windows

package tunperm

import "golang.org/x/sys/windows"

// currentStatus 检测 Windows 当前进程令牌是否已提升为管理员权限。
//
// 参数：无。
//
// 返回值：
//   - Status：令牌已提升时允许，否则提示使用“以管理员身份运行”。
//
// 错误情况：GetCurrentProcessToken 返回当前进程伪句柄，不需要关闭且不返回错误；
// 无法提升的普通令牌统一按权限不足处理。
func currentStatus() Status {
	if windows.GetCurrentProcessToken().IsElevated() {
		return Status{Allowed: true, Platform: "Windows"}
	}
	return Status{
		Platform: "Windows",
		Hint:     "请关闭当前实例，以“管理员身份运行”PowerShell 或终端后重新启动 proxyd",
	}
}
