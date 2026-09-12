//go:build linux

package gateway

// Linux 执行层：nftables 应用/清除 proxyd-gw 表，经 /proc/sys 开关 IPv4 转发。
// 依赖 setcap 能力位（cap_net_admin/cap_net_raw/cap_net_bind_service，见 perm_linux.go）。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"proxyd/internal/config"
)

// ipForwardPath 是内核 IPv4 转发开关的 sysctl 文件。
const ipForwardPath = "/proc/sys/net/ipv4/ip_forward"

// nftTableName 是本模块独占的 nftables 表名；Clear 只删除这张表。
const nftTableName = "proxyd-gw"

// runCommand 执行外部命令并向其 stdin 写入 input；定义为包级变量以便测试替换
// （sysproxy 同款惯例）。
var runCommand = func(input, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// writeSysctl 写 sysctl 文件（如 /proc/sys/net/ipv4/ip_forward）；包级变量便于测试替换。
var writeSysctl = func(path, value string) error {
	return os.WriteFile(path, []byte(value), 0o644)
}

// readSysctl 读 sysctl 文件首行文本；失败时返回空串。
func readSysctl(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// nftRunner 是 Linux 的 Runner 实现。
type nftRunner struct{}

// newRunner 选定 Linux 执行层。
func newRunner() Runner { return nftRunner{} }

// isUnsupported 报告当前平台不受支持；Linux 恒为 false。
func isUnsupported() bool { return false }

// renderRules 生成 Linux 的 nftables ruleset 文本。
func renderRules(cfg config.GatewayConfig, dnsListenPort int) string {
	return RenderNFTRuleset(cfg, dnsListenPort)
}

// Apply 先预检特权能力位，再经 `nft -f -` 整体替换 proxyd-gw 表。
//
// 参数：
//   - text: string，RenderNFTRuleset 生成的 ruleset 文本。
//
// 返回值：error，能力位缺失（含中文 setcap 指引）或 nft 执行失败时返回。
//
// 错误情况：nft 语法/内核拒绝时错误文案带命令输出原文。
func (nftRunner) Apply(text string) error {
	if err := requireGatewayCaps(); err != nil {
		return err
	}
	if _, err := runCommand(text, "nft", "-f", "-"); err != nil {
		return fmt.Errorf("应用 nftables 规则失败: %w", err)
	}
	return nil
}

// Clear 删除 proxyd-gw 表；表不存在视为已成功（幂等清除）。
func (nftRunner) Clear() error {
	out, err := runCommand("", "nft", "delete", "table", "inet", nftTableName)
	if err != nil && !strings.Contains(out, "No such file") {
		return fmt.Errorf("清除 nftables 规则失败: %w", err)
	}
	return nil
}

// Forwarding 经 /proc/sys/net/ipv4/ip_forward 开关内核 IPv4 转发。
func (nftRunner) Forwarding(enable bool) error {
	value := "0"
	if enable {
		value = "1"
	}
	if err := writeSysctl(ipForwardPath, value); err != nil {
		return fmt.Errorf("设置 IPv4 转发失败: %w", err)
	}
	return nil
}

// Status 返回 ip_forward 当前值与 proxyd-gw 表是否存在的可读摘要。
func (nftRunner) Status() string {
	forwarding := "off"
	if readSysctl(ipForwardPath) == "1" {
		forwarding = "on"
	}
	table := "absent"
	if _, err := runCommand("", "nft", "list", "table", "inet", nftTableName); err == nil {
		table = "present"
	}
	return fmt.Sprintf("ip_forward=%s table=%s", forwarding, table)
}

// DiagnoseGateway 执行 Linux 专属检查：能力位、IPv4 转发实际值与 nft 表存在性。
// Detail 只用固定文案，不回传原始命令输出。
func (nftRunner) DiagnoseGateway(_ context.Context) []DiagnosticCheck {
	steps := []DiagnosticCheck{}
	if err := requireGatewayCaps(); err != nil {
		steps = append(steps, DiagnosticCheck{ID: "gateway_permission", Status: "failed", Detail: "缺少 cap_net_admin 等能力位，请按网关页指引执行 setcap 后重启 proxyd"})
	} else {
		steps = append(steps, DiagnosticCheck{ID: "gateway_permission", Status: "passed", Detail: "特权能力位已就绪"})
	}
	if readSysctl(ipForwardPath) == "1" {
		steps = append(steps, DiagnosticCheck{ID: "gateway_forward", Status: "passed", Detail: "IPv4 转发当前为 on"})
	} else {
		steps = append(steps, DiagnosticCheck{ID: "gateway_forward", Status: "failed", Detail: "IPv4 转发当前为 off，请查看网关页状态"})
	}
	if _, err := runCommand("", "nft", "list", "table", "inet", nftTableName); err == nil {
		steps = append(steps, DiagnosticCheck{ID: "gateway_nft", Status: "passed", Detail: "nftables 规则表已载入"})
	} else {
		steps = append(steps, DiagnosticCheck{ID: "gateway_nft", Status: "failed", Detail: "nftables 规则表未载入"})
	}
	return steps
}
