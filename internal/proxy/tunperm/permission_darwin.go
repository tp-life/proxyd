//go:build darwin

package tunperm

import "os"

// currentStatus 检测 macOS 当前进程是否以 root 身份运行。
//
// 参数：无。
//
// 返回值：
//   - Status：有效 UID 为 0 时允许，否则给出通过 sudo 启动 proxyd 的指引。
//
// 错误情况：os.Geteuid 不返回错误；非 root 统一按权限不足处理。
func currentStatus() Status {
	if os.Geteuid() == 0 {
		return Status{Allowed: true, Platform: "macOS"}
	}
	return Status{
		Platform: "macOS",
		Hint:     "请停止当前实例，并使用 sudo proxyd serve -c <配置文件> 启动；后台模式可使用 sudo proxyd start -c <配置文件>",
	}
}
