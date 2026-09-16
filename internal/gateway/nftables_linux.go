//go:build linux

package gateway

// Linux 执行层：nftables 应用/清除 proxyd-gw 表，经 /proc/sys 开关 IPv4 转发。
// 依赖 setcap 能力位（cap_net_admin/cap_net_raw/cap_net_bind_service，见 perm_linux.go）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"proxyd/internal/config"
)

// ipForwardPath 是内核 IPv4 转发开关的 sysctl 文件。
const ipForwardPath = "/proc/sys/net/ipv4/ip_forward"

// nftTableName 是本模块独占的 nftables 表名；Clear 只删除这张表。
const nftTableName = "proxyd-gw"

const (
	// linuxTProxyRouteTable 是 proxyd 独占的策略路由表编号。使用固定的高位编号，
	// 避开 main/default/local 与文档示例常用的 100；清理时只操作此表内的精确路由。
	linuxTProxyRouteTable = "20260"
	// linuxTProxyRulePriority 是 proxyd 策略规则的固定优先级。固定优先级让重复 Apply
	// 能先精确删除旧规则再重建，避免每次配置刷新积累重复 ip rule。
	linuxTProxyRulePriority = "12026"
)

// newGatewayCommand 创建可继承 proxyd CAP_NET_ADMIN 的 Linux 子进程。
//
// 参数：name 为可执行文件名；args 为原样传入该程序的命令行参数。
// 返回值：*exec.Cmd，尚未启动，调用方仍可设置 stdin/stdout 与超时。
// 错误情况：本函数不执行 I/O；父进程缺少 permitted capability 时，真正 Start
// 会因无法提升 ambient capability 返回权限错误。
func newGatewayCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	// proxyd 通过文件 capability 获得 CAP_NET_ADMIN，但 Linux 在 exec 普通子程序时
	// 不会默认继承该能力。Go 的 AmbientCaps 会在 fork 后把父进程 permitted 集中的
	// CAP_NET_ADMIN 加入 inheritable/ambient 集，再执行 nft/ip；否则 setcap 指引看似
	// 通过预检，真正调用子程序时仍会因权限不足失败。TPROXY 要求 Linux 4.18+，已
	// 高于 ambient capability 的 4.3 内核门槛。
	cmd.SysProcAttr = &syscall.SysProcAttr{AmbientCaps: []uintptr{capNetAdmin}}
	return cmd
}

// runCommand 执行网关外部命令，并把 input 写入 stdin。
//
// 参数：input 为标准输入文本；name 为程序名；args 为命令行参数。
// 返回值：合并后的 stdout/stderr 与执行错误；成功时错误为 nil。
// 错误情况：程序不存在、ambient capability 提升失败、退出码非零等均包装命令上下文返回。
// 定义为包级变量是为了让 Linux 执行层测试替换真实系统命令。
var runCommand = func(input, name string, args ...string) (string, error) {
	cmd := newGatewayCommand(name, args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// tproxyRuleMark 返回 iproute2 接受的 mark/mask 文本，并与 render.go 写入报文的
// mark 共用常量，防止 nftables 与策略路由在后续修改中悄然失配。
//
// 参数：无。
// 返回值：string，十六进制 `mark/mask` 文本。
// 错误情况：无；两个值均为编译期常量。
func tproxyRuleMark() string {
	return fmt.Sprintf("0x%x/0x%x", linuxTProxyMark, linuxTProxyMarkMask)
}

// isMissingRoutingObject 判断 iproute2 删除命令是否只是目标不存在。
//
// 参数：output 为 ip 命令 stdout/stderr 合并文本。
// 返回值：bool，仅对不同 iproute2 版本常见的“不存在”文案返回 true。
// 错误情况：无；未知错误保守返回 false，避免把真实权限或参数错误当作幂等成功。
func isMissingRoutingObject(output string) bool {
	normalized := strings.ToLower(output)
	for _, fragment := range []string{"no such file", "no such process", "fib table does not exist", "cannot find"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

// clearNFTTable 删除 proxyd 独占的 nftables table。
//
// 参数：无。
// 返回值：error，table 已不存在时返回 nil，其他 nft 执行错误原样包装。
// 错误情况：nft 缺失、权限不足或内核拒绝删除时返回错误；不会操作其他 table。
func clearNFTTable() error {
	out, err := runCommand("", "nft", "delete", "table", "inet", nftTableName)
	if err == nil || isMissingRoutingObject(out) {
		return nil
	}
	return fmt.Errorf("清除 nftables 规则失败: %w", err)
}

// clearPolicyRouting 删除 proxyd 为 UDP TPROXY 安装的精确规则与本地路由。
//
// 参数：无。
// 返回值：error，任一非“不存在”错误都会返回；多个失败通过 errors.Join 保留。
// 错误情况：规则/路由已不存在视为成功；权限不足、ip 缺失或参数不兼容会返回错误。
func clearPolicyRouting() error {
	var result error
	if out, err := runCommand("", "ip", "-4", "rule", "del", "priority", linuxTProxyRulePriority, "fwmark", tproxyRuleMark(), "lookup", linuxTProxyRouteTable); err != nil && !isMissingRoutingObject(out) {
		result = errors.Join(result, fmt.Errorf("删除 TPROXY 策略规则失败: %w", err))
	}
	if out, err := runCommand("", "ip", "-4", "route", "del", "local", "0.0.0.0/0", "dev", "lo", "table", linuxTProxyRouteTable); err != nil && !isMissingRoutingObject(out) {
		result = errors.Join(result, fmt.Errorf("删除 TPROXY 本地路由失败: %w", err))
	}
	return result
}

// clearLinuxDataplane 按“停止产生 mark → 移除 mark 路由”的顺序清理 Linux 数据面。
//
// 参数：无。
// 返回值：error，nftables 或策略路由清理失败时返回，多个错误通过 errors.Join 保留。
// 错误情况：所有目标都不存在时仍成功；只删除 proxyd 的 table、精确 rule 与精确 route。
func clearLinuxDataplane() error {
	return errors.Join(clearNFTTable(), clearPolicyRouting())
}

// applyPolicyRouting 幂等安装 UDP TPROXY 所需的本地路由与 fwmark 规则。
//
// 参数：无。
// 返回值：error，iproute2 命令失败时返回带具体阶段的错误。
// 错误情况：先用 route replace 保证路由幂等；规则使用精确 del+add 防止重复。
// 中途失败后的完整数据面清理由 nftRunner.Apply 统一负责，保证旧 table 也会被移除。
func applyPolicyRouting() error {
	if _, err := runCommand("", "ip", "-4", "route", "replace", "local", "0.0.0.0/0", "dev", "lo", "table", linuxTProxyRouteTable); err != nil {
		return fmt.Errorf("安装 TPROXY 本地路由失败: %w", err)
	}
	if out, err := runCommand("", "ip", "-4", "rule", "del", "priority", linuxTProxyRulePriority, "fwmark", tproxyRuleMark(), "lookup", linuxTProxyRouteTable); err != nil && !isMissingRoutingObject(out) {
		return fmt.Errorf("更新 TPROXY 策略规则前清理旧规则失败: %w", err)
	}
	if _, err := runCommand("", "ip", "-4", "rule", "add", "priority", linuxTProxyRulePriority, "fwmark", tproxyRuleMark(), "lookup", linuxTProxyRouteTable); err != nil {
		return fmt.Errorf("安装 TPROXY 策略规则失败: %w", err)
	}
	return nil
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
	// 先准备本地路由再启用 nftables 截获，避免规则已命中但 mark 尚无路由的短暂黑洞。
	if err := applyPolicyRouting(); err != nil {
		// 热更新时磁盘上可能已有上一版 proxyd-gw table。策略路由准备失败后也要
		// 清理旧 table，否则旧规则仍给 UDP 打 mark，而配套路由已被回滚，会造成黑洞。
		if clearErr := clearLinuxDataplane(); clearErr != nil {
			return errors.Join(err, fmt.Errorf("策略路由失败后清理 Linux 网关数据面失败: %w", clearErr))
		}
		return err
	}
	// nft ruleset 文件中的 add table/add chain 对既有对象是幂等的，但 add rule 会
	// 继续追加规则。加载前必须精确删除 proxyd 自有 table，才能保证配置刷新不会
	// 积累重复 redirect/tproxy 规则；删除与新 ruleset 加载之间不触碰其他 table。
	if err := clearNFTTable(); err != nil {
		joined := errors.Join(err, clearPolicyRouting())
		return joined
	}
	if _, err := runCommand(text, "nft", "-f", "-"); err != nil {
		joined := fmt.Errorf("应用 nftables 规则失败: %w", err)
		if clearErr := clearLinuxDataplane(); clearErr != nil {
			joined = errors.Join(joined, fmt.Errorf("应用失败后清理 Linux 网关数据面失败: %w", clearErr))
		}
		return joined
	}
	return nil
}

// Clear 先删除 proxyd-gw 表阻止产生新 mark，再清除配套 ip rule 与本地路由；
// 任一对象不存在都视为成功，其他错误合并返回。
func (nftRunner) Clear() error {
	return clearLinuxDataplane()
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

// policyRoutingPresent 检查专用 ip rule 与本地路由是否同时存在。
//
// 参数：无。
// 返回值：bool，两次只读查询均成功且包含本模块精确标识时返回 true。
// 错误情况：ip 命令缺失、查询失败或输出不完整均折叠为 false，详细原因由诊断项呈现。
func policyRoutingPresent() bool {
	rules, ruleErr := runCommand("", "ip", "-4", "rule", "show")
	routes, routeErr := runCommand("", "ip", "-4", "route", "show", "table", linuxTProxyRouteTable)
	return ruleErr == nil && routeErr == nil &&
		strings.Contains(rules, tproxyRuleMark()) && strings.Contains(rules, "lookup "+linuxTProxyRouteTable) &&
		strings.Contains(routes, "local") && strings.Contains(routes, "dev lo")
}

// Status 返回 ip_forward、proxyd-gw 表与 TPROXY 策略路由的可读摘要。
func (nftRunner) Status() string {
	forwarding := "off"
	if readSysctl(ipForwardPath) == "1" {
		forwarding = "on"
	}
	table := "absent"
	if _, err := runCommand("", "nft", "list", "table", "inet", nftTableName); err == nil {
		table = "present"
	}
	routing := "absent"
	if policyRoutingPresent() {
		routing = "present"
	}
	return fmt.Sprintf("ip_forward=%s table=%s policy_routing=%s", forwarding, table, routing)
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
	if policyRoutingPresent() {
		steps = append(steps, DiagnosticCheck{ID: "gateway_policy_route", Status: "passed", Detail: "UDP TPROXY 策略路由已载入"})
	} else {
		steps = append(steps, DiagnosticCheck{ID: "gateway_policy_route", Status: "failed", Detail: "UDP TPROXY 缺少 fwmark 规则或本地路由"})
	}
	return steps
}
