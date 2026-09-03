package app

// 代理域：自定义规则与远程规则源（rule-urls）用例。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"proxyd/internal/config"
	"proxyd/internal/proxy/ruleurl"
)

// RuleURLStat 是规则源的最近一次拉取状态（供 API 展示）。
type RuleURLStat struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Count int    `json:"count"`
	Error string `json:"error,omitempty"` // 拉取与缓存都失败
	Warn  string `json:"warn,omitempty"`  // 拉取失败但降级用了缓存
}

// CustomRules 返回自定义规则快照（供 API 展示）。
func (a *App) CustomRules() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, len(a.cfg.CustomRules))
	copy(out, a.cfg.CustomRules)
	return out
}

// AddRule 追加一条自定义规则（生成时前置到远程规则与内置规则之前）。
//
// 参数：
//   - rule: string，Clash 规则文本；首尾空白会被移除。
//
// 返回值：error，规则非法、mihomo 热更新、持久化或回滚失败时返回。
//
// 错误情况：通过统一事务助手提交，任何失败都会恢复旧规则与旧运行态。
func (a *App) AddRule(rule string) error {
	rule = strings.TrimSpace(rule)
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.RLock()
	next := append([]string(nil), a.cfg.CustomRules...)
	a.mu.RUnlock()
	next = append(next, rule)
	return a.commitCustomRulesLocked(next)
}

// RemoveRule 按下标删除一条可编辑的 custom-rules 规则。
//
// 参数：
//   - index: int，自定义规则的零基下标；远程规则和内置规则不在该索引空间内。
//
// 返回值：error，下标不存在、热更新、持久化或回滚失败时返回。
//
// 错误情况：删除只作用于 custom-rules，不会修改 rule-url 内容或内置 rules。
func (a *App) RemoveRule(index int) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.RLock()
	if index < 0 || index >= len(a.cfg.CustomRules) {
		a.mu.RUnlock()
		return fmt.Errorf("规则下标 %d 不存在", index)
	}
	next := append([]string(nil), a.cfg.CustomRules...)
	a.mu.RUnlock()
	next = append(next[:index], next[index+1:]...)
	return a.commitCustomRulesLocked(next)
}

// UpdateRule 原位替换一条自定义规则，保留其优先级位置。
//
// 参数：
//   - index: int，自定义规则的零基下标。
//   - rule: string，新的 Clash 规则文本。
//
// 返回值：error，下标/规则非法、热更新、持久化或回滚失败时返回。
//
// 错误情况：新规则必须先通过静态校验和 mihomo 完整配置自检，失败不会覆盖旧规则。
func (a *App) UpdateRule(index int, rule string) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.RLock()
	if index < 0 || index >= len(a.cfg.CustomRules) {
		a.mu.RUnlock()
		return fmt.Errorf("规则下标 %d 不存在", index)
	}
	next := append([]string(nil), a.cfg.CustomRules...)
	a.mu.RUnlock()
	next[index] = strings.TrimSpace(rule)
	return a.commitCustomRulesLocked(next)
}

// MoveRule 调整一条自定义规则的优先级位置。
//
// 参数：
//   - from: int，规则当前零基下标。
//   - to: int，移动完成后的目标零基下标。
//
// 返回值：error，下标非法、热更新、持久化或回滚失败时返回。
//
// 错误情况：仅重排 custom-rules；远程与内置规则保持各自既定顺序和只读语义。
func (a *App) MoveRule(from, to int) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.RLock()
	if from < 0 || from >= len(a.cfg.CustomRules) || to < 0 || to >= len(a.cfg.CustomRules) {
		a.mu.RUnlock()
		return fmt.Errorf("规则移动下标无效：from=%d to=%d", from, to)
	}
	next := append([]string(nil), a.cfg.CustomRules...)
	a.mu.RUnlock()
	if from == to {
		return nil
	}
	moved := next[from]
	next = append(next[:from], next[from+1:]...)
	next = append(next, "")
	copy(next[to+1:], next[to:])
	next[to] = moved
	return a.commitCustomRulesLocked(next)
}

// commitCustomRulesLocked 校验并提交完整 custom-rules 候选列表。
// 调用方必须持有 refreshing 锁，以串行化规则、刷新和核心热更新。
//
// 参数：
//   - next: []string，目标自定义规则列表；函数会复制后再写入配置。
//
// 返回值：error，静态校验、mihomo 自检/热更新、持久化或回滚失败时返回。
//
// 错误情况：持久化失败时既恢复内存和运行态，也尝试重写旧磁盘配置；多重失败合并返回。
func (a *App) commitCustomRulesLocked(next []string) error {
	for index, rule := range next {
		if err := config.ValidateCustomRule(strings.TrimSpace(rule)); err != nil {
			return fmt.Errorf("custom-rules[%d]: %w", index, err)
		}
		next[index] = strings.TrimSpace(rule)
	}
	a.mu.Lock()
	old := append([]string(nil), a.cfg.CustomRules...)
	a.cfg.CustomRules = append([]string(nil), next...)
	a.mu.Unlock()
	if err := a.regenerateCurrentLocked(); err != nil {
		return a.rollbackCustomRulesLocked(old, fmt.Errorf("规则未通过 mihomo 校验: %w", err), false)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return a.rollbackCustomRulesLocked(old, persistErr, true)
	}
	return nil
}

// rollbackCustomRulesLocked 恢复规则事务开始前的配置、运行态和可选磁盘状态。
// 调用方必须持有 refreshing 锁。
//
// 参数：
//   - old: []string，事务前的 custom-rules 副本。
//   - cause: error，触发回滚的原始错误。
//   - restoreDisk: bool，是否额外重写旧配置文件。
//
// 返回值：error，始终包含 cause；回滚失败时合并返回全部错误。
//
// 错误情况：运行态或磁盘恢复失败不会被吞掉，避免 UI 错误提示与实际规则不一致。
func (a *App) rollbackCustomRulesLocked(old []string, cause error, restoreDisk bool) error {
	a.mu.Lock()
	a.cfg.CustomRules = old
	a.mu.Unlock()
	joined := cause
	if rollbackErr := a.regenerateCurrentLocked(); rollbackErr != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复旧自定义规则运行态失败: %w", rollbackErr))
	}
	if restoreDisk {
		a.mu.Lock()
		rollbackErr := a.persistLocked()
		a.mu.Unlock()
		if rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复旧自定义规则配置文件失败: %w", rollbackErr))
		}
	}
	return joined
}

// RuleURLs 返回规则源列表及最近拉取状态（供 API 展示）。
func (a *App) RuleURLs() []RuleURLStat {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]RuleURLStat, 0, len(a.cfg.RuleURLs))
	for _, ru := range a.cfg.RuleURLs {
		st := RuleURLStat{Name: ru.Name, URL: ru.URL}
		if s, ok := a.ruleStats[ru.Name]; ok {
			st.Count, st.Error, st.Warn = s.Count, s.Error, s.Warn
		}
		out = append(out, st)
	}
	return out
}

// AddRuleURL 新增规则源：持久化后立即拉取该源并热更新（失败仅记录状态，不报错）。
func (a *App) AddRuleURL(ru config.RuleURL) error {
	ru.Name = strings.TrimSpace(ru.Name)
	ru.URL = strings.TrimSpace(ru.URL)
	a.mu.Lock()
	if err := a.cfg.CheckRuleURL(ru); err != nil {
		a.mu.Unlock()
		return err
	}
	a.cfg.RuleURLs = append(a.cfg.RuleURLs, ru)
	a.mu.Unlock()

	// 立即拉取这一个源；失败只记入状态，规则等下一轮刷新再试
	res := ruleurl.Fetch(context.Background(), ru, a.cfg.StateDir)
	a.applyRuleResults([]ruleurl.Result{res})

	if err := a.Regenerate(); err != nil {
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// RemoveRuleURL 按名字删除规则源，移除其导入规则，持久化并热更新。
func (a *App) RemoveRuleURL(name string) error {
	a.mu.Lock()
	idx := -1
	for i, ru := range a.cfg.RuleURLs {
		if ru.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		a.mu.Unlock()
		return fmt.Errorf("规则源 %q 不存在", name)
	}
	a.cfg.RuleURLs = append(a.cfg.RuleURLs[:idx], a.cfg.RuleURLs[idx+1:]...)
	delete(a.imported, name)
	delete(a.ruleStats, name)
	a.mu.Unlock()
	if err := a.Regenerate(); err != nil {
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// RuleURLContent 返回规则源适合直接阅读的内容。
//
// 参数：
//   - name: string，配置中的规则源唯一名称。
//
// 返回值：
//   - []byte：普通规则源保持原文，整体 Base64 gfwlist 返回解码后的 AutoProxy 文本。
//   - error：规则源不存在，或没有缓存且现场拉取失败时返回错误。
//
// 错误情况：
// 查找规则源时只持有读锁；真正的缓存读取和网络拉取在释放锁后执行，避免慢 I/O 阻塞
// 其他配置操作。解码仅影响展示返回值，不会改写配置或原始缓存。
func (a *App) RuleURLContent(name string) ([]byte, error) {
	a.mu.RLock()
	var ru config.RuleURL
	found := false
	for _, r := range a.cfg.RuleURLs {
		if r.Name == name {
			ru = r
			found = true
			break
		}
	}
	stateDir := a.cfg.StateDir
	a.mu.RUnlock()
	if !found {
		return nil, fmt.Errorf("规则源 %q 不存在", name)
	}
	return ruleurl.Content(context.Background(), ru, stateDir)
}

// applyRuleResults 记录规则源拉取结果（导入规则 + 状态），并打日志。
func (a *App) applyRuleResults(results []ruleurl.Result) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, res := range results {
		st := RuleURLStat{Name: res.Name, Count: len(res.Rules)}
		if res.Err != nil {
			st.Error = res.Err.Error()
			log.Printf("[rules] %v", res.Err)
		}
		if res.Warn != nil {
			st.Warn = res.Warn.Error()
			log.Printf("[rules] %v", res.Warn)
		}
		a.ruleStats[res.Name] = st
		if res.Err == nil {
			a.imported[res.Name] = res.Rules
		}
	}
}

// mergedImportedLocked 合并全部规则源导入的规则：按 rule-urls 配置顺序，去重，
// 超出 ruleurl.MaxImportedRules 截断。调用方须已持有 a.mu（读锁）。
func (a *App) mergedImportedLocked() []string {
	var out []string
	seen := map[string]bool{}
	for _, ru := range a.cfg.RuleURLs {
		for _, r := range a.imported[ru.Name] {
			if seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
	}
	if len(out) > ruleurl.MaxImportedRules {
		log.Printf("[rules] 导入规则 %d 条超过上限 %d，截断", len(out), ruleurl.MaxImportedRules)
		out = out[:ruleurl.MaxImportedRules]
	}
	return out
}
