package app

// 代理域：Tailscale 一体化接入用例。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/proxy/core"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/pool"
	"proxyd/internal/proxy/subscribe"
	proxytailscale "proxyd/internal/proxy/tailscale"
)

// TailscaleSetupResult 是一次一体化接入事务成功后的可见结果。
type TailscaleSetupResult struct {
	Name       string                           `json:"name"`
	GroupName  string                           `json:"group_name"`
	Port       int                              `json:"port"`
	AccessMode proxytailscale.AccessMode        `json:"access_mode"`
	RouteRule  string                           `json:"route_rule,omitempty"`
	Enrollment core.TailscaleEnrollmentSnapshot `json:"enrollment"`
}

// TailscaleTerminationResult 描述终止一次一体化接入时实际清理的聚合资源。
type TailscaleTerminationResult struct {
	Name          string   `json:"name"`
	RemovedGroups []string `json:"removed_groups"`
	RemovedRules  int      `json:"removed_rules"`
}

// SetupTailscale 在单个事务中创建 Tailscale 出站、单成员 select 分组、固定代理
// 入口以及可选的 TUN 路由，并在提交后主动启动 mihomo/tsnet 注册流程。
//
// 参数说明：
//   - ctx: context.Context，控制配置校验、节点刷新和 mihomo 热更新。
//   - requested: tailscale.Setup，来自管理面的单页接入表单。
//
// 返回值说明：
//   - TailscaleSetupResult：包含实际分配的分组端口、规则与初始注册状态。
//   - error：领域校验、名称/端口冲突、mihomo 热更新或配置持久化失败时返回。
//
// 错误情况：任一步失败都会恢复旧配置与旧 mihomo 运行态；注册链接只保存在
// Runner 内存。成功后触发动作异步执行，避免管理员审批等待占用 HTTP 请求。
func (a *App) SetupTailscale(ctx context.Context, requested proxytailscale.Setup) (TailscaleSetupResult, error) {
	setup, err := requested.Normalize()
	if err != nil {
		return TailscaleSetupResult{}, err
	}

	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.Lock()
	oldConfig := a.cfg.Clone()
	nextConfig := a.cfg.Clone()
	for _, entry := range nextConfig.ManualNodes {
		if subscribe.ManualNodeName(entry) == setup.Name {
			a.mu.Unlock()
			return TailscaleSetupResult{}, fmt.Errorf("节点 %q 已存在", setup.Name)
		}
	}
	for _, existing := range a.nodes {
		if existing == nil {
			continue
		}
		if existing.Name == setup.Name {
			a.mu.Unlock()
			return TailscaleSetupResult{}, fmt.Errorf("节点 %q 已存在", setup.Name)
		}
		if existing.Name == setup.GroupName {
			a.mu.Unlock()
			return TailscaleSetupResult{}, fmt.Errorf("策略组名 %q 与现有节点冲突", setup.GroupName)
		}
	}
	for _, existing := range nextConfig.Groups {
		if existing.Name == setup.Name {
			a.mu.Unlock()
			return TailscaleSetupResult{}, fmt.Errorf("节点名 %q 与现有策略组冲突", setup.Name)
		}
	}
	mapping := tailscaleOutboundMapping(setup)
	if _, parseErr := subscribe.ParseManualNode(mapping); parseErr != nil {
		a.mu.Unlock()
		return TailscaleSetupResult{}, fmt.Errorf("Tailscale 出站校验失败: %w", parseErr)
	}
	group := config.NodeGroup{
		Name:  setup.GroupName,
		Port:  setup.Port,
		Type:  config.GroupTypeSelect,
		Nodes: []string{setup.Name},
	}
	if group.Port == 0 {
		group.Port, err = nextTunnelGroupPort(nextConfig, group)
	} else {
		err = nextConfig.CheckGroup(group)
	}
	if err != nil {
		a.mu.Unlock()
		return TailscaleSetupResult{}, err
	}
	nextConfig.ManualNodes = append(nextConfig.ManualNodes, mapping)
	nextConfig.Groups = append(nextConfig.Groups, group)
	routeRule := setup.RouteRule()
	if routeRule != "" {
		if err := config.ValidateCustomRule(routeRule); err != nil {
			a.mu.Unlock()
			return TailscaleSetupResult{}, err
		}
		if !containsString(nextConfig.CustomRules, routeRule) {
			nextConfig.CustomRules = append([]string{routeRule}, nextConfig.CustomRules...)
		}
		nextConfig.TUN.ApplyDefaults()
		nextConfig.TUN.Enable = true
		if err := nextConfig.TUN.Validate(); err != nil {
			a.mu.Unlock()
			return TailscaleSetupResult{}, err
		}
	}
	a.cfg = nextConfig
	a.mu.Unlock()

	a.runner.BeginTailscaleEnrollment(setup.Name, string(setup.AuthMode))
	if err := a.refreshLocked(ctx, false); err != nil {
		return TailscaleSetupResult{}, a.rollbackTailscaleSetupLocked(ctx, oldConfig, setup.Name, fmt.Errorf("Tailscale 接入运行态应用失败: %w", err))
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return TailscaleSetupResult{}, a.rollbackTailscaleSetupLocked(ctx, oldConfig, setup.Name, persistErr)
	}

	// tsnet 出站采用懒启动；事务提交后用有界探测主动触发认证。该探测不会被当作
	// Tailnet 业务健康检查，未配置 Exit Node 时访问公网失败属于预期结果。
	go func(name string) {
		triggerCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		a.runner.TriggerTailscaleEnrollment(triggerCtx, name)
	}(setup.Name)
	enrollment, _ := a.runner.TailscaleEnrollment(setup.Name)
	return TailscaleSetupResult{
		Name:       setup.Name,
		GroupName:  setup.GroupName,
		Port:       group.Port,
		AccessMode: setup.AccessMode,
		RouteRule:  routeRule,
		Enrollment: enrollment,
	}, nil
}

// TailscaleEnrollment 返回指定接入节点的内存注册状态。
//
// 参数说明：name 是 mihomo Tailscale 出站名称。
//
// 返回值说明：状态副本和存在标志；注册链接不会从配置文件恢复。
//
// 错误情况：无；未知或进程重启后尚未重新触发的名称返回 false。
func (a *App) TailscaleEnrollment(name string) (core.TailscaleEnrollmentSnapshot, bool) {
	return a.runner.TailscaleEnrollment(strings.TrimSpace(name))
}

// TailscaleEnrollments 返回当前进程全部一体化接入注册状态。
//
// 参数说明：无。
//
// 返回值说明：按节点名排序的内存状态切片。
//
// 错误情况：无。
func (a *App) TailscaleEnrollments() []core.TailscaleEnrollmentSnapshot {
	return a.runner.TailscaleEnrollments()
}

// TerminateTailscaleSetup 以单个配置事务终止失败、启动中或等待审批的 Tailscale
// 一体化接入，并清理节点、引用该节点的策略组、受管 TUN 路由与内存注册状态。
//
// 参数说明：
//   - ctx: context.Context，在事务开始前检查调用方是否已经取消。
//   - name: string，需要终止的 mihomo Tailscale 出站名称。
//
// 返回值说明：TailscaleTerminationResult 包含被删除的策略组与受管规则数量；error
// 表示节点不存在、目标并非 Tailscale、mihomo 热更新、配置落盘或快照提交失败。
//
// 错误情况：运行态、配置文件或快照任一步失败都会恢复事务前的配置、节点与端口
// 分配。TUN 总开关属于全局用户配置，不在终止时自动关闭；这里只删除本接入生成的
// 精确 IP 路由，避免影响其它 TUN 用途。
func (a *App) TerminateTailscaleSetup(ctx context.Context, name string) (TailscaleTerminationResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return TailscaleTerminationResult{}, fmt.Errorf("Tailscale 节点名称不能为空")
	}
	if err := ctx.Err(); err != nil {
		return TailscaleTerminationResult{}, err
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
	var removedNode *node.Node
	for index, entry := range nextConfig.ManualNodes {
		candidate, err := subscribe.ParseManualNode(entry)
		if err != nil || candidate.Name != name {
			continue
		}
		if !candidate.IsTailscale() {
			return TailscaleTerminationResult{}, fmt.Errorf("节点 %q 不是 Tailscale 接入", name)
		}
		manualIndex = index
		removedNode = candidate
		break
	}
	if manualIndex < 0 {
		return TailscaleTerminationResult{}, fmt.Errorf("Tailscale 节点 %q 不存在", name)
	}
	nextConfig.ManualNodes = append(nextConfig.ManualNodes[:manualIndex], nextConfig.ManualNodes[manualIndex+1:]...)

	// 一体化接入组初始只有目标节点。若用户后来把该节点加入其它显式组，同样移除
	// 成员引用；成员清空的组无法再提供有效出口，必须随聚合一并删除。订阅来源组不
	// 使用显式 Nodes，不会被这段逻辑误删。
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
			if proxytailscale.IsManagedRouteForGroup(rule, groupName) {
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
	if nextConfig.MainNode == removedNode.Key() {
		nextConfig.MainNode = ""
	}

	nextNodes := filterNodesByName(oldNodes, name)
	nextAssignments := filterAssignmentsByNodeName(oldAssignments, name)
	a.runner.CancelTailscaleEnrollmentTrigger(name)
	a.mu.Lock()
	a.cfg = nextConfig
	a.nodes = nextNodes
	a.assigns = nextAssignments
	a.mu.Unlock()

	if err := a.regenerateLocked(nextAssignments); err != nil {
		return TailscaleTerminationResult{}, a.rollbackTailscaleTerminationLocked(oldConfig, oldNodes, oldAssignments, err, false)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return TailscaleTerminationResult{}, a.rollbackTailscaleTerminationLocked(oldConfig, oldNodes, oldAssignments, persistErr, true)
	}
	if err := a.saveTailscaleTerminationSnapshots(nextNodes, nextAssignments); err != nil {
		return TailscaleTerminationResult{}, a.rollbackTailscaleTerminationLocked(oldConfig, oldNodes, oldAssignments, err, true)
	}
	a.runner.RemoveTailscaleEnrollment(name)

	removedGroups := make([]string, 0, len(removedGroupSet))
	for _, group := range oldConfig.Groups {
		if removedGroupSet[group.Name] {
			removedGroups = append(removedGroups, group.Name)
		}
	}
	return TailscaleTerminationResult{Name: name, RemovedGroups: removedGroups, RemovedRules: removedRules}, nil
}

// filterNodesByName 复制节点切片并排除指定出站，供终止事务生成下一版运行态。
//
// 参数说明：nodes 是事务前节点快照；name 是要删除的出站名称。
//
// 返回值说明：保持原顺序且不包含目标名称的新切片。
//
// 错误情况：无；nil 节点会原样保留，由既有配置生成逻辑忽略。
func filterNodesByName(nodes []*node.Node, name string) []*node.Node {
	filtered := make([]*node.Node, 0, len(nodes))
	for _, candidate := range nodes {
		if candidate != nil && candidate.Name == name {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered
}

// filterAssignmentsByNodeName 复制稳定端口分配并排除指定节点。
//
// 参数说明：assignments 是事务前分配快照；name 是要删除的节点名称。
//
// 返回值说明：保持端口与顺序不变的剩余分配。
//
// 错误情况：无；nil Node 分配会保留，后续配置生成负责既有防御性处理。
func filterAssignmentsByNodeName(assignments []pool.Assignment, name string) []pool.Assignment {
	filtered := make([]pool.Assignment, 0, len(assignments))
	for _, assignment := range assignments {
		if assignment.Node != nil && assignment.Node.Name == name {
			continue
		}
		filtered = append(filtered, assignment)
	}
	return filtered
}

// saveTailscaleTerminationSnapshots 在运行态与主配置提交后更新端口和节点快照，防止
// 已终止的手动节点在下次启动时从旧 nodes.json 恢复。
//
// 参数说明：nodes 是已提交节点集；assignments 是已提交端口分配。
//
// 返回值说明：两个快照都原子写入时返回 nil，否则返回包含具体路径阶段的错误。
//
// 错误情况：任一快照序列化、目录创建、写入或 rename 失败时返回；调用方必须回滚
// 主事务并重写旧快照，不能把快照失败仅作为日志忽略。
func (a *App) saveTailscaleTerminationSnapshots(nodes []*node.Node, assignments []pool.Assignment) error {
	snapshot := &pool.Snapshot{Mapping: make(map[string]int, len(assignments))}
	for _, assignment := range assignments {
		if assignment.Node != nil {
			snapshot.Mapping[assignment.Node.Key()] = assignment.Port
		}
	}
	if err := pool.SaveSnapshot(a.snapshotPath(), snapshot); err != nil {
		return fmt.Errorf("保存 Tailscale 终止端口快照失败: %w", err)
	}
	if err := node.SaveSnapshot(a.nodesSnapshotPath(), nodes); err != nil {
		return fmt.Errorf("保存 Tailscale 终止节点快照失败: %w", err)
	}
	return nil
}

// rollbackTailscaleTerminationLocked 恢复终止事务前的配置、运行态与持久化快照。
//
// 参数说明：oldConfig、oldNodes、oldAssignments 是事务前深浅适配快照；cause 是原始
// 错误；restoreDisk 表示新配置是否可能已落盘，需要主动写回旧配置。
//
// 返回值说明：始终返回包含 cause 的错误；回滚各阶段失败时通过 errors.Join 汇总。
//
// 错误情况：调用方必须持有 refreshing 锁。即使某一步恢复失败也继续执行后续恢复，
// 最大限度避免内存、mihomo、配置文件和启动快照处于不同版本。
func (a *App) rollbackTailscaleTerminationLocked(oldConfig *config.Config, oldNodes []*node.Node, oldAssignments []pool.Assignment, cause error, restoreDisk bool) error {
	a.mu.Lock()
	a.cfg = oldConfig.Clone()
	a.nodes = append([]*node.Node(nil), oldNodes...)
	a.assigns = append([]pool.Assignment(nil), oldAssignments...)
	a.mu.Unlock()
	joined := cause
	if err := a.regenerateLocked(oldAssignments); err != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复 Tailscale 终止前运行态失败: %w", err))
	}
	if restoreDisk {
		a.mu.Lock()
		if err := a.persistLocked(); err != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复 Tailscale 终止前配置文件失败: %w", err))
		}
		a.mu.Unlock()
	}
	if err := a.saveTailscaleTerminationSnapshots(oldNodes, oldAssignments); err != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复 Tailscale 终止前快照失败: %w", err))
	}
	return joined
}

// tailscaleOutboundMapping 把规范化接入值对象转换为 mihomo 原生出站映射。
//
// 参数说明：setup 已通过 Normalize，不包含未经校验的 URL、模式或 IP 偏好。
//
// 返回值说明：独立 map；空 auth-key 在审批模式下不会写入配置。
//
// 错误情况：无；mihomo 语义校验随后由 subscribe.ParseManualNode 与 core.Generate 完成。
func tailscaleOutboundMapping(setup proxytailscale.Setup) map[string]any {
	mapping := map[string]any{
		"name":          setup.Name,
		"type":          node.TunnelTypeTailscale,
		"hostname":      setup.Hostname,
		"accept-routes": setup.AcceptRoutes,
		"udp":           setup.UDP,
		"ephemeral":     setup.Ephemeral,
		"ip-version":    setup.IPVersion,
	}
	if setup.ControlURL != "" {
		mapping["control-url"] = setup.ControlURL
	}
	if setup.AuthKey != "" {
		mapping["auth-key"] = setup.AuthKey
	}
	if setup.DialerProxy != "" {
		mapping["dialer-proxy"] = setup.DialerProxy
	}
	if setup.ExitNode != "" {
		mapping["exit-node"] = setup.ExitNode
		if setup.ExitNodeLAN {
			mapping["exit-node-allow-lan-access"] = true
		}
	}
	return mapping
}

// nextTunnelGroupPort 从 43000 起寻找第一个通过完整配置校验的隧道分组端口。
//
// 参数说明：cfg 是尚未追加目标分组的配置副本；group 包含稳定名称与单节点成员。
//
// 返回值说明：可用端口；error 表示分组名称本身冲突或 43000-65535 已无可用端口。
//
// 错误情况：名称冲突需立即返回，不能伪装成端口耗尽；端口冲突则继续尝试下一位。
func nextTunnelGroupPort(cfg *config.Config, group config.NodeGroup) (int, error) {
	for _, existing := range cfg.Groups {
		if existing.Name == group.Name {
			return 0, fmt.Errorf("分组 %q 已存在", group.Name)
		}
	}
	for port := 43000; port <= 65535; port++ {
		group.Port = port
		if err := cfg.CheckGroup(group); err == nil {
			return port, nil
		} else if !strings.Contains(err.Error(), "端口") {
			// 名称、类型或成员来源错误不会随着端口递增而改善，立即返回真实原因，
			// 避免把保留名称等配置错误误报成“端口耗尽”。
			return 0, err
		}
	}
	return 0, fmt.Errorf("没有可用于 VPN 接入的分组端口")
}

// rollbackTailscaleSetupLocked 恢复一体化接入事务前的配置、运行态和注册状态。
//
// 参数说明：ctx 控制回滚刷新；oldConfig 是事务前深快照；name 是临时注册会话名；
// cause 是触发回滚的原始错误。
//
// 返回值说明：始终包含 cause；运行态或磁盘恢复失败时通过 errors.Join 一并返回。
//
// 错误情况：调用方必须持有 refreshing 锁；即使运行态恢复失败也会继续尝试恢复磁盘，
// 避免内存、进程和配置文件静默分叉。
func (a *App) rollbackTailscaleSetupLocked(ctx context.Context, oldConfig *config.Config, name string, cause error) error {
	a.runner.RemoveTailscaleEnrollment(name)
	a.mu.Lock()
	a.cfg = oldConfig.Clone()
	a.mu.Unlock()
	joined := cause
	if err := a.refreshLocked(ctx, false); err != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复 Tailscale 接入前运行态失败: %w", err))
	}
	a.mu.Lock()
	if err := a.persistLocked(); err != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复 Tailscale 接入前配置文件失败: %w", err))
	}
	a.mu.Unlock()
	return joined
}

// containsString 判断字符串切片是否已经包含目标规则。
//
// 参数说明：values 是现有自定义规则；target 是规范化后的新规则。
//
// 返回值说明：完全相等时返回 true。
//
// 错误情况：无；规则顺序具有业务语义，因此这里只去除完全重复项，不做排序或模糊匹配。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
