package app

// 代理域：广告拦截（adblock）开关与规则集地址用例。

import (
	"errors"
	"fmt"

	"proxyd/internal/config"
)

// AdBlock 返回当前广告拦截配置的只读快照（值类型，无共享引用）。
func (a *App) AdBlock() config.AdBlockConfig {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.AdBlock
}

// AdBlockEffective 报告广告拦截规则在当前运行态下是否实际参与匹配：
// 主端口恒为规则模式，开启即生效。
func (a *App) AdBlockEffective() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.AdBlock.Enable
}

// SetAdBlock 以事务方式开关广告拦截并可选更新规则集地址：改内存配置 → 热更新 →
// 持久化，任一步失败整体回滚。ruleURL 为空表示保持当前地址不变。
//
// 参数：
//   - enable: bool，true 表示生成配置时注入 adblock rule-provider 与 REJECT 规则。
//   - ruleURL: string，新的规则集地址（须为 domain 行为 YAML payload 格式）；空串保持原值。
//
// 返回值：
//   - error：校验、mihomo 配置生成/热更新或持久化失败时返回；成功返回 nil。
//
// 错误情况：任何失败都会恢复旧内存配置并重新应用旧运行态；若磁盘提交阶段失败，
// 还会尝试把旧配置重新写回。回滚中的多个错误使用 errors.Join 合并。
func (a *App) SetAdBlock(enable bool, ruleURL string) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	a.mu.Lock()
	old := a.cfg.AdBlock
	next := old
	next.Enable = enable
	if ruleURL != "" {
		next.RuleURL = ruleURL
	}
	next.ApplyDefaults()
	if err := next.Validate(); err != nil {
		a.mu.Unlock()
		return err
	}
	a.cfg.AdBlock = next
	a.mu.Unlock()

	if err := a.regenerateCurrentLocked(); err != nil {
		return a.rollbackAdBlockLocked(old, err, false)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return a.rollbackAdBlockLocked(old, persistErr, true)
	}
	return nil
}

// rollbackAdBlockLocked 恢复广告拦截变更前的配置、运行态和可选磁盘状态。
// 调用方必须持有 refreshing 锁，确保回滚期间不会被订阅刷新或其它热更新穿插。
//
// 参数：
//   - old: config.AdBlockConfig，变更前的原始配置。
//   - cause: error，触发回滚的原始热更新或持久化错误。
//   - restoreDisk: bool，true 表示新配置已经尝试落盘，需要额外重写旧配置以消除不确定状态。
//
// 返回值：
//   - error：始终包含 cause；运行态或磁盘恢复失败时通过 errors.Join 一并返回。
//
// 错误情况：回滚失败不会被吞掉，API/CLI 可以明确告知用户人工检查运行态和配置文件。
func (a *App) rollbackAdBlockLocked(old config.AdBlockConfig, cause error, restoreDisk bool) error {
	a.mu.Lock()
	a.cfg.AdBlock = old
	a.mu.Unlock()

	joined := cause
	if rollbackErr := a.regenerateCurrentLocked(); rollbackErr != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复旧广告拦截运行态失败: %w", rollbackErr))
	}
	if restoreDisk {
		a.mu.Lock()
		rollbackErr := a.persistLocked()
		a.mu.Unlock()
		if rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复旧广告拦截配置文件失败: %w", rollbackErr))
		}
	}
	return joined
}
