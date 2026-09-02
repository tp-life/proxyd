//go:build linux

package tunperm

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const linuxCapNetAdmin = 12

// currentStatus 检测 Linux 当前进程是否为 root 或具有 CAP_NET_ADMIN。
//
// 参数：无。
//
// 返回值：
//   - Status：root/CAP_NET_ADMIN 满足时允许，否则给出 setcap 指令。
//
// 错误情况：无法读取 /proc/self/status 时按权限不足处理；提示仍给出可执行的 setcap 方案。
func currentStatus() Status {
	if os.Geteuid() == 0 {
		return Status{Allowed: true, Platform: "Linux"}
	}
	statusBody, err := os.ReadFile("/proc/self/status")
	if err == nil && hasLinuxCapability(string(statusBody), linuxCapNetAdmin) {
		return Status{Allowed: true, Platform: "Linux"}
	}
	executable, executableErr := os.Executable()
	if executableErr != nil {
		executable = "<proxyd 二进制路径>"
	}
	return Status{
		Platform: "Linux",
		Hint: fmt.Sprintf(
			"请执行 sudo setcap cap_net_admin=+ep %s 后重启 proxyd；每次替换二进制后需要重新执行 setcap，也可直接用 sudo 启动",
			executable,
		),
	}
}

// hasLinuxCapability 从 /proc/self/status 的 CapEff 位图判断指定 capability 是否生效。
//
// 参数：
//   - statusBody: string，/proc/self/status 的完整文本。
//   - capability: int，Linux capability 编号；CAP_NET_ADMIN 为 12。
//
// 返回值：
//   - bool：CapEff 对应位为 1 时返回 true，字段缺失或解析失败时返回 false。
//
// 错误情况：该函数不返回 error；损坏或超出 64 位的输入保守判定为 false，
// 避免把未知权限状态误报为可用。
func hasLinuxCapability(statusBody string, capability int) bool {
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
