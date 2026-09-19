//go:build !darwin && !linux && !windows

package processmem

// Footprint 在不支持的平台上恒返回不可用。
//
// 功能说明：
// 当前发布矩阵只覆盖 darwin/linux/windows；保留该实现是为了让其它 GOOS 仍能编译通过，
// 调用方看到 false 后应退化为不展示内存指标，而不是显示一个口径不明的数字。
//
// 参数：无。
//
// 返回值：恒为 (0, false)。
//
// 错误情况：无。
func Footprint() (uint64, bool) {
	return 0, false
}
