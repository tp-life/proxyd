package gateway

// 网关模块诊断步骤：平台支持性、执行层握手/权限、转发开关实际值、
// 规则应用状态与 redir 入口监听。Detail 只用固定文案（docs/CONTEXT.md 约定），
// 不回传原始错误或环境输出；平台专属检查经 diagnoseRunner 接口下沉到执行层。

import (
	"context"
	"fmt"
	"net"
	"time"

	"proxyd/internal/config"
)

// DiagnosticCheck 是网关适配器的稳定诊断结果，形状与 remote.DiagnosticCheck 一致，
// 由应用层转换为 diagnostics.Step，不向调用方暴露执行层对象。
type DiagnosticCheck struct {
	ID, Status, Detail string
	DurationMS         int64
}

// diagnoseRunner 是支持平台专属诊断的执行层（macOS helper 握手/转发实际值，
// Linux 能力位/nft 表/ip_forward）。
type diagnoseRunner interface {
	DiagnoseGateway(ctx context.Context) []DiagnosticCheck
}

// Diagnose 执行网关模块的本机检查。
//
// 参数：
//   - ctx: context.Context，总期限；单项网络/拨号检查自带短超时。
//
// 返回值：
//   - []DiagnosticCheck：模块禁用或平台不支持时后续步骤标记 skipped。
//
// 错误情况：不返回 error；单项失败以 failed 状态与固定指引文案表达。
func (m *Manager) Diagnose(ctx context.Context) []DiagnosticCheck {
	m.mu.Lock()
	cfg := m.cfg.Clone()
	enabled := m.enabled
	applied := m.applied
	runner := m.runner
	m.mu.Unlock()

	steps := []DiagnosticCheck{}
	if isUnsupported() {
		return append(steps, DiagnosticCheck{ID: "gateway_platform", Status: "failed", Detail: "当前平台不支持 LAN 网关"})
	}
	steps = append(steps, DiagnosticCheck{ID: "gateway_platform", Status: "passed", Detail: "平台支持 LAN 网关"})
	if cfg.Disabled || !enabled {
		return append(steps,
			DiagnosticCheck{ID: "gateway_runtime", Status: "skipped", Detail: "网关模块已禁用"},
			DiagnosticCheck{ID: "gateway_rules", Status: "skipped", Detail: "网关模块已禁用"},
		)
	}
	if len(cfg.Devices) == 0 {
		steps = append(steps, DiagnosticCheck{ID: "gateway_devices", Status: "skipped", Detail: "设备表为空，登记设备后网关才开始分流"})
	}
	if extra, ok := runner.(diagnoseRunner); ok {
		steps = append(steps, extra.DiagnoseGateway(ctx)...)
	}
	if applied {
		steps = append(steps, DiagnosticCheck{ID: "gateway_rules", Status: "passed", Detail: "转发规则已应用"})
		steps = append(steps, diagnoseRedirListen(cfg))
	} else {
		steps = append(steps, DiagnosticCheck{ID: "gateway_rules", Status: "failed", Detail: "转发规则未应用，请查看网关页状态与运行日志"})
	}
	return steps
}

// diagnoseRedirListen 探测 mihomo redir 入口是否在本机监听（规则已应用但入口未监听
// 时下游连接会被重置）。
//
// 参数：
//   - cfg: config.GatewayConfig，当前网关配置。
//
// 返回值：DiagnosticCheck，监听可达为 passed，否则 failed 并给出固定指引。
//
// 错误情况：拨号失败折叠为 failed 状态，不外泄原始错误。
func diagnoseRedirListen(cfg config.GatewayConfig) DiagnosticCheck {
	address := fmt.Sprintf("127.0.0.1:%d", cfg.EffectiveRedirPort())
	start := time.Now()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		return DiagnosticCheck{ID: "gateway_redir", Status: "failed", Detail: "redir 入口未监听，请确认代理模块运行正常", DurationMS: time.Since(start).Milliseconds()}
	}
	_ = conn.Close()
	return DiagnosticCheck{ID: "gateway_redir", Status: "passed", Detail: "redir 入口正在监听", DurationMS: time.Since(start).Milliseconds()}
}
