package app

// 本文件承载「LAN 网关」旁路由模块的编排逻辑（docs/adr/0003）。
// 数据面寄生于 proxy 模块的 mihomo（redir/tproxy 入口与设备 SRC-IP-CIDR 规则由
// core 生成）；本层负责特权执行面（pf anchor / nftables / IPv4 转发）的调和、
// 配置事务与模块生命周期相位。helper 不可达在本阶段是预期状态，映射为
// degraded + 安装指引而非 failed。

import (
	"errors"
	"fmt"
	"log"
	"runtime"
	"strings"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/gateway"
	"proxyd/internal/lifecycle"
)

// initGateway 创建网关管理器；在 New 中调用。
func (a *App) initGateway() {
	a.gateway = gateway.NewManager(a.cfg.StateDir, func(format string, args ...any) {
		log.Printf(format, args...)
	})
}

// startGateway 在 Run 启动阶段按当前配置调和一次网关执行层；失败仅记录日志，
// 不影响代理主功能启动（helper 缺失等可恢复状态由生命周期相位表达）。
func (a *App) startGateway() {
	// 与配置事务串行，避免启动调和覆盖用户刚提交的停用。
	a.gatewayMutationMu.Lock()
	defer a.gatewayMutationMu.Unlock()
	a.mu.RLock()
	cfg := a.cfg.Gateway.Clone()
	a.mu.RUnlock()
	if err := a.applyGatewayRuntime(cfg); err != nil {
		log.Printf("[gateway] 启动应用配置失败: %v", err)
	}
}

// stopGateway 停止网关模块并尽力清除已应用的转发规则（进程退出路径）。
func (a *App) stopGateway() {
	a.gateway.Close()
}

// applyGatewayRuntime 将网关配置应用与观测状态封装为单一用例。
//
// 参数说明：
//   - cfg: config.GatewayConfig，目标网关配置（独立副本）。
//
// 返回值说明：error，执行层明确失败（如 Linux nftables/能力位错误）时返回，
// 供配置事务回滚；helper 不可达、平台不支持、代理停用、设备表为空均返回 nil
// （状态经生命周期相位表达，配置可提交，后续可恢复）。
//
// 错误情况：相位映射规则——disabled=配置停用/平台不支持/代理随停；
// idle=设备表为空等待添加；degraded=helper 未安装（带安装指引与重试）；
// running=规则已应用；failed=执行层错误（返回 error 触发事务回滚）。
func (a *App) applyGatewayRuntime(cfg config.GatewayConfig) error {
	generation := a.gatewayLifecycle.Begin()
	complete := func(phase string, running bool, message string, retry time.Duration) {
		a.gatewayLifecycle.Complete(generation, phase, running, message, retry)
	}

	a.mu.RLock()
	proxyDisabled := a.cfg.ProxyDisabled
	a.mu.RUnlock()
	status := a.gateway.Status()

	// stopWith 清除执行层规则（若曾应用）并按给定相位收尾。
	stopWith := func(phase, message string) {
		if status.Applied {
			disabledCfg := cfg
			disabledCfg.Disabled = true
			if err := a.gateway.Apply(disabledCfg); err != nil {
				log.Printf("[gateway] 清除转发规则失败: %v", err)
			}
		}
		complete(phase, false, message, 0)
	}

	switch {
	case cfg.Disabled:
		stopWith("disabled", "")
		return nil
	case !status.Supported:
		stopWith("disabled", "当前平台不支持 LAN 网关（docs/adr/0003：Windows 不做网关）")
		return nil
	case proxyDisabled:
		// 数据面寄生于 mihomo；代理停用时网关同步停止（ADR 0003 联动）。
		stopWith("disabled", "代理模块已停用，网关同步停止")
		return nil
	case len(cfg.Devices) == 0:
		stopWith("idle", "设备表为空，添加设备后生效")
		return nil
	}

	err := a.gateway.Apply(cfg)
	switch {
	case err == nil:
		complete("running", true, "", 0)
		return nil
	case errors.Is(err, gateway.ErrUnsupported):
		complete("disabled", false, "当前平台不支持 LAN 网关（docs/adr/0003）", 0)
		return nil
	case errors.Is(err, gateway.ErrHelperUnavailable):
		// helper 缺失是可恢复状态：配置照常提交，装好后经重试/手动 Retry 收敛。
		complete("degraded", false, "统一特权助手未安装或未运行：请执行 sudo proxyd helper install 安装", 30*time.Second)
		return nil
	default:
		complete("failed", false, "网关转发规则应用失败: "+firstErrorLine(err.Error()), 30*time.Second)
		return err
	}
}

// mutateGatewayLocked 执行已串行化的网关配置事务；调用方必须持有 gatewayMutationMu。
//
// 参数说明：
//   - mutate: func(*config.Config) error，只作用于当前配置的独立克隆，
//     返回错误即中止事务（用于字段校验，如设备改名限制）。
//
// 返回值说明：error，校验、执行层调和、Regenerate 或落盘全部成功时返回 nil。
//
// 错误情况：任一步失败时恢复旧配置、旧 mihomo 运行态与旧执行层规则，
// 回滚错误与原始错误 errors.Join 返回。事务顺序为 Regenerate（先让 mihomo
// 备好 redir/dns 入口）→ applyGatewayRuntime（再下发 pf/nftables 规则），
// 避免规则先生效而入口未监听导致下游断流。
func (a *App) mutateGatewayLocked(mutate func(*config.Config) error) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	a.mu.Lock()
	old := a.cfg.Clone()
	next := a.cfg.Clone()
	if err := mutate(next); err != nil {
		a.mu.Unlock()
		return err
	}
	if err := next.CheckGateway(); err != nil {
		a.mu.Unlock()
		return err
	}
	a.cfg = next
	applyCfg := next.Gateway.Clone()
	a.mu.Unlock()

	if err := a.regenerateCurrentLocked(); err != nil {
		return a.rollbackGateway(old, fmt.Errorf("网关配置未通过 mihomo 校验: %w", err))
	}
	if err := a.applyGatewayRuntime(applyCfg); err != nil {
		return a.rollbackGateway(old, err)
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	if err != nil {
		return a.rollbackGateway(old, err)
	}
	return nil
}

// rollbackGateway 恢复旧网关配置、mihomo 运行态与执行层规则。
//
// 参数说明：
//   - old: *config.Config，事务开始前的完整配置快照。
//   - cause: error，触发回滚的原始错误。
//
// 返回值说明：error，始终包含 cause；恢复步骤失败时合并返回。
//
// 错误情况：调用方须持有 gatewayMutationMu 与 refreshing 锁，
// 保证回滚期间不会插入另一笔网关事务或刷新。
func (a *App) rollbackGateway(old *config.Config, cause error) error {
	// 先恢复内存配置，再按旧配置重生成与重调和，保证两个运行态都回到事务前。
	a.mu.Lock()
	a.cfg = old
	a.mu.Unlock()
	joined := cause
	if err := a.regenerateCurrentLocked(); err != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复旧 mihomo 配置失败: %w", err))
	}
	if err := a.applyGatewayRuntime(old.Gateway.Clone()); err != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复旧网关运行态失败: %w", err))
	}
	return joined
}

// reconcileGatewayLocked 在订阅刷新流水线尾部顺带调和网关执行层：上次应用失败
// 到点重试，或规则被外部清除（如 helper 看门狗）时重新收敛。调用方须持有
// refreshing 锁（与配置事务互斥，不会插进事务中间）。
//
// 参数：无。
//
// 返回值：无；失败仅记录日志，状态经生命周期相位与 NextRetryAt 表达。
//
// 错误情况：重试节奏遵循 lifecycle 的 NextRetryAt，避免每次刷新都拨号 helper。
func (a *App) reconcileGatewayLocked() {
	a.mu.RLock()
	cfg := a.cfg.Gateway.Clone()
	proxyDisabled := a.cfg.ProxyDisabled
	a.mu.RUnlock()
	status := a.gateway.Status()
	wantApplied := !cfg.Disabled && !proxyDisabled && status.Supported && len(cfg.Devices) > 0
	if status.Applied == wantApplied {
		return
	}
	if wantApplied {
		if next := a.gatewayLifecycle.Snapshot().NextRetryAt; next != nil && time.Now().Before(*next) {
			return
		}
	}
	if err := a.applyGatewayRuntime(cfg); err != nil {
		log.Printf("[gateway] 刷新顺带调和失败: %v", err)
	}
}

// stopGatewayForProxyDisableLocked 在禁用代理模块前联动停用网关（ADR 0003：
// 数据面寄生于 mihomo，禁 proxy 必须先停 gateway）。调用方须持有
// gatewayMutationMu；网关本就停用时直接返回，不产生多余落盘。
//
// 参数：无。
//
// 返回值：error，停用事务失败时返回，代理保持原状（先停 gateway 失败则不动 proxy）。
//
// 错误情况：回滚语义由 mutateGatewayLocked 保证。
func (a *App) stopGatewayForProxyDisableLocked() error {
	a.mu.RLock()
	gatewayActive := !a.cfg.Gateway.Disabled
	a.mu.RUnlock()
	if !gatewayActive {
		return nil
	}
	if err := a.mutateGatewayLocked(func(c *config.Config) error {
		c.Gateway.Disabled = true
		return nil
	}); err != nil {
		return fmt.Errorf("停用代理前自动停用网关失败: %w", err)
	}
	return nil
}

// GatewayStatus 返回网关模块的运行态快照。
//
// 参数：无。
//
// 返回值：gateway.Status，含平台、转发状态、规则应用状态与执行层错误；
// 模块生命周期相位（phase）经 Modules() 暴露。
//
// 错误情况：无。
func (a *App) GatewayStatus() gateway.Status {
	return a.gateway.Status()
}

// GatewayOverview 是网关模块状态接口的应用层视图：执行层快照 + 生命周期相位 +
// 生效端口与设备表。无凭据字段，可整体序列化到管理面。
type GatewayOverview struct {
	gateway.Status
	lifecycle.State
	RedirPort     int                    `json:"redir_port"`
	TProxyPort    int                    `json:"tproxy_port"`
	DNSRedirect   bool                   `json:"dns_redirect"`
	DNSListenPort int                    `json:"dns_listen_port"`
	Devices       []config.GatewayDevice `json:"devices"`
	Runner        string                 `json:"runner"` // 执行层自述状态（helper/nftables 摘要）
}

// GatewayOverview 返回网关模块的完整状态视图。
//
// 参数：无。
//
// 返回值：
//   - GatewayOverview：配置停用时相位强制为 disabled（与 Modules() 同口径）。
//
// 错误情况：无；执行层不可达折叠进 Runner 文本。
func (a *App) GatewayOverview() GatewayOverview {
	a.mu.RLock()
	cfg := a.cfg.Gateway.Clone()
	a.mu.RUnlock()
	if cfg.Devices == nil {
		// 保证 JSON 序列化为 [] 而非 null，前端无需判空。
		cfg.Devices = []config.GatewayDevice{}
	}
	state := a.gatewayLifecycle.Snapshot()
	if cfg.Disabled {
		state.Phase = "disabled"
		state.Running = false
		state.NextRetryAt = nil
	}
	return GatewayOverview{
		Status:        a.gateway.Status(),
		State:         state,
		RedirPort:     cfg.EffectiveRedirPort(),
		TProxyPort:    cfg.EffectiveTProxyPort(),
		DNSRedirect:   cfg.DNSRedirect,
		DNSListenPort: config.DefaultGatewayDNSListenPort,
		Devices:       cfg.Devices,
		Runner:        a.gateway.RunnerStatus(),
	}
}

// GatewayPrecheck 返回「启用前检查」结果（平台支持性、helper/能力位状态与修复指引）。
//
// 参数：无。
//
// 返回值：gateway.PrecheckResult；未就绪时 Detail 为可执行的中文修复指引。
//
// 错误情况：无；检测失败折叠进 Detail 文本。
func (a *App) GatewayPrecheck() gateway.PrecheckResult {
	return gateway.Precheck()
}

// SetGatewayEnabled 热切换网关模块总开关并持久化。
//
// 参数说明：
//   - on: bool，true 启用（写 disabled=false），false 停用并清除转发规则。
//
// 返回值说明：error，校验、调和、热更新或落盘失败时返回并整体回滚。
//
// 错误情况：Windows 等未适配平台拒绝启用（ADR 0003）；代理模块已停用时拒绝启用
// （网关数据面寄生于 mihomo）。
func (a *App) SetGatewayEnabled(on bool) error {
	if on && runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return fmt.Errorf("当前平台（%s）不支持 LAN 网关（docs/adr/0003：Windows 不做网关）", runtime.GOOS)
	}
	a.gatewayMutationMu.Lock()
	defer a.gatewayMutationMu.Unlock()
	return a.mutateGatewayLocked(func(c *config.Config) error {
		if on && c.ProxyDisabled {
			return fmt.Errorf("启用网关前请先启用代理模块（网关数据面寄生于 mihomo，见 docs/adr/0003）")
		}
		c.Gateway.Disabled = !on
		return nil
	})
}

// AddGatewayDevice 登记一台下游设备并持久化；设备规则随下次生成前置于 custom-rules。
//
// 参数说明：
//   - d: config.GatewayDevice，名称/IP/策略；MAC 仅用于识别展示。
//
// 返回值说明：error，重名/IP 重复/字段非法/调和失败时返回并回滚。
//
// 错误情况：去重与合法性由 config.CheckGateway 统一校验（含 policy 引用分组存在性）。
func (a *App) AddGatewayDevice(d config.GatewayDevice) error {
	d.Name = strings.TrimSpace(d.Name)
	d.IP = strings.TrimSpace(d.IP)
	d.Policy = strings.TrimSpace(d.Policy)
	a.gatewayMutationMu.Lock()
	defer a.gatewayMutationMu.Unlock()
	return a.mutateGatewayLocked(func(c *config.Config) error {
		c.Gateway.Devices = append(c.Gateway.Devices, d)
		return nil
	})
}

// UpdateGatewayDevice 原位更新一台已登记设备；本用例暂不允许改名（与分组一致）。
//
// 参数说明：
//   - name: string，现有设备名称，用于稳定定位。
//   - d: config.GatewayDevice，目标值；Name 必须与 name 一致。
//
// 返回值说明：error，设备不存在、改名或校验失败时返回。
//
// 错误情况：任何失败经统一事务回滚，不产生半更新状态。
func (a *App) UpdateGatewayDevice(name string, d config.GatewayDevice) error {
	name = strings.TrimSpace(name)
	d.Name = strings.TrimSpace(d.Name)
	d.IP = strings.TrimSpace(d.IP)
	d.Policy = strings.TrimSpace(d.Policy)
	if d.Name != name {
		return fmt.Errorf("网关设备暂不支持改名：%q -> %q", name, d.Name)
	}
	a.gatewayMutationMu.Lock()
	defer a.gatewayMutationMu.Unlock()
	return a.mutateGatewayLocked(func(c *config.Config) error {
		for i := range c.Gateway.Devices {
			if c.Gateway.Devices[i].Name == name {
				c.Gateway.Devices[i] = d
				return nil
			}
		}
		return fmt.Errorf("设备 %q 不存在", name)
	})
}

// RemoveGatewayDevice 删除一台已登记设备并持久化；其分流规则随下次生成消失。
//
// 参数说明：
//   - name: string，待删除设备名称。
//
// 返回值说明：error，设备不存在或调和失败时返回并回滚。
//
// 错误情况：删除最后一台设备后模块回到「设备表为空不生效」状态，
// 执行层规则随事务清除。
func (a *App) RemoveGatewayDevice(name string) error {
	name = strings.TrimSpace(name)
	a.gatewayMutationMu.Lock()
	defer a.gatewayMutationMu.Unlock()
	return a.mutateGatewayLocked(func(c *config.Config) error {
		for i := range c.Gateway.Devices {
			if c.Gateway.Devices[i].Name == name {
				c.Gateway.Devices = append(c.Gateway.Devices[:i], c.Gateway.Devices[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("设备 %q 不存在", name)
	})
}
