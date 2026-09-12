//go:build linux

package gateway

// Linux 特权预检：网关数据面需要 cap_net_admin（nftables）、cap_net_raw
//（tproxy 套接字）与 cap_net_bind_service（保留给将来的低端口监听）。
// 检测与修复指引沿用 tunperm 的心智：二进制更新后能力位丢失需重设。

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Linux capability 编号（linux/capability.h）。
const (
	capNetBindService = 10
	capNetAdmin       = 12
	capNetRaw         = 13
)

// requireGatewayCaps 检测当前进程是否具备网关数据面所需的全部能力位。
// 定义为包级变量以便测试替换。
//
// 参数：无。
//
// 返回值：
//   - error：root 或三个能力位齐备时返回 nil；缺失时返回带缺失项与
//     中文 setcap 修复指引的错误。
//
// 错误情况：无法读取 /proc/self/status 时按权限不足处理，仍给出可执行的指引。
var requireGatewayCaps = func() error {
	if os.Geteuid() == 0 {
		return nil
	}
	var missing []string
	statusBody, err := os.ReadFile("/proc/self/status")
	if err != nil || !hasCapability(string(statusBody), capNetAdmin) {
		missing = append(missing, "cap_net_admin")
	}
	if err != nil || !hasCapability(string(statusBody), capNetRaw) {
		missing = append(missing, "cap_net_raw")
	}
	if err != nil || !hasCapability(string(statusBody), capNetBindService) {
		missing = append(missing, "cap_net_bind_service")
	}
	if len(missing) == 0 {
		return nil
	}
	executable, execErr := os.Executable()
	if execErr != nil {
		executable = "<proxyd 二进制路径>"
	}
	return fmt.Errorf(
		"LAN 网关权限不足（缺少 %s）：请执行 sudo setcap 'cap_net_admin,cap_net_raw,cap_net_bind_service=+ep' %s 后重启 proxyd；每次替换二进制后需要重新执行 setcap（与 TUN 同一指引），也可直接用 sudo 启动",
		strings.Join(missing, ", "), executable,
	)
}

// Precheck 检测 Linux 网关前置条件：setcap 能力位是否齐备。
//
// 参数：无。
//
// 返回值：
//   - PrecheckResult：Ready 表示 root 或三个能力位齐备；未就绪时 Detail 含 setcap 指引。
//
// 错误情况：不返回 error；/proc 读取失败按权限不足折叠进 Detail。
func Precheck() PrecheckResult {
	result := PrecheckResult{Platform: "linux", Supported: true}
	if err := requireGatewayCaps(); err != nil {
		result.Detail = err.Error()
		return result
	}
	result.Ready = true
	result.Detail = "能力位已就绪（cap_net_admin/cap_net_raw/cap_net_bind_service）"
	return result
}

// hasCapability 从 /proc/self/status 的 CapEff 位图判断指定 capability 是否生效。
//
// 参数：
//   - statusBody: string，/proc/self/status 的完整文本。
//   - capability: int，Linux capability 编号。
//
// 返回值：
//   - bool：CapEff 对应位为 1 时返回 true，字段缺失或解析失败时返回 false。
//
// 错误情况：该函数不返回 error；损坏或超出 64 位的输入保守判定为 false，
// 避免把未知权限状态误报为可用。
func hasCapability(statusBody string, capability int) bool {
	if capability < 0 || capability >= 64 {
		return false
	}
	for _, line := range strings.Split(statusBody, "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "CapEff:"))
		value, err := strconv.ParseUint(raw, 16, 64)
		if err != nil {
			return false
		}
		return value&(uint64(1)<<uint(capability)) != 0
	}
	return false
}
