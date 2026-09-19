package app

// 代理域：节点分组用例。

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"proxyd/internal/config"
	"proxyd/internal/proxy/groupstate"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/pool"
)

// builtinProxyGroupName 是 mihomo 规则模式兜底 MATCH,PROXY 落入的内置 select 组：
// 生成层恒创建（成员 = 全部可用节点 + AUTO + DIRECT），其选中项即「默认出口」，
// 与自定义 select 分组共用 groupstate 持久化（键 "PROXY"）。
const builtinProxyGroupName = "PROXY"

// Groups 返回节点分组快照（供 API 展示）。
func (a *App) Groups() []config.NodeGroup {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]config.NodeGroup, len(a.cfg.Groups))
	copy(out, a.cfg.Groups)
	return out
}

// GroupSelected 返回 select 分组的持久化选中项（分组名 -> 节点名），供 API 展示。
// 状态文件损坏时打日志并返回空映射，与生成路径的降级语义一致。
func (a *App) GroupSelected() map[string]string {
	selected, err := groupstate.Load(a.groupSelectedPath())
	if err != nil {
		log.Printf("[groupstate] %v (ignored)", err)
		return map[string]string{}
	}
	return selected
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

// SetGroupSelected 修改 select 类型分组的手动选中节点，并以「落盘 → 热更新 → 失败回滚」
// 的顺序提交。选中项持久化到 state-dir/group-selected.json，配置生成时写入 mihomo 的
// default-selected 字段，因此重启与订阅刷新后仍保持；mihomo 侧选中节点消失时按其原生
// 语义回退组成员首位。分组名 "PROXY" 特指内置组（默认出口）：选中值须为当前可用节点名
// （含隧道类）、"AUTO"（需存在可用节点）或 "DIRECT"。
//
// 参数：
//   - groupName: string，目标分组名；必须是已配置的 select 类型分组或内置 "PROXY"。
//   - nodeName: string，选中节点名；必须在分组当前可用成员中（隧道类节点允许）。
//
// 返回值：error，分组不存在/类型不符/节点不在成员中、状态落盘失败或热更新失败时返回。
//
// 错误情况：热更新失败时把选中状态文件回滚到修改前内容并尝试恢复运行态，
// 回滚错误与原始错误一并返回。选中状态不影响配置文件本身，因此不参与配置事务，
// 只回滚状态文件而不重写 config.yaml。
func (a *App) SetGroupSelected(groupName, nodeName string) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	a.mu.RLock()
	group := config.NodeGroup{}
	found := false
	for _, g := range a.cfg.Groups {
		if g.Name == groupName {
			group = g
			found = true
			break
		}
	}
	nodes := make([]*node.Node, len(a.nodes))
	copy(nodes, a.nodes)
	assigns := make([]pool.Assignment, len(a.assigns))
	copy(assigns, a.assigns)
	a.mu.RUnlock()

	if groupName == builtinProxyGroupName {
		if err := checkBuiltinProxySelection(nodeName, nodes, assigns); err != nil {
			return err
		}
	} else {
		if !found {
			return fmt.Errorf("分组 %q 不存在", groupName)
		}
		groupType := group.Type
		if groupType == "" {
			groupType = config.GroupTypeURLTest
		}
		if groupType != config.GroupTypeSelect {
			return fmt.Errorf("分组 %q 类型为 %s，仅 select 分组支持手动选中", groupName, groupType)
		}
		if !groupMemberAlive(group, nodes, nodeName) {
			return fmt.Errorf("节点 %q 不在分组 %q 当前可用成员中", nodeName, groupName)
		}
	}

	path := a.groupSelectedPath()
	old, err := groupstate.Load(path)
	if err != nil {
		log.Printf("[groupstate] %v (ignored)", err)
		old = map[string]string{}
	}
	next := make(map[string]string, len(old)+1)
	for k, v := range old {
		next[k] = v
	}
	next[groupName] = nodeName
	if err := groupstate.Save(path, next); err != nil {
		return fmt.Errorf("保存分组选中状态失败: %w", err)
	}
	if err := a.regenerateCurrentLocked(); err != nil {
		joined := fmt.Errorf("分组选中热更新失败: %w", err)
		if rollbackErr := groupstate.Save(path, old); rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复旧分组选中状态失败: %w", rollbackErr))
		}
		if rollbackErr := a.regenerateCurrentLocked(); rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复旧分组选中运行态失败: %w", rollbackErr))
		}
		return joined
	}
	return nil
}

// checkBuiltinProxySelection 校验内置 PROXY 组（默认出口）的选中值：
// DIRECT 恒可选；AUTO 需要存在可用节点（生成层有可用节点才创建 AUTO 组）；
// 节点名须为当前可用节点（含隧道类，与生成层的 PROXY 组成员集合一致）。
func checkBuiltinProxySelection(nodeName string, nodes []*node.Node, assigns []pool.Assignment) error {
	switch nodeName {
	case "DIRECT":
		return nil
	case "AUTO":
		for _, as := range assigns {
			if as.Node != nil {
				return nil
			}
		}
		return fmt.Errorf("当前无可用节点，AUTO 不可选")
	}
	for _, n := range nodes {
		if n != nil && n.Alive && n.Name == nodeName {
			return nil
		}
	}
	return fmt.Errorf("节点 %q 不在内置 PROXY 组当前可用成员中", nodeName)
}

// groupMemberAlive 判断节点是否在分组当前可用成员中，与 core 生成时的成员交集规则一致。
func groupMemberAlive(group config.NodeGroup, nodes []*node.Node, name string) bool {
	for _, n := range nodes {
		if n == nil || !n.Alive || n.Name != name {
			continue
		}
		if group.Subscription != "" {
			return n.Subscription == group.Subscription
		}
		for _, want := range group.Nodes {
			if want == name {
				return true
			}
		}
	}
	return false
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
