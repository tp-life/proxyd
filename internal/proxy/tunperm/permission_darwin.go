//go:build darwin

package tunperm

import (
	"os"

	"proxyd/internal/proxy/tunhelper"
)

// helperReachable 检测 tun-helper 是否可接管 TUN 设备创建；声明为变量以便测试替换。
var helperReachable = func() bool {
	return tunhelper.Precheck() == nil
}

// currentStatus 检测 macOS 当前进程的 TUN 权限：root 直接允许（mihomo 自行创建
// utun）；普通用户依赖 tun-helper（LaunchDaemon 常驻的 root 助手，经 SCM_RIGHTS
// 回传设备 fd，见 docs/adr/0004 方案 B 追述），helper 可达即允许。
//
// 参数：无。
//
// 返回值：
//   - Status：root 或 helper 可达时允许，否则给出一次性安装 helper 的指引。
//
// 错误情况：os.Geteuid 不返回错误；helper 探测失败按权限不足处理。
func currentStatus() Status {
	if os.Geteuid() == 0 || helperReachable() {
		return Status{Allowed: true, Platform: "macOS"}
	}
	return Status{
		Platform: "macOS",
		Hint:     "请执行 proxyd helper install 安装统一特权助手（一次性管理员授权，之后无需 sudo）；或使用 sudo proxyd serve 以 root 运行",
	}
}
