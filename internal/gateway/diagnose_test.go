package gateway

// Diagnose 步骤形态单测：禁用/空设备表/未应用规则的固定文案与 skipped 语义。
// 平台专属步骤由执行层 DiagnoseGateway 覆盖（fake runner 不实现该接口即不出现）。

import (
	"runtime"
	"testing"

	"proxyd/internal/config"
)

func TestManagerDiagnoseShapes(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("未适配平台的步骤形态不同")
	}
	m := NewManager(t.TempDir(), nil)
	m.runner = &fakeRunner{}

	// 禁用：平台步骤 passed，后续 skipped。
	steps := m.Diagnose(t.Context())
	if len(steps) < 3 || steps[0].ID != "gateway_platform" || steps[0].Status != "passed" {
		t.Fatalf("平台步骤异常: %+v", steps)
	}
	if steps[1].Status != "skipped" || steps[2].Status != "skipped" {
		t.Errorf("禁用时应 skipped: %+v", steps)
	}

	// 启用但规则未应用：gateway_rules failed 且带固定指引。
	m.cfg = config.GatewayConfig{Devices: []config.GatewayDevice{{Name: "a", IP: "192.168.1.10"}}}
	m.enabled = true
	steps = m.Diagnose(t.Context())
	var rulesStep *DiagnosticCheck
	for i := range steps {
		if steps[i].ID == "gateway_rules" {
			rulesStep = &steps[i]
		}
	}
	if rulesStep == nil || rulesStep.Status != "failed" {
		t.Fatalf("规则步骤异常: %+v", steps)
	}
}

func TestDiagnoseRedirListen(t *testing.T) {
	// 无监听端口：failed 且固定文案。
	step := diagnoseRedirListen(config.GatewayConfig{RedirPort: 1})
	if step.ID != "gateway_redir" || step.Status != "failed" {
		t.Errorf("未监听时应 failed: %+v", step)
	}
}
