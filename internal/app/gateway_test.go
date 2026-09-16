package app

// 网关模块编排层用例：模块注册、启停事务、失败回滚、proxy 联动与设备增删改查。
// 运行平台为 darwin 时 helper 未安装是预期状态，applyGatewayRuntime 映射为
// degraded + 安装指引而非 failed，配置事务照常提交（helper 装好后经重试收敛）。

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"proxyd/internal/config"
	"proxyd/internal/gateway"
)

// gatewayTestDevice 返回一台合法的测试设备。
func gatewayTestDevice() config.GatewayDevice {
	return config.GatewayDevice{Name: "电视", IP: "192.168.1.10", Policy: "proxy"}
}

// gatewayModuleState 取 Modules() 中的 gateway 项。
func gatewayModuleState(t *testing.T, a *App) ModuleState {
	t.Helper()
	for _, state := range a.Modules() {
		if state.ID == "gateway" {
			return state
		}
	}
	t.Fatal("Modules() 缺少 gateway 模块")
	return ModuleState{}
}

func TestModulesIncludesGateway(t *testing.T) {
	a := newSystemProxyTestApp(t, filepath.Join(t.TempDir(), "config.yaml"))
	modules := a.Modules()
	if len(modules) != 3 {
		t.Fatalf("模块数 = %d, 期望 3", len(modules))
	}
	state := gatewayModuleState(t, a)
	if state.Name != "网关" {
		t.Errorf("名称 = %q", state.Name)
	}
	// 零值配置 Disabled=false，设备表为空 → enabled 但 idle 不生效。
	if !state.Enabled {
		t.Error("零值配置应显示 enabled（设备表为空时不生效）")
	}
}

// TestSetGatewayEnabledOnDarwinDegradedButCommitted 验证 macOS 缺少特权 helper 时，
// 网关启用请求会提交配置并进入可重试的 degraded 状态，而不是回滚用户意图。
//
// 参数说明：
//   - t: *testing.T，提供临时配置目录、清理回调和断言报告。
//
// 返回值说明：无；通过 testing 状态报告成功、跳过或失败。
//
// 错误情况：非 macOS 或当前机器已经安装 helper 时跳过；降级相位、安装指引、重试
// 时间或配置持久化不符合约定时令测试失败。
func TestSetGatewayEnabledOnDarwinDegradedButCommitted(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("helper 缺失路径的平台相关断言")
	}
	// 本用例只验证 helper 缺失时的降级事务；开发机若已经安装 helper，继续执行会把
	// 测试设备规则下发到真实特权进程，既无法得到预期错误，也破坏测试环境隔离。
	// Precheck 只完成协议握手、不修改系统状态；helper 可用时由底层 gateway 集成测试
	// 覆盖正常路径，这里跳过依赖外部安装状态的反向场景。
	if precheck := gateway.Precheck(); precheck.Ready {
		t.Skip("gateway helper 已安装，跳过仅适用于 helper 缺失环境的降级断言")
	}
	a := newSystemProxyTestApp(t, filepath.Join(t.TempDir(), "config.yaml"))
	a.cfg.ManualNodes = []any{"http://127.0.0.1:9#local"}
	a.cfg.Gateway.Devices = []config.GatewayDevice{gatewayTestDevice()}

	if err := a.SetGatewayEnabled(true); err != nil {
		t.Fatalf("helper 缺失应映射为 degraded 而非事务失败: %v", err)
	}
	state := gatewayModuleState(t, a)
	if state.Phase != "degraded" {
		t.Fatalf("phase = %q, 期望 degraded: %+v", state.Phase, state)
	}
	if !strings.Contains(state.Error, "helper install") {
		t.Errorf("状态消息应含安装指引: %q", state.Error)
	}
	if state.NextRetryAt == nil {
		t.Error("degraded 应安排重试")
	}
	// 配置照常提交并落盘。
	saved, err := config.Load(a.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Gateway.Disabled || len(saved.Gateway.Devices) != 1 {
		t.Errorf("落盘配置 = %+v", saved.Gateway)
	}

	// 停用：清除规则（helper 不可达时尽力而为）+ 落盘。
	if err := a.SetGatewayEnabled(false); err != nil {
		t.Fatalf("停用: %v", err)
	}
	if state := gatewayModuleState(t, a); state.Enabled || state.Phase != "disabled" {
		t.Errorf("停用后状态 = %+v", state)
	}
	saved, err = config.Load(a.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Gateway.Disabled {
		t.Error("停用未持久化")
	}
}

func TestSetGatewayEnabledRequiresProxy(t *testing.T) {
	a := newSystemProxyTestApp(t, filepath.Join(t.TempDir(), "config.yaml"))
	a.cfg.ProxyDisabled = true
	a.cfg.Gateway.Disabled = true
	err := a.SetGatewayEnabled(true)
	if err == nil || !strings.Contains(err.Error(), "代理模块") {
		t.Fatalf("代理停用时应拒绝启用网关: %v", err)
	}
	if !a.Config().Gateway.Disabled {
		t.Error("拒绝后网关开关不应变化")
	}
}

func TestProxyDisableStopsGateway(t *testing.T) {
	a := newSystemProxyTestApp(t, filepath.Join(t.TempDir(), "config.yaml"))
	a.cfg.ManualNodes = []any{"http://127.0.0.1:9#local"}
	a.cfg.Gateway.Devices = []config.GatewayDevice{gatewayTestDevice()}
	if err := a.SetGatewayEnabled(true); err != nil {
		t.Fatalf("启用网关: %v", err)
	}

	if err := a.SetModuleEnabled("proxy", false); err != nil {
		t.Fatalf("停用代理: %v", err)
	}
	// ADR 0003 联动：gateway 已被先行停用并持久化。
	if !a.Config().Gateway.Disabled {
		t.Fatal("停用代理未联动停用网关")
	}
	saved, err := config.Load(a.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Gateway.Disabled || !saved.ProxyDisabled {
		t.Errorf("落盘配置 = gateway.Disabled=%t proxy-disabled=%t", saved.Gateway.Disabled, saved.ProxyDisabled)
	}
	if state := gatewayModuleState(t, a); state.Enabled || state.Phase != "disabled" {
		t.Errorf("联动停用后网关状态 = %+v", state)
	}

	// 代理恢复后网关不自动复活（保持用户可见的显式开关语义）。
	if err := a.SetModuleEnabled("proxy", true); err != nil {
		t.Fatalf("恢复代理: %v", err)
	}
	if !a.Config().Gateway.Disabled {
		t.Error("代理恢复后网关不应自动启用")
	}
}

func TestGatewayDeviceCRUD(t *testing.T) {
	a := newSystemProxyTestApp(t, filepath.Join(t.TempDir(), "config.yaml"))
	a.cfg.ManualNodes = []any{"http://127.0.0.1:9#local"}

	if err := a.AddGatewayDevice(gatewayTestDevice()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := a.Config().Gateway.Devices; len(got) != 1 || got[0].Name != "电视" {
		t.Fatalf("devices = %+v", got)
	}
	saved, err := config.Load(a.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Gateway.Devices) != 1 {
		t.Fatal("设备未持久化")
	}

	// 重名拒绝。
	if err := a.AddGatewayDevice(gatewayTestDevice()); err == nil {
		t.Fatal("重名设备应被拒绝")
	}
	// IP 重复拒绝。
	dup := gatewayTestDevice()
	dup.Name = "另一台"
	if err := a.AddGatewayDevice(dup); err == nil {
		t.Fatal("IP 重复应被拒绝")
	}
	// 非法 IP 拒绝。
	bad := config.GatewayDevice{Name: "坏设备", IP: "999.1.1.1"}
	if err := a.AddGatewayDevice(bad); err == nil {
		t.Fatal("非法 IP 应被拒绝")
	}

	// 更新（不改名）。
	if err := a.UpdateGatewayDevice("电视", config.GatewayDevice{Name: "电视", IP: "192.168.1.10", Policy: "direct"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := a.Config().Gateway.Devices[0].Policy; got != "direct" {
		t.Errorf("policy = %q", got)
	}
	// 改名拒绝。
	if err := a.UpdateGatewayDevice("电视", config.GatewayDevice{Name: "新名字", IP: "192.168.1.10"}); err == nil {
		t.Fatal("改名应被拒绝")
	}
	// 不存在的设备。
	if err := a.UpdateGatewayDevice("不存在", config.GatewayDevice{Name: "不存在", IP: "10.0.0.1"}); err == nil {
		t.Fatal("更新不存在的设备应报错")
	}

	// 删除。
	if err := a.RemoveGatewayDevice("电视"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := a.Config().Gateway.Devices; len(got) != 0 {
		t.Errorf("devices = %+v", got)
	}
	if err := a.RemoveGatewayDevice("电视"); err == nil {
		t.Fatal("删除不存在的设备应报错")
	}
	saved, err = config.Load(a.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Gateway.Devices) != 0 {
		t.Error("删除未持久化")
	}
}

func TestGatewayTransactionRollbackOnPersistFailure(t *testing.T) {
	// cfgPath 指向目录：persistLocked 落盘必失败。
	a := newSystemProxyTestApp(t, t.TempDir())

	if err := a.AddGatewayDevice(gatewayTestDevice()); err == nil {
		t.Fatal("落盘失败应返回错误")
	}
	if got := a.Config().Gateway.Devices; len(got) != 0 {
		t.Fatalf("落盘失败后设备未回滚: %+v", got)
	}

	if err := a.SetGatewayEnabled(false); err == nil {
		t.Fatal("停用落盘失败应返回错误")
	}
	if a.Config().Gateway.Disabled {
		t.Fatal("停用失败后开关未回滚")
	}
}

func TestGatewayRuntimeFailureRollsBack(t *testing.T) {
	a := newSystemProxyTestApp(t, filepath.Join(t.TempDir(), "config.yaml"))
	// 关闭 Manager 使 Apply 必然返回非 helper 类错误，覆盖事务回滚路径。
	a.gateway.Close()

	err := a.AddGatewayDevice(gatewayTestDevice())
	if err == nil {
		t.Fatal("执行层失败应返回错误")
	}
	if got := a.Config().Gateway.Devices; len(got) != 0 {
		t.Fatalf("执行层失败后设备未回滚: %+v", got)
	}
}

func TestRetryModuleGateway(t *testing.T) {
	a := newSystemProxyTestApp(t, filepath.Join(t.TempDir(), "config.yaml"))
	if err := a.RetryModule(t.Context(), "gateway"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	a.cfg.Gateway.Disabled = true
	if err := a.RetryModule(t.Context(), "gateway"); err == nil {
		t.Fatal("已禁用模块应拒绝重试")
	}
}
