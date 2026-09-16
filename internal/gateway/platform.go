package gateway

import "errors"

// ErrUnsupported 表示当前平台不支持 LAN 网关（Windows 及其他未适配平台，
// 见 docs/adr/0003：Windows 不做网关）。
var ErrUnsupported = errors.New("当前平台不支持 LAN 网关")

// ErrHelperUnavailable 表示 macOS 特权 helper 不可达（未安装或未运行）。
// 编排层用 errors.Is 识别该哨兵，把模块映射为 degraded + 安装指引而非 failed。
var ErrHelperUnavailable = errors.New("gateway 特权 helper 不可用")

// PrecheckResult 是「启用前检查」的结果：平台支持性、特权能力是否就绪与修复指引。
type PrecheckResult struct {
	Platform  string `json:"platform"`
	Supported bool   `json:"supported"`
	Ready     bool   `json:"ready"`  // 特权前置条件已满足（macOS helper 可达 / Linux 能力位齐备）
	Detail    string `json:"detail"` // 中文说明；未就绪时为可执行的修复指引
}

// Runner 是平台执行层抽象：应用/清除转发规则、开关内核 IPv4 转发。
// 实现：macOS 经 root helper 的 unix socket 白名单指令（pf_darwin.go），
// Linux 直接 nftables + /proc/sys（nftables_linux.go），其他平台返回 ErrUnsupported。
type Runner interface {
	// Apply 整体应用规则文本（pf anchor / nftables ruleset），幂等。
	Apply(text string) error
	// Clear 清除本模块应用的全部规则；规则不存在时不报错。
	Clear() error
	// Forwarding 开关内核 IPv4 转发。
	Forwarding(enable bool) error
	// Status 返回执行层的人类可读状态（供诊断展示）。
	Status() string
}
