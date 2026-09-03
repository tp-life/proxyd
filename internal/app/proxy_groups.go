package app

// 代理域：节点分组用例。

import (
	"errors"
	"fmt"
	"strings"

	"proxyd/internal/config"
)

// Groups 返回节点分组快照（供 API 展示）。
func (a *App) Groups() []config.NodeGroup {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]config.NodeGroup, len(a.cfg.Groups))
	copy(out, a.cfg.Groups)
	return out
}

// AddGroup 新增节点分组（一组节点 → 指定端口，组内自动选优），持久化并热更新。
func (a *App) AddGroup(g config.NodeGroup) error {
	g.Name = strings.TrimSpace(g.Name)
	g.Type = strings.TrimSpace(g.Type)
	g.Subscription = strings.TrimSpace(g.Subscription)
	if g.Type == "" {
		g.Type = config.GroupTypeURLTest
	}
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.Lock()
	if err := a.cfg.CheckGroup(g); err != nil {
		a.mu.Unlock()
		return err
	}
	for _, n := range a.nodes {
		if n.Name == g.Name {
			a.mu.Unlock()
			return fmt.Errorf("分组名 %q 与节点名冲突", g.Name)
		}
	}
	a.cfg.Groups = append(a.cfg.Groups, g)
	a.mu.Unlock()
	if err := a.regenerateCurrentLocked(); err != nil {
		a.mu.Lock()
		a.cfg.Groups = a.cfg.Groups[:len(a.cfg.Groups)-1]
		a.mu.Unlock()
		_ = a.regenerateCurrentLocked()
		return fmt.Errorf("分组配置未通过 mihomo 校验: %w", err)
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// UpdateGroup 原位修改一个策略分组的端口、策略和成员来源，并作为单个事务热更新与持久化。
//
// 参数：
//   - currentName: string，路径中现有分组名称，用于稳定定位待更新实体。
//   - next: config.NodeGroup，目标分组值；本用例暂不允许改名，避免破坏订阅节点中可能存在的 dialer-proxy 引用。
//
// 返回值：error，分组不存在、字段/端口冲突、mihomo 热更新、持久化或回滚失败时返回。
//
// 错误情况：任何提交步骤失败都会恢复事务前的完整 Groups 列表和运行态；持久化已经
// 尝试但失败时还会重写旧磁盘配置，回滚错误通过 errors.Join 与原始错误一并返回。
func (a *App) UpdateGroup(currentName string, next config.NodeGroup) error {
	currentName = strings.TrimSpace(currentName)
	next.Name = strings.TrimSpace(next.Name)
	next.Type = strings.TrimSpace(next.Type)
	next.Subscription = strings.TrimSpace(next.Subscription)
	if next.Type == "" {
		next.Type = config.GroupTypeURLTest
	}
	if next.Name != currentName {
		return fmt.Errorf("策略分组暂不支持改名：%q -> %q", currentName, next.Name)
	}
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	a.mu.Lock()
	index := -1
	for i, group := range a.cfg.Groups {
		if group.Name == currentName {
			index = i
			break
		}
	}
	if index < 0 {
		a.mu.Unlock()
		return fmt.Errorf("分组 %q 不存在", currentName)
	}
	oldGroups := cloneNodeGroups(a.cfg.Groups)
	// 临时移除当前实体后复用新增校验，既能允许端口保持不变，又能检查与其它分组冲突。
	a.cfg.Groups = append(cloneNodeGroups(oldGroups[:index]), oldGroups[index+1:]...)
	if err := a.cfg.CheckGroup(next); err != nil {
		a.cfg.Groups = oldGroups
		a.mu.Unlock()
		return err
	}
	for _, currentNode := range a.nodes {
		if currentNode.Name == next.Name {
			a.cfg.Groups = oldGroups
			a.mu.Unlock()
			return fmt.Errorf("分组名 %q 与节点名冲突", next.Name)
		}
	}
	candidate := cloneNodeGroups(oldGroups)
	candidate[index] = next
	a.cfg.Groups = candidate
	a.mu.Unlock()

	if err := a.regenerateCurrentLocked(); err != nil {
		return a.rollbackGroupsLocked(oldGroups, fmt.Errorf("分组配置未通过 mihomo 校验: %w", err), false)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return a.rollbackGroupsLocked(oldGroups, persistErr, true)
	}
	return nil
}

// rollbackGroupsLocked 恢复策略分组事务开始前的配置、运行态和可选磁盘状态。
// 调用方必须持有 refreshing 锁，避免回滚期间刷新任务写入新的运行配置。
//
// 参数：
//   - oldGroups: []config.NodeGroup，事务前的深拷贝分组列表。
//   - cause: error，触发回滚的原始错误。
//   - restoreDisk: bool，是否还需要把旧配置重新写回磁盘。
//
// 返回值：error，始终包含 cause；运行态或磁盘恢复失败时合并返回全部错误。
//
// 错误情况：回滚失败不会被吞掉，调用方可以据此提示管理员检查实际监听状态。
func (a *App) rollbackGroupsLocked(oldGroups []config.NodeGroup, cause error, restoreDisk bool) error {
	a.mu.Lock()
	a.cfg.Groups = cloneNodeGroups(oldGroups)
	a.mu.Unlock()
	joined := cause
	if rollbackErr := a.regenerateCurrentLocked(); rollbackErr != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复旧策略分组运行态失败: %w", rollbackErr))
	}
	if restoreDisk {
		a.mu.Lock()
		rollbackErr := a.persistLocked()
		a.mu.Unlock()
		if rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复旧策略分组配置文件失败: %w", rollbackErr))
		}
	}
	return joined
}

// RemoveGroup 按名字删除节点分组，持久化并热更新。
func (a *App) RemoveGroup(name string) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.Lock()
	idx := -1
	for i, g := range a.cfg.Groups {
		if g.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		a.mu.Unlock()
		return fmt.Errorf("分组 %q 不存在", name)
	}
	removed := a.cfg.Groups[idx]
	a.cfg.Groups = append(a.cfg.Groups[:idx], a.cfg.Groups[idx+1:]...)
	a.mu.Unlock()
	if err := a.regenerateCurrentLocked(); err != nil {
		a.mu.Lock()
		gs := append(a.cfg.Groups, config.NodeGroup{})
		copy(gs[idx+1:], gs[idx:])
		gs[idx] = removed
		a.cfg.Groups = gs
		a.mu.Unlock()
		_ = a.regenerateCurrentLocked()
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// cloneNodeGroups 深复制策略组切片，避免事务内修改 Nodes 或 Subscription 时污染旧快照。
//
// 参数：
//   - groups: []config.NodeGroup，源策略组列表。
//
// 返回值：[]config.NodeGroup，可独立修改的副本。
//
// 错误情况：无；nil 输入返回 nil。
func cloneNodeGroups(groups []config.NodeGroup) []config.NodeGroup {
	out := append([]config.NodeGroup(nil), groups...)
	for i := range out {
		out[i].Nodes = append([]string(nil), groups[i].Nodes...)
	}
	return out
}

// groupNames 提取策略组名称，作为 dialer-proxy 健康检查允许引用的目标集合。
//
// 参数：
//   - groups: []config.NodeGroup，候选策略组列表。
//
// 返回值：[]string，保持配置顺序的组名。
//
// 错误情况：无；空名称由下游健康检查忽略。
func groupNames(groups []config.NodeGroup) []string {
	out := make([]string, 0, len(groups))
	for _, group := range groups {
		out = append(out, group.Name)
	}
	return out
}
