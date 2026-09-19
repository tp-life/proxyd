package processmem

import (
	"runtime"
	"testing"
)

// TestFootprint 验证受支持平台上能读到合理的内存占用。
//
// 功能说明：
// 该指标只用于展示，但读错口径会把「已归还的空闲页 + 共享库页」当成真实占用
// （mihomo `/memory` 的 resident_size 就是这个问题）。这里至少验证接口在发布矩阵
// 覆盖的平台上真的可用，且数值落在物理上可能的范围内；不支持的平台必须明确返回
// false，让调用方退化为不展示，而不是显示一个凭空捏造的数字。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无；返回值不可用或超出合理区间时失败。
//
// 错误情况：无外部依赖；任何平台都能执行。
func TestFootprint(t *testing.T) {
	got, ok := Footprint()

	supported := runtime.GOOS == "darwin" || runtime.GOOS == "linux" || runtime.GOOS == "windows"
	if !supported {
		if ok {
			t.Fatalf("不支持的平台 %s 必须返回不可用", runtime.GOOS)
		}
		return
	}
	if !ok {
		t.Fatalf("%s 上 Footprint 必须可用", runtime.GOOS)
	}
	// 一个正在运行的 Go 进程至少占用数 MB；上限取 1 TiB，用于发现单位换算错误
	// （例如把 kB 当字节返回，或把页数当字节返回）。
	const (
		minFootprint = 1 << 20
		maxFootprint = 1 << 40
	)
	if got < minFootprint || got > maxFootprint {
		t.Fatalf("Footprint() = %d 字节，超出合理区间 [%d, %d]", got, minFootprint, maxFootprint)
	}
}
