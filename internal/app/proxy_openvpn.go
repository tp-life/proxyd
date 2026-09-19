package app

// 代理域：OpenVPN 一体化接入用例。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
	proxyopenvpn "proxyd/internal/proxy/openvpn"
	"proxyd/internal/proxy/pool"
	"proxyd/internal/proxy/subscribe"
)

// OpenVPNSetupResult 是一次一体化接入事务成功后的可见结果。
type OpenVPNSetupResult struct {
	Name       string                  `json:"name"`
	GroupName  string                  `json:"group_name"`
	Port       int                     `json:"port"`
	AccessMode proxyopenvpn.AccessMode `json:"access_mode"`
	RouteRule  string                  `json:"route_rule,omitempty"`
	Warnings   []string                `json:"warnings,omitempty"`
}

// OpenVPNTerminationResult 描述删除一条 OpenVPN 聚合接入时实际清理的资源。
type OpenVPNTerminationResult struct {
	Name          string   `json:"name"`
	RemovedGroups []string `json:"removed_groups"`
	RemovedRules  int      `json:"removed_rules"`
}

// PreviewOpenVPNProfile 解析 .ovpn，并返回不包含证书或凭据的安全摘要。
//
// 参数说明：raw 是用户在浏览器选择的完整 .ovpn 文本。
//
// 返回值说明：openvpn.ImportPreview 用于页面回填远端和显示认证要求；error 表示
// profile 缺少 mihomo 所需内联材料或包含不支持的关键配置。
//
// 错误情况：本方法不修改配置和运行态，因此任何解析错误都没有回滚需求。
func (a *App) PreviewOpenVPNProfile(raw string) (proxyopenvpn.ImportPreview, error) {
	return proxyopenvpn.PreviewProfile(raw)
}

// SetupOpenVPN 在单个事务中创建 OpenVPN 出站、单成员 select 分组、固定代理入口
// 与可选 TUN 路由。
//
// 参数说明：
//   - ctx: context.Context，控制节点探测、mihomo 热更新与失败回滚。
//   - requested: openvpn.Setup，包含上传 profile、认证输入和接入方式。
//
// 返回值说明：OpenVPNSetupResult 包含分配端口、受管路由和非阻断兼容性警告；
// error 表示 profile、认证、名称、端口、运行态或持久化失败。
//
// 错误情况：askpass 只用于 Normalize 解密私钥，既不进入结果也不写入配置。任一步
// 失败都会恢复旧配置与旧 mihomo 运行态，避免留下只有节点或只有策略组的半套接入。
func (a *App) SetupOpenVPN(ctx context.Context, requested proxyopenvpn.Setup) (OpenVPNSetupResult, error) {
	setup, err := requested.Normalize()
	if err != nil {
		return OpenVPNSetupResult{}, err
	}

	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.Lock()
	oldConfig := a.cfg.Clone()
	nextConfig := a.cfg.Clone()
	for _, entry := range nextConfig.ManualNodes {
		if subscribe.ManualNodeName(entry) == setup.Name {
			a.mu.Unlock()
			return OpenVPNSetupResult{}, fmt.Errorf("节点 %q 已存在", setup.Name)
		}
	}
	for _, existing := range a.nodes {
		if existing == nil {
			continue
		}
		if existing.Name == setup.Name {
			a.mu.Unlock()
			return OpenVPNSetupResult{}, fmt.Errorf("节点 %q 已存在", setup.Name)
		}
		if existing.Name == setup.GroupName {
			a.mu.Unlock()
			return OpenVPNSetupResult{}, fmt.Errorf("策略组名 %q 与现有节点冲突", setup.GroupName)
		}
	}
	for _, existing := range nextConfig.Groups {
		if existing.Name == setup.Name {
			a.mu.Unlock()
			return OpenVPNSetupResult{}, fmt.Errorf("节点名 %q 与现有策略组冲突", setup.Name)
		}
	}
	mapping := setup.OutboundMapping()
	if _, parseErr := subscribe.ParseManualNode(mapping); parseErr != nil {
		a.mu.Unlock()
		return OpenVPNSetupResult{}, fmt.Errorf("OpenVPN 出站校验失败: %w", parseErr)
	}
	group := config.NodeGroup{Name: setup.GroupName, Port: setup.GroupPort, Type: config.GroupTypeSelect, Nodes: []string{setup.Name}}
	if group.Port == 0 {
		group.Port, err = nextTunnelGroupPort(nextConfig, group)
	} else {
		err = nextConfig.CheckGroup(group)
	}
	if err != nil {
		a.mu.Unlock()
		return OpenVPNSetupResult{}, err
	}
	nextConfig.ManualNodes = append(nextConfig.ManualNodes, mapping)
	nextConfig.Groups = append(nextConfig.Groups, group)
	routeRule := setup.RouteRule()
	if routeRule != "" {
		if err := config.ValidateCustomRule(routeRule); err != nil {
			a.mu.Unlock()
			return OpenVPNSetupResult{}, err
		}
		if !containsString(nextConfig.CustomRules, routeRule) {
			nextConfig.CustomRules = append([]string{routeRule}, nextConfig.CustomRules...)
		}
		nextConfig.TUN.ApplyDefaults()
		nextConfig.TUN.Enable = true
		if err := nextConfig.TUN.Validate(); err != nil {
			a.mu.Unlock()
			return OpenVPNSetupResult{}, err
		}
	}
	a.cfg = nextConfig
	a.mu.Unlock()

	if err := a.refreshLocked(ctx, false); err != nil {
		return OpenVPNSetupResult{}, a.rollbackOpenVPNSetupLocked(ctx, oldConfig, fmt.Errorf("OpenVPN 接入运行态应用失败: %w", err))
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return OpenVPNSetupResult{}, a.rollbackOpenVPNSetupLocked(ctx, oldConfig, persistErr)
	}
	return OpenVPNSetupResult{Name: setup.Name, GroupName: setup.GroupName, Port: group.Port, AccessMode: setup.AccessMode, RouteRule: routeRule, Warnings: append([]string(nil), setup.Profile.Warnings...)}, nil
}

// TerminateOpenVPNSetup 以单个事务删除 OpenVPN 节点、引用成员、空策略组与受管路由。
//
// 参数说明：ctx 用于请求取消检查；name 是 OpenVPN 出站名称。
//
// 返回值说明：OpenVPNTerminationResult 汇总删除的组与规则；error 表示目标不存在、
// 类型不匹配、mihomo 热更新、落盘或快照更新失败。
//
// 错误情况：任何提交阶段失败都会恢复内存配置、运行态、磁盘配置和节点/端口快照。
// 全局 TUN 开关不会自动关闭，因为它可能仍承载 Tailscale 或普通分流规则。
func (a *App) TerminateOpenVPNSetup(ctx context.Context, name string) (OpenVPNTerminationResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return OpenVPNTerminationResult{}, fmt.Errorf("OpenVPN 节点名称不能为空")
	}
	if err := ctx.Err(); err != nil {
		return OpenVPNTerminationResult{}, err
	}

	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.RLock()
	oldConfig := a.cfg.Clone()
	oldNodes := append([]*node.Node(nil), a.nodes...)
	oldAssignments := append([]pool.Assignment(nil), a.assigns...)
	a.mu.RUnlock()
	nextConfig := oldConfig.Clone()

	manualIndex := -1
	for index, entry := range nextConfig.ManualNodes {
		candidate, err := subscribe.ParseManualNode(entry)
		if err != nil || candidate.Name != name {
			continue
		}
		typ, _ := candidate.Mapping["type"].(string)
		if typ != node.TunnelTypeOpenVPN {
			return OpenVPNTerminationResult{}, fmt.Errorf("节点 %q 不是 OpenVPN 接入", name)
		}
		manualIndex = index
		break
	}
	if manualIndex < 0 {
		return OpenVPNTerminationResult{}, fmt.Errorf("OpenVPN 节点 %q 不存在", name)
	}
	nextConfig.ManualNodes = append(nextConfig.ManualNodes[:manualIndex], nextConfig.ManualNodes[manualIndex+1:]...)

	// 节点可能在创建后被用户加入其它显式组，因此删除时清理所有成员引用；仅当组
	// 已无成员时删除整个组，避免破坏仍有其它出口的用户自定义策略组。
	removedGroupSet := make(map[string]bool)
	nextGroups := make([]config.NodeGroup, 0, len(nextConfig.Groups))
	for _, group := range nextConfig.Groups {
		members := make([]string, 0, len(group.Nodes))
		removedReference := false
		for _, member := range group.Nodes {
			if member == name {
				removedReference = true
				continue
			}
			members = append(members, member)
		}
		if !removedReference {
			nextGroups = append(nextGroups, group)
			continue
		}
		if len(members) == 0 {
			removedGroupSet[group.Name] = true
			continue
		}
		group.Nodes = members
		nextGroups = append(nextGroups, group)
	}
	nextConfig.Groups = nextGroups

	removedRules := 0
	nextRules := make([]string, 0, len(nextConfig.CustomRules))
	for _, rule := range nextConfig.CustomRules {
		managed := false
		for groupName := range removedGroupSet {
			if proxyopenvpn.IsManagedRouteForGroup(rule, groupName) {
				managed = true
				break
			}
		}
		if managed {
			removedRules++
			continue
		}
		nextRules = append(nextRules, rule)
	}
	nextConfig.CustomRules = nextRules

	nextNodes := filterNodesByName(oldNodes, name)
	nextAssignments := filterAssignmentsByNodeName(oldAssignments, name)
	a.mu.Lock()
	a.cfg = nextConfig
	a.nodes = nextNodes
	a.assigns = nextAssignments
	a.mu.Unlock()
	if err := a.regenerateLocked(nextAssignments); err != nil {
		return OpenVPNTerminationResult{}, a.rollbackOpenVPNTerminationLocked(oldConfig, oldNodes, oldAssignments, err, false)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return OpenVPNTerminationResult{}, a.rollbackOpenVPNTerminationLocked(oldConfig, oldNodes, oldAssignments, persistErr, true)
	}
	if err := a.saveOpenVPNTerminationSnapshots(nextNodes, nextAssignments); err != nil {
		return OpenVPNTerminationResult{}, a.rollbackOpenVPNTerminationLocked(oldConfig, oldNodes, oldAssignments, err, true)
	}
	removedGroups := make([]string, 0, len(removedGroupSet))
	for _, group := range oldConfig.Groups {
		if removedGroupSet[group.Name] {
			removedGroups = append(removedGroups, group.Name)
		}
	}
	return OpenVPNTerminationResult{Name: name, RemovedGroups: removedGroups, RemovedRules: removedRules}, nil
}

// rollbackOpenVPNSetupLocked 恢复一体化创建前的配置、运行态和磁盘文件。
//
// 参数说明：ctx 控制回滚刷新；oldConfig 是事务前深快照；cause 是原始错误。
//
// 返回值说明：始终返回包含 cause 的错误，回滚失败通过 errors.Join 合并。
//
// 错误情况：调用方必须持有 refreshing 锁；即使运行态恢复失败仍继续恢复磁盘。
func (a *App) rollbackOpenVPNSetupLocked(ctx context.Context, oldConfig *config.Config, cause error) error {
	a.mu.Lock()
	a.cfg = oldConfig.Clone()
	a.mu.Unlock()
	joined := cause
	if err := a.refreshLocked(ctx, false); err != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复 OpenVPN 接入前运行态失败: %w", err))
	}
	a.mu.Lock()
	if err := a.persistLocked(); err != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复 OpenVPN 接入前配置文件失败: %w", err))
	}
	a.mu.Unlock()
	return joined
}

// saveOpenVPNTerminationSnapshots 更新删除后的端口与节点快照。
//
// 参数说明：nodes 是已提交节点集；assignments 是已提交端口分配。
//
// 返回值说明：两个快照均原子写入时返回 nil，否则返回具体阶段错误。
//
// 错误情况：快照失败必须触发完整回滚，否则旧 nodes.json 会在重启时复活已删节点。
func (a *App) saveOpenVPNTerminationSnapshots(nodes []*node.Node, assignments []pool.Assignment) error {
	snapshot := &pool.Snapshot{Mapping: make(map[string]int, len(assignments))}
	for _, assignment := range assignments {
		if assignment.Node != nil {
			snapshot.Mapping[assignment.Node.Key()] = assignment.Port
		}
	}
	if err := pool.SaveSnapshot(a.snapshotPath(), snapshot); err != nil {
		return fmt.Errorf("保存 OpenVPN 终止端口快照失败: %w", err)
	}
	if err := node.SaveSnapshot(a.nodesSnapshotPath(), nodes); err != nil {
		return fmt.Errorf("保存 OpenVPN 终止节点快照失败: %w", err)
	}
	return nil
}

// rollbackOpenVPNTerminationLocked 恢复删除事务前的配置、运行态与持久化快照。
//
// 参数说明：oldConfig、oldNodes、oldAssignments 是事务前快照；cause 是原始错误；
// restoreDisk 表示新配置是否已可能落盘。
//
// 返回值说明：始终返回包含原始错误和全部回滚错误的聚合值。
//
// 错误情况：调用方必须持有 refreshing 锁；每个恢复阶段相互独立并尽力执行。
func (a *App) rollbackOpenVPNTerminationLocked(oldConfig *config.Config, oldNodes []*node.Node, oldAssignments []pool.Assignment, cause error, restoreDisk bool) error {
	a.mu.Lock()
	a.cfg = oldConfig.Clone()
	a.nodes = append([]*node.Node(nil), oldNodes...)
	a.assigns = append([]pool.Assignment(nil), oldAssignments...)
	a.mu.Unlock()
	joined := cause
	if err := a.regenerateLocked(oldAssignments); err != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复 OpenVPN 终止前运行态失败: %w", err))
	}
	if restoreDisk {
		a.mu.Lock()
		if err := a.persistLocked(); err != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复 OpenVPN 终止前配置文件失败: %w", err))
		}
		a.mu.Unlock()
	}
	if err := a.saveOpenVPNTerminationSnapshots(oldNodes, oldAssignments); err != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复 OpenVPN 终止前快照失败: %w", err))
	}
	return joined
}
