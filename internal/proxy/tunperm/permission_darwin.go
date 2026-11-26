//go:build darwin

package tunperm

import "os"

// currentStatus 检测 macOS 当前进程的 TUN 权限：创建 utun 设备与配置路由需要 root。
//
// 参数：无。
//
// 返回值：
//   - Status：root 直接允许，否则给出 sudo 或开机自启指引。
//
// 错误情况：os.Geteuid 不返回错误。
func currentStatus() Status {
	if os.Geteuid() == 0 {
		return Status{Allowed: true, Platform: "macOS"}
	}
	return Status{
		Platform: "macOS",
		Hint:     "请使用 sudo proxyd serve 以 root 运行，或启用开机自启（macOS LaunchDaemon 默认以 root 运行）",
	}
}
