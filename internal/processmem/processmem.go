// Package processmem 读取当前进程的物理内存占用。
//
// 功能说明：
// proxyd 以库方式内嵌 mihomo，数据面与控制面在同一个进程里，因此控制台展示的
// 「内核内存」实际上是整个进程的内存。mihomo `/memory` 上报的是 libproc 的
// PROC_PIDTASKINFO.resident_size：在 darwin 上它既包含 Go 运行时用 MADV_FREE_REUSABLE
// 归还后仍然驻留的空闲页，也包含全机共享的 dyld 共享缓存页，因此会远高于真实占用。
// 本包按平台返回更贴近操作系统口径的数值，供控制台与 CLI 展示。
//
// 平台差异：
//   - darwin：libproc 的 phys_footprint，与「活动监视器」展示的内存列一致。
//   - linux：/proc/self/smaps_rollup 的匿名常驻内存减去惰性释放页（MADV_FREE）。
//   - windows：GetProcessMemoryInfo 返回的工作集大小。
//
// 不在上述平台时 Footprint 返回 (0, false)，调用方应退化为不展示该指标。
package processmem
