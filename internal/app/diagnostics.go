package app

// 诊断用例统一 Web 与 CLI 的结果，不把网络适配器或原始配置暴露给界面。
import (
	"context"
	"fmt"
	"proxyd/internal/diagnostics"
	"time"
)

// Diagnose 检查本机能力及可选命名设备；参数 ctx 为取消上下文、peer 为保存的设备名或空。
// 返回 Report/error；一次仅允许一个诊断，防止重复点击持续占用隧道与 shell；未知设备拒绝执行。
func (a *App) Diagnose(ctx context.Context, peer string) (diagnostics.Report, error) {
	report := diagnostics.Report{CreatedAt: time.Now().UTC(), Scope: "local", Steps: []diagnostics.Step{}}
	if !a.diagnosing.CompareAndSwap(false, true) {
		return report, fmt.Errorf("已有诊断正在运行，请稍后重试")
	}
	defer a.diagnosing.Store(false)
	cfg := a.Config()
	token := ""
	if peer != "" {
		for _, entry := range cfg.Remote.Remotes {
			if entry.Name == peer {
				token = entry.Token
				break
			}
		}
		if token == "" {
			return report, fmt.Errorf("请选择已保存的设备名称")
		}
		report.Scope = "saved-device"
	}
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	for _, module := range a.Modules() {
		status, detail := "passed", "模块运行正常"
		if !module.Enabled {
			status, detail = "skipped", "模块已禁用"
		} else if module.Phase == "failed" || module.Phase == "degraded" || module.Phase == "retrying" {
			status, detail = "failed", module.Error
		} else if !module.Running {
			status, detail = "skipped", "模块尚未运行或没有启用的子服务"
		}
		report.Steps = append(report.Steps, diagnostics.Step{ID: "module_" + module.ID, Status: status, Detail: detail})
	}
	for _, check := range a.remote.DiagnoseNetwork(ctx) {
		report.Steps = append(report.Steps, diagnostics.Step{ID: check.ID, Status: check.Status, Detail: check.Detail, DurationMS: check.DurationMS})
	}
	for _, check := range a.gateway.Diagnose(ctx) {
		report.Steps = append(report.Steps, diagnostics.Step{ID: check.ID, Status: check.Status, Detail: check.Detail, DurationMS: check.DurationMS})
	}
	if token != "" {
		for _, check := range a.remote.DiagnoseSSH(ctx, token) {
			report.Steps = append(report.Steps, diagnostics.Step{ID: check.ID, Status: check.Status, Detail: check.Detail, DurationMS: check.DurationMS})
		}
	}
	return report, nil
}
