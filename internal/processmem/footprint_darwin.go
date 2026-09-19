//go:build darwin

package processmem

import (
	"os"
	"syscall"
	"unsafe"
)

// rusageInfoV4 是 libproc 的 RUSAGE_INFO_V4 flavor；rusage_info_v4 已包含
// ri_phys_footprint，比 V0 更明确地要求内核填充完整布局。
const rusageInfoV4 = 4

// physFootprintOffset 是 rusage_info_t 中 ri_phys_footprint 的字节偏移：
// 16 字节 UUID 之后依次是 ri_user_time、ri_system_time、ri_pkg_idle_wkups、
// ri_interrupt_wkups、ri_pageins、ri_wired_size、ri_resident_size 共 7 个 uint64。
// 该字段自 rusage_info_v0 起位置固定，属于 libproc 的稳定 ABI。
const physFootprintOffset = 16 + 7*8

// rusageBufferSize 是传给 proc_pid_rusage 的缓冲大小。取当前最大布局
// （rusage_info_v6）之上再留余量：只需要读取偏移固定的一个字段，因此不跟着 libproc
// 的字段增删维护一长串结构体定义；缓冲区偏大不会影响内核写入。
const rusageBufferSize = 1024

// Footprint 返回当前进程的物理内存占用（phys_footprint，字节）。
//
// 功能说明：
// darwin 上 Go 运行时用 MADV_FREE_REUSABLE 归还堆页，这些页已经空闲、不再计入
// phys_footprint，但仍然算在 resident_size 里；后者正是 mihomo `/memory` 上报的值，
// 因此会明显高于任务管理器口径。改用 phys_footprint 后展示值与「活动监视器」一致。
//
// 参数：无。
//
// 返回值：
//   - uint64：物理内存占用字节数。
//   - bool：读取成功为 true；调用失败时为 false，调用方应退化为不展示该指标。
//
// 错误情况：proc_pid_rusage 返回非 0 errno（例如系统不支持该 flavor）时返回 (0, false)，
// 不 panic 也不输出日志。
func Footprint() (uint64, bool) {
	buf := make([]byte, rusageBufferSize)
	_, _, errno := syscall_syscall6(libc_proc_pid_rusage_trampoline_addr, uintptr(os.Getpid()), rusageInfoV4, uintptr(unsafe.Pointer(&buf[0])), 0, 0, 0)
	if errno != 0 {
		return 0, false
	}
	return *(*uint64)(unsafe.Pointer(&buf[physFootprintOffset])), true
}

// libc_proc_pid_rusage_trampoline_addr 由同目录的汇编文件填充：它指向一段只做
// JMP libc_proc_pid_rusage 的 trampoline 代码。Go 无法直接调用 libSystem 中的 C 函数，
// 只能取该地址后交给 runtime 的 syscall_syscall6 间接调用（与 x/sys/unix 同构）。
var libc_proc_pid_rusage_trampoline_addr uintptr

//go:cgo_import_dynamic libc_proc_pid_rusage proc_pid_rusage "/usr/lib/libSystem.B.dylib"

// syscall_syscall6 由 runtime 实现（runtime/sys_darwin.go），用于间接调用 libSystem
// 中的 C 函数；libproc 不是 syscall，必须走这条路径。
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname syscall_syscall6 syscall.syscall6
