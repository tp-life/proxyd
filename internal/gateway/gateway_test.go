package gateway

import (
	"errors"
	"strings"
	"testing"
	"time"

	"proxyd/internal/config"
)

// fakeRunner 记录执行层调用序列，供 Manager 调和逻辑测试。
type fakeRunner struct {
	applied    []string
	cleared    int
	forwarding []bool
	applyErr   error
}

func (f *fakeRunner) Apply(text string) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, text)
	return nil
}

func (f *fakeRunner) Clear() error {
	f.cleared++
	return nil
}

func (f *fakeRunner) Forwarding(enable bool) error {
	f.forwarding = append(f.forwarding, enable)
	return nil
}

func (f *fakeRunner) Status() string { return "fake" }

func TestManagerApplyAndClose(t *testing.T) {
	runner := &fakeRunner{}
	m := NewManager(t.TempDir(), nil)
	m.runner = runner

	if err := m.Apply(config.GatewayConfig{
		Devices: []config.GatewayDevice{{Name: "a", IP: "192.168.1.10"}},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(runner.applied) != 1 || len(runner.forwarding) != 1 || !runner.forwarding[0] {
		t.Fatalf("执行层调用 = %+v", runner)
	}
	status := m.Status()
	if !status.Enabled || !status.Applied || !status.Forwarding || status.DeviceNum != 1 || status.Err != "" {
		t.Errorf("Status = %+v", status)
	}

	m.Close()
	if runner.cleared != 1 {
		t.Errorf("Close 应清除一次规则, cleared=%d", runner.cleared)
	}
	if s := m.Status(); s.Applied {
		t.Error("Close 后 Applied 应为 false")
	}
	m.Close() // 幂等
	if runner.cleared != 1 {
		t.Errorf("重复 Close 不应再次清除, cleared=%d", runner.cleared)
	}
	if err := m.Apply(config.GatewayConfig{}); err == nil {
		t.Error("Close 后 Apply 应报错")
	}
}

func TestManagerApplyDisabled(t *testing.T) {
	runner := &fakeRunner{}
	m := NewManager(t.TempDir(), nil)
	m.runner = runner

	if err := m.Apply(config.GatewayConfig{Disabled: true}); err != nil {
		t.Fatalf("Apply(disabled): %v", err)
	}
	if len(runner.applied) != 0 {
		t.Errorf("停用配置不应应用规则: %+v", runner)
	}
	if s := m.Status(); s.Enabled || s.Applied {
		t.Errorf("Status = %+v", s)
	}

	// 先启用再停用：应清除规则。
	if err := m.Apply(config.GatewayConfig{Devices: []config.GatewayDevice{{Name: "a", IP: "10.0.0.1"}}}); err != nil {
		t.Fatalf("Apply(enable): %v", err)
	}
	if err := m.Apply(config.GatewayConfig{Disabled: true}); err != nil {
		t.Fatalf("Apply(disable): %v", err)
	}
	if runner.cleared != 1 {
		t.Errorf("停用应清除已应用规则, cleared=%d", runner.cleared)
	}
}

func TestManagerApplyFailureRecorded(t *testing.T) {
	runner := &fakeRunner{applyErr: errors.New("boom")}
	m := NewManager(t.TempDir(), nil)
	m.runner = runner
	if err := m.Apply(config.GatewayConfig{}); err == nil {
		t.Fatal("执行层失败应返回错误")
	}
	if s := m.Status(); s.Err != "boom" || s.Applied {
		t.Errorf("Status = %+v", s)
	}
}

// forwardingFailureRunner 模拟规则已应用、但开启 IPv4 转发失败的执行层，
// 用于验证 Manager 不会留下“已截获但无法转发”的半套运行态。
type forwardingFailureRunner struct {
	fakeRunner
}

// Forwarding 固定返回模拟错误。
//
// 参数：enable 表示目标转发状态，本测试只记录调用而不触碰系统。
// 返回值：error，始终返回测试错误。
// 错误情况：无额外分支；固定失败是该 fake 的唯一职责。
func (f *forwardingFailureRunner) Forwarding(enable bool) error {
	f.forwarding = append(f.forwarding, enable)
	return errors.New("forwarding boom")
}

// TestManagerForwardingFailureClearsAppliedRules 验证开启 IPv4 转发失败后，
// Manager 会立即清理已应用规则并把运行态恢复为未应用。
//
// 参数：t 由 testing 注入，用于构造临时 Manager 并报告断言失败。
// 返回值：无。
// 错误情况：Apply 未失败、未清理规则或状态仍显示已应用时测试失败。
func TestManagerForwardingFailureClearsAppliedRules(t *testing.T) {
	runner := &forwardingFailureRunner{}
	m := NewManager(t.TempDir(), nil)
	m.runner = runner

	err := m.Apply(config.GatewayConfig{Devices: []config.GatewayDevice{{Name: "a", IP: "192.168.1.10"}}})
	if err == nil || !strings.Contains(err.Error(), "forwarding boom") {
		t.Fatalf("Apply 错误 = %v", err)
	}
	if runner.cleared != 1 {
		t.Fatalf("转发开启失败后清理次数 = %d, 期望 1", runner.cleared)
	}
	status := m.Status()
	if status.Applied || status.Forwarding || status.Err == "" {
		t.Fatalf("失败后状态 = %+v", status)
	}
}

// pingingRunner 在 fakeRunner 上实现 heartbeatPinger，验证看门狗心跳生命周期。
type pingingRunner struct {
	fakeRunner
	pings chan struct{}
}

func (p *pingingRunner) Ping() error {
	select {
	case p.pings <- struct{}{}:
	default:
	}
	return nil
}

func TestManagerHeartbeatLifecycle(t *testing.T) {
	orig := helperHeartbeatInterval
	helperHeartbeatInterval = 5 * time.Millisecond
	defer func() { helperHeartbeatInterval = orig }()

	runner := &pingingRunner{pings: make(chan struct{}, 16)}
	m := NewManager(t.TempDir(), nil)
	m.runner = runner

	if err := m.Apply(config.GatewayConfig{
		Devices: []config.GatewayDevice{{Name: "a", IP: "192.168.1.10"}},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 规则应用后心跳协程启动。
	select {
	case <-runner.pings:
	case <-time.After(2 * time.Second):
		t.Fatal("Apply 成功后应开始心跳")
	}
	// 停用心跳随规则清除停止。先等待可能在飞的最后一次心跳落库再排空，
	// 随后观察静默窗口确认协程已退出。
	if err := m.Apply(config.GatewayConfig{Disabled: true}); err != nil {
		t.Fatalf("停用: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	drainPings(runner.pings)
	select {
	case <-runner.pings:
		t.Fatal("停用后不应再有心跳")
	case <-time.After(30 * time.Millisecond):
	}
}

// drainPings 清空心跳通道中积存的信号。
func drainPings(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
