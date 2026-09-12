//go:build !linux && !darwin

package gateway

// 未适配平台（Windows 等）的执行层：全部操作返回 ErrUnsupported，
// 网关模块在这些平台上报告不支持并保持停用（docs/adr/0003）。

import (
	"runtime"

	"proxyd/internal/config"
)

// unsupportedRunner 是未适配平台的 Runner 实现。
type unsupportedRunner struct{}

// newRunner 选定未适配平台的执行层。
func newRunner() Runner { return unsupportedRunner{} }

// isUnsupported 报告当前平台不受支持；未适配平台恒为 true。
func isUnsupported() bool { return true }

// renderRules 在未适配平台不产生规则文本。
func renderRules(config.GatewayConfig, int) string { return "" }

// Precheck 在未适配平台直接报告不支持。
func Precheck() PrecheckResult {
	return PrecheckResult{Platform: runtime.GOOS, Supported: false, Detail: ErrUnsupported.Error()}
}

// Apply 返回 ErrUnsupported。
func (unsupportedRunner) Apply(string) error { return ErrUnsupported }

// Clear 返回 ErrUnsupported。
func (unsupportedRunner) Clear() error { return ErrUnsupported }

// Forwarding 返回 ErrUnsupported。
func (unsupportedRunner) Forwarding(bool) error { return ErrUnsupported }

// Status 返回不支持提示。
func (unsupportedRunner) Status() string { return ErrUnsupported.Error() }
