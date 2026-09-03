package app

// 代理域：TUN 与 DNS 预设开关用例。

import (
	"errors"
	"fmt"
	"strings"

	"proxyd/internal/config"
	"proxyd/internal/proxy/tunperm"
)

// TUNStatus 是 TUN 运行状态与当前进程权限状态的应用层只读视图。
//
// API 和 CLI 只依赖该视图，不直接读取操作系统权限适配器，从而避免展示层承担
// TUN 权限规则或拼接平台相关指引。
type TUNStatus struct {
	Enabled    bool   `json:"enabled"`
	Active     bool   `json:"active"`
	Allowed    bool   `json:"allowed"`
	Platform   string `json:"platform"`
	Permission string `json:"permission,omitempty"`
}

// TUNStatus 返回 TUN 配置状态以及当前进程的权限检测结果。
//
// 参数：无。
//
// 返回值：
//   - TUNStatus：Enabled 来自当前配置；Allowed/Platform/Permission 来自操作系统适配器。
//
// 错误情况：无；底层无法读取权限状态时按不允许处理，并通过 Permission 返回修复指引。
func (a *App) TUNStatus() TUNStatus {
	a.mu.RLock()
	enabled := a.cfg.TUN.Enable
	a.mu.RUnlock()
	permission := tunperm.Current()
	return TUNStatus{
		Enabled:    enabled,
		Active:     a.runner.TUNEnabled(),
		Allowed:    permission.Allowed,
		Platform:   permission.Platform,
		Permission: permission.Hint,
	}
}

// SetTUN 开关 TUN 模式，并按“权限检查 → 热更新 → 持久化”的顺序应用变更。
//
// 参数：
//   - enabled: bool，true 表示让 mihomo 创建 TUN 设备并接管系统流量，false 表示关闭。
//
// 返回值：
//   - error：权限不足、mihomo 配置生成/热更新失败或配置文件持久化失败时返回错误。
//
// 错误情况：开启前必须通过平台权限检测；热更新失败时会恢复旧 TUN 配置并尝试
// 再应用旧运行配置。回滚失败不会覆盖原始错误，runner 会同时记录底层失败日志。
func (a *App) SetTUN(enabled bool) error {
	if enabled {
		if err := tunperm.Require(); err != nil {
			return err
		}
	}

	a.mu.Lock()
	if a.cfg.TUN.Enable == enabled {
		a.mu.Unlock()
		return nil
	}
	old := a.cfg.TUN.Clone()
	a.cfg.TUN.ApplyDefaults()
	a.cfg.TUN.Enable = enabled
	a.mu.Unlock()

	if err := a.Regenerate(); err != nil {
		return a.rollbackTUN(old, err)
	}

	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	if err != nil {
		return a.rollbackTUN(old, err)
	}
	return nil
}

// rollbackTUN 恢复 TUN 配置并重新应用旧的 mihomo 运行态。
//
// 参数：
//   - old: config.TUNConfig，切换前的完整 TUN 配置副本。
//   - cause: error，触发回滚的生成、热更新或持久化错误。
//
// 返回值：
//   - error：始终保留原始 cause；运行态恢复也失败时通过 errors.Join 同时返回两者。
//
// 错误情况：mihomo 的 TUN 创建错误可能只写日志，因此回滚同样经过 Regenerate 的
// listener 实际状态校验；不能静默宣称已经恢复。
func (a *App) rollbackTUN(old config.TUNConfig, cause error) error {
	a.mu.Lock()
	a.cfg.TUN = old
	a.mu.Unlock()
	if rollbackErr := a.Regenerate(); rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("恢复旧 TUN 运行态失败: %w", rollbackErr))
	}
	return cause
}

// SetDNSPreset 切换 proxyd 生成的 DNS 预设，并热更新 mihomo 配置。
//
// 参数：
//   - preset: string，仅允许 off、fake-ip 或 redir-host；大小写和首尾空白会被规范化。
//
// 返回值：
//   - error：枚举无效、mihomo 生成/热更新失败或配置持久化失败时返回错误。
//
// 错误情况：手写 cfg.DNS 非空时仍保存预设但不改变当前生成结果，因为手写 DNS 的
// 优先级更高；热更新失败会恢复旧预设并尝试重新应用旧配置。
func (a *App) SetDNSPreset(preset string) error {
	preset = strings.ToLower(strings.TrimSpace(preset))
	switch preset {
	case config.DNSPresetOff, config.DNSPresetFakeIP, config.DNSPresetRedirHost:
	default:
		return fmt.Errorf("invalid dns-preset %q (off|fake-ip|redir-host)", preset)
	}

	a.mu.Lock()
	if a.cfg.DNSPreset == preset {
		a.mu.Unlock()
		return nil
	}
	old := a.cfg.DNSPreset
	a.cfg.DNSPreset = preset
	a.mu.Unlock()

	if err := a.Regenerate(); err != nil {
		return a.rollbackDNSPreset(old, err)
	}

	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	if err != nil {
		return a.rollbackDNSPreset(old, err)
	}
	return nil
}

// rollbackDNSPreset 恢复旧 DNS 预设并重新应用此前的 mihomo DNS 行为。
//
// 参数：
//   - old: string，切换前的 dns-preset。
//   - cause: error，触发回滚的热更新或持久化错误。
//
// 返回值：
//   - error：原始错误；回滚也失败时同时包含回滚错误。
//
// 错误情况：回滚生成失败会通过 errors.Join 暴露，避免 UI 收到失败后运行态仍悄悄
// 使用新 DNS 预设，尤其避免 TUN 场景出现难以定位的解析分裂。
func (a *App) rollbackDNSPreset(old string, cause error) error {
	a.mu.Lock()
	a.cfg.DNSPreset = old
	a.mu.Unlock()
	if rollbackErr := a.Regenerate(); rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("恢复旧 DNS 预设失败: %w", rollbackErr))
	}
	return cause
}
