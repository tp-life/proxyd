//go:build linux

package processmem

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// statusPath 是提供匿名常驻内存的内核接口；smapsRollupPath 额外提供惰性释放页。
const (
	statusPath      = "/proc/self/status"
	smapsRollupPath = "/proc/self/smaps_rollup"
)

// Footprint 返回当前进程的匿名物理内存占用（字节）。
//
// 功能说明：
// linux 上 Go 运行时优先用 MADV_FREE 归还堆页，被标记的页在内存压力出现前仍然计入
// `/proc/self/status` 的 RssAnon，因此直接展示 RssAnon 会像 darwin 一样偏高。
// 这里优先读 `/proc/self/smaps_rollup`，用 `Anonymous` 减去 `LazyFree` 得到真正仍在
// 使用的匿名内存；内核过老、没有 LazyFree 字段时退化为 `RssAnon`。
//
// 参数：无。
//
// 返回值：
//   - uint64：匿名物理内存占用字节数。
//   - bool：读取成功为 true；两个接口都不可用时为 false，调用方应退化为不展示。
//
// 错误情况：文件不存在、无权限或字段缺失时退化为下一档数据源；全部失败返回 (0, false)。
func Footprint() (uint64, bool) {
	if anon, lazyFree, ok := readSmapsRollup(); ok {
		if lazyFree >= anon {
			return anon, true
		}
		return anon - lazyFree, true
	}
	return readStatusRssAnon()
}

// readSmapsRollup 从 /proc/self/smaps_rollup 读取匿名常驻内存与惰性释放页。
//
// 参数：无。
//
// 返回值：
//   - uint64：Anonymous 字段（字节）。
//   - uint64：LazyFree 字段（字节）；字段缺失时为 0。
//   - bool：Anonymous 字段读取成功时为 true。
//
// 错误情况：文件不可读或缺少 Anonymous 字段时返回 (0, 0, false)。
func readSmapsRollup() (uint64, uint64, bool) {
	fields, err := readProcFields(smapsRollupPath)
	if err != nil {
		return 0, 0, false
	}
	anon, ok := fields["Anonymous"]
	if !ok {
		return 0, 0, false
	}
	return anon, fields["LazyFree"], true
}

// readStatusRssAnon 从 /proc/self/status 读取 RssAnon，作为 smaps_rollup 不可用时的退路。
//
// 参数：无。
//
// 返回值：
//   - uint64：RssAnon（字节）。
//   - bool：字段读取成功时为 true。
//
// 错误情况：文件不可读或缺少 RssAnon 字段（内核过老）时返回 (0, false)。
func readStatusRssAnon() (uint64, bool) {
	fields, err := readProcFields(statusPath)
	if err != nil {
		return 0, false
	}
	anon, ok := fields["RssAnon"]
	return anon, ok
}

// readProcFields 解析 /proc 中 "Key: <数值> kB" 形式的行，统一换算为字节。
//
// 参数：
//   - path: string，/proc 下的文件路径。
//
// 返回值：
//   - map[string]uint64：键为去掉冒号的字段名，值为字节数。
//   - error：文件打开或扫描失败时返回。
//
// 错误情况：非数值行、单位不是 kB 的行与格式异常的行被跳过，不影响其它字段解析。
func readProcFields(path string) (map[string]uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	fields := make(map[string]uint64)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		// 数值与单位之间可能是空格或制表符，统一按空白切分。
		parts := strings.Fields(value)
		if len(parts) != 2 || parts[1] != "kB" {
			continue
		}
		parsed, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			continue
		}
		fields[strings.TrimSpace(key)] = parsed * 1024
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return fields, nil
}
