//go:build windows

package processmem

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// processMemoryCounters 是 psapi.h 中 PROCESS_MEMORY_COUNTERS 的完整布局。
// 32 位与 64 位下前两个 DWORD 之后都自然对齐到 SIZE_T，无需额外填充。
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var (
	modpsapi                 = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo = modpsapi.NewProc("GetProcessMemoryInfo")
)

// Footprint 返回当前进程的工作集大小（字节）。
//
// 功能说明：
// windows 上 Go 运行时通过 VirtualFree/MEM_DECOMMIT 归还堆内存，工作集大小会随之下降，
// 因此 GetProcessMemoryInfo 的 WorkingSetSize 已经是可信口径；这里只做接口封装。
//
// 参数：无。
//
// 返回值：
//   - uint64：工作集字节数。
//   - bool：调用成功为 true；psapi 调用失败时为 false，调用方应退化为不展示该指标。
//
// 错误情况：GetProcessMemoryInfo 返回 0 时返回 (0, false)，不 panic 也不输出日志。
func Footprint() (uint64, bool) {
	var counters processMemoryCounters
	counters.CB = uint32(unsafe.Sizeof(counters))
	ok, _, _ := procGetProcessMemoryInfo.Call(
		uintptr(windows.CurrentProcess()),
		uintptr(unsafe.Pointer(&counters)),
		uintptr(counters.CB),
	)
	if ok == 0 {
		return 0, false
	}
	return uint64(counters.WorkingSetSize), true
}
