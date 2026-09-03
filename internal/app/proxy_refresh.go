package app

// 代理域：订阅刷新流水线（拉取/健康检测/端口分配/热更新）与启动快照恢复。

import (
	"context"
	"fmt"
	"log"
	"strings"

	"proxyd/internal/config"
	"proxyd/internal/proxy/core"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/pool"
	"proxyd/internal/proxy/ruleurl"
	"proxyd/internal/proxy/subscribe"
)

// Regenerate 仅按当前 cfg + 当前 assignments 重新生成并热应用 mihomo 配置
// （不拉订阅、不测速）。用于 auto-port/rules/groups 等变更后的热更新。
func (a *App) Regenerate() error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	return a.regenerateCurrentLocked()
}

// regenerateCurrentLocked 用当前 assigns 重新生成；调用方须已持有 refreshing 锁。
func (a *App) regenerateCurrentLocked() error {
	a.mu.RLock()
	assigns := make([]pool.Assignment, len(a.assigns))
	copy(assigns, a.assigns)
	a.mu.RUnlock()
	return a.regenerateLocked(assigns)
}

// regenerateLocked 是重新生成热更新的核心；调用方须已持有 refreshing 锁。
func (a *App) regenerateLocked(assigns []pool.Assignment) error {
	a.mu.RLock()
	cfg := a.cfg
	imported := a.mergedImportedLocked()
	a.mu.RUnlock()
	return a.regenerateWithLocked(cfg, assigns, imported)
}

// regenerateWithLocked 用指定配置生成并热应用；调用方须已持有 refreshing 锁。
// cfg 为运行时副本（不与 a.cfg 同步修改），供两阶段热更新等场景使用。
//
// 主端口形态转换保护：mihomo 热更新先 PatchInboundListeners（监听新 listener）
// 后 ReCreateMixed（关闭旧 mixed-port），主端口从顶层 mixed-port 直接换成同端口
// listener 会 bind 冲突把端口打挂。因此当目标配置的主端口是 listener 形态
// （main-auto/main-node 生效）而上次应用的不是时，先应用一版"主端口入口完全关闭"
// 的配置释放端口，再应用目标配置。反向（listener → mixed-port）以及
// listener 同名仅换 proxy 目标（main-auto ↔ main-node）由 mihomo 安全处理。
func (a *App) regenerateWithLocked(cfg *config.Config, assigns []pool.Assignment, imported []string) error {
	willListener := core.MainInboundIsListener(cfg, assigns)
	a.mu.RLock()
	wasListener := a.mainListenerOn
	a.mu.RUnlock()
	if willListener && !wasListener {
		phase := *cfg // 浅拷贝：Generate 只读
		phase.MainAuto = false
		phase.MainNode = ""
		phase.MixedPort = 0 // 生成 mixed-port: 0（mihomo 视为关闭该入口）
		if err := a.applyConfigLocked(&phase, assigns, imported); err != nil {
			// 释放失败不致命：继续尝试直接应用目标配置
			log.Printf("[app] 主端口形态切换：释放旧入口失败（继续应用目标配置）: %v", err)
		}
	}
	if err := a.applyConfigLocked(cfg, assigns, imported); err != nil {
		return err
	}
	a.mu.Lock()
	a.mainListenerOn = willListener
	a.mu.Unlock()
	return nil
}

// applyConfigLocked 生成并热应用一版 mihomo 配置。
//
// 参数：
//   - cfg: *config.Config，本次需要应用的运行配置副本。
//   - assigns: []pool.Assignment，需要创建本地 listener 的节点端口映射。
//   - imported: []string，已经合并清洗的远程规则。
//
// 返回值：error，生成、自检、mihomo 热加载或 TUN 实际状态不一致时返回。
//
// 错误情况：调用方必须持有 refreshing 锁，确保读取节点快照和 Reload 期间没有另一轮
// 刷新并发修改。生成时同时传入完整健康节点集，使 dialer-proxy 依赖和策略组成员即使
// 没有独立本地端口，也会作为 proxy-only 出站注册到 mihomo。
func (a *App) applyConfigLocked(cfg *config.Config, assigns []pool.Assignment, imported []string) error {
	cfgYAML, err := core.GenerateWithNodes(cfg, assigns, a.Nodes(), imported)
	if err != nil {
		return fmt.Errorf("generate mihomo config: %w", err)
	}
	if err := a.runner.Reload(cfgYAML); err != nil {
		return fmt.Errorf("apply mihomo config: %w", err)
	}
	// mihomo 的 ReCreateTun 在创建虚拟网卡失败时只写日志，不把错误返回给 hub.Parse。
	// 因此必须在 Reload 返回后读取 listener 实际状态，否则可能把 enable:true 持久化，
	// 但运行时 TUN 已关闭。状态不一致作为应用失败返回，上层会恢复旧配置。
	if active := a.runner.TUNEnabled(); active != cfg.TUN.Enable {
		return fmt.Errorf("mihomo TUN 实际状态与请求不一致（期望 enable=%t，实际 active=%t）；请检查 TUN 日志、stack 与系统权限", cfg.TUN.Enable, active)
	}
	return nil
}

// Refresh 执行一轮完整流水线：fetch=true 时先拉取订阅与规则源，否则复用上次结果。
// 步骤：拉取/合并 → 健康检测 → 端口分配（稳定映射）→ 生成配置 → 热更新核心。
func (a *App) Refresh(ctx context.Context, fetch bool) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	var nodes []*node.Node
	if fetch || len(a.Nodes()) == 0 {
		a.mu.RLock()
		subs := make([]config.Subscription, len(a.cfg.Subscriptions))
		copy(subs, a.cfg.Subscriptions)
		ruleURLs := make([]config.RuleURL, len(a.cfg.RuleURLs))
		copy(ruleURLs, a.cfg.RuleURLs)
		manualEntries := make([]string, len(a.cfg.ManualNodes))
		copy(manualEntries, a.cfg.ManualNodes)
		a.mu.RUnlock()

		// 订阅与规则源并发拉取
		ruleCh := make(chan []ruleurl.Result, 1)
		go func() { ruleCh <- ruleurl.FetchAll(ctx, ruleURLs, a.cfg.StateDir) }()

		manual, manualErrs := subscribe.ParseManualNodes(manualEntries)
		for _, err := range manualErrs {
			if err != nil {
				log.Printf("[manual] %v", err)
			}
		}

		var errs []error
		var infos map[string]subscribe.UserInfo
		nodes, infos, errs = subscribe.FetchAllWithInfoAndFilters(ctx, subs, a.cfg.StateDir, a.includeRe, a.excludeRe,
			map[string][]*node.Node{subscribe.ManualSubscription: manual})
		for _, err := range errs {
			if err != nil {
				log.Printf("[subscribe] %v", err)
			}
		}
		a.applyRuleResults(<-ruleCh)
		if len(nodes) == 0 {
			return fmt.Errorf("no nodes available from any subscription")
		}
		a.mu.Lock()
		a.nodes = nodes
		a.subInfos = infos
		a.mu.Unlock()
	} else {
		// 仅测速刷新也必须重新应用当前订阅开关；否则刚从配置恢复的禁用订阅节点
		// 可能因旧内存快照继续参与健康检测和监听生成。
		nodes = filterEnabledSubscriptionNodes(a.Nodes(), a.Subscriptions())
	}

	pool.Check(ctx, nodes, a.cfg.HealthURL, a.cfg.HealthTimeout.D(), 32, a.dialerTargets()...)
	return a.applyNodes(ctx, nodes)
}

// applyNodes 执行健康检测后的流水线尾部，并完成链式代理的二阶段验证。
//
// 参数：
//   - ctx: context.Context，控制完整链路 URLTest 的取消与超时传播。
//   - nodes: []*node.Node，本轮订阅合并后的节点集合；健康状态会原地更新并保存快照。
//
// 返回值：error，没有任何可用节点、端口分配后的配置无法加载，或完整链路全部失败时返回。
//
// 错误情况：调用方必须持有 a.refreshing 锁。普通节点已经由 pool.Check 直接测速；
// dialer-proxy 节点先以依赖候选身份加载，随后通过 Runner.URLTest 验证真实完整链路。
// 若候选失败，会重新分配端口并热加载一次，确保失败链路不会残留在最终监听入口。
func (a *App) applyNodes(ctx context.Context, nodes []*node.Node) error {
	// 先更新应用节点快照，让 GenerateWithNodes 能看到未分配端口的链路依赖。
	// 刷新失败时仍保留本轮状态和失败原因，便于 Web/CLI 解释问题，而不是展示旧假象。
	a.mu.Lock()
	a.nodes = nodes
	a.mu.Unlock()

	alive := make([]*node.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Alive {
			alive = append(alive, n)
		}
	}
	if len(alive) == 0 {
		return fmt.Errorf("all %d nodes failed health check", len(nodes))
	}
	if capacity := a.cfg.Capacity(); len(alive) > capacity {
		log.Printf("[alloc] %d alive nodes exceed port capacity %d, keeping the fastest", len(alive), capacity)
	}

	prev, err := pool.LoadSnapshot(a.snapshotPath())
	if err != nil {
		log.Printf("[alloc] load snapshot: %v (ignored)", err)
	}
	assigns := pool.Allocate(alive, a.cfg.PortRange[0], a.cfg.PortRange[1], prev)
	if err := a.regenerateLocked(assigns); err != nil {
		return err
	}

	// pool.Check 无法在首次配置加载前解析 dialer-proxy 的运行时依赖。此处使用刚刚
	// 生效的 mihomo 代理表执行真实 URLTest；只在可用性发生变化时重新生成配置，
	// 延迟数值变化本身不触发第二次热更新，避免无意义地重建 listener。
	if a.verifyDialerNodes(ctx, nodes) {
		alive = alive[:0]
		for _, n := range nodes {
			if n.Alive {
				alive = append(alive, n)
			}
		}
		if len(alive) == 0 {
			// 候选配置已经短暂加载，不能直接返回并把失败链路 listener 留在运行态。
			// 生成空 assignment 配置会释放节点端口，同时保留主端口的 DIRECT 回退；
			// 清理失败时把两层错误一并返回，避免调用方误以为运行态已经安全收敛。
			if cleanupErr := a.regenerateLocked(nil); cleanupErr != nil {
				return fmt.Errorf("all %d dialer-proxy candidates failed end-to-end health check; cleanup failed: %w", len(nodes), cleanupErr)
			}
			a.mu.Lock()
			a.assigns = nil
			a.mu.Unlock()
			return fmt.Errorf("all %d dialer-proxy candidates failed end-to-end health check", len(nodes))
		}
		assigns = pool.Allocate(alive, a.cfg.PortRange[0], a.cfg.PortRange[1], prev)
		if err := a.regenerateLocked(assigns); err != nil {
			return err
		}
	}

	snap := &pool.Snapshot{Mapping: make(map[string]int, len(assigns))}
	for _, as := range assigns {
		snap.Mapping[as.Node.Key()] = as.Port
	}
	if err := pool.SaveSnapshot(a.snapshotPath(), snap); err != nil {
		log.Printf("[alloc] save snapshot: %v", err)
	}

	a.mu.Lock()
	a.assigns = assigns
	a.mu.Unlock()
	if err := node.SaveSnapshot(a.nodesSnapshotPath(), nodes); err != nil {
		log.Printf("[snapshot] 保存节点快照失败: %v", err)
	}
	log.Printf("[refresh] done: %d nodes, %d alive, %d ports mapped", len(nodes), len(alive), len(assigns))
	return nil
}

// verifyDialerNodes 使用已加载的 mihomo 代理表验证所有链式节点的真实端到端可用性。
//
// 参数：
//   - ctx: context.Context，整轮刷新取消时终止后续测试。
//   - nodes: []*node.Node，包含普通节点和 dialer-proxy 节点的本轮节点集合。
//
// 返回值：bool，只要任一链式节点从候选可用变为不可用就返回 true，提示调用方重生成配置。
//
// 错误情况：代理不存在、上游组不可用、网络失败与超时均写入 FailReason。检测串行执行，
// 因为 Runner 为保护 mihomo 全局代理表会持锁；这样避免并发 URLTest 与热更新产生竞态。
func (a *App) verifyDialerNodes(ctx context.Context, nodes []*node.Node) bool {
	availabilityChanged := false
	for _, n := range nodes {
		if n == nil || n.DialerProxy() == "" || !n.Alive {
			continue
		}
		delay, err := a.runner.URLTest(ctx, n.Name, a.cfg.HealthURL, a.cfg.HealthTimeout.D())
		if err != nil {
			n.Alive = false
			n.Delay = 0
			n.FailReason = firstErrorLine(err.Error())
			availabilityChanged = true
			continue
		}
		n.Delay = delay
		n.FailReason = ""
	}
	return availabilityChanged
}

// firstErrorLine 把底层多行错误压缩为适合节点状态展示的一行文本。
//
// 参数：
//   - message: string，可能包含换行、堆栈或协议详情的错误内容。
//
// 返回值：string，第一个换行符之前的内容；没有换行时返回原内容。
//
// 错误情况：无；空字符串原样返回。
func firstErrorLine(message string) string {
	if index := strings.IndexByte(message, '\n'); index >= 0 {
		return message[:index]
	}
	return message
}

// dialerTargets 返回当前配置中可被节点 dialer-proxy 引用的策略组名称快照。
//
// 参数：无；方法在读锁内读取 Config.Groups。
//
// 返回值：[]string，保持配置顺序的组名列表；调用方可自由修改返回切片。
//
// 错误情况：无；分组结构已经由 config.Validate 校验，空名称仍会在 pool 层忽略。
func (a *App) dialerTargets() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	targets := make([]string, 0, len(a.cfg.Groups))
	for _, group := range a.cfg.Groups {
		targets = append(targets, group.Name)
	}
	return targets
}

// restoreSnapshot 启动时加载 nodes.json 节点快照并立即生成 mihomo 配置提供服务，
// 不必等首次订阅刷新完成。快照缺失/损坏仅打日志丢弃，不致命。
// 之后 Run 里的首次 Refresh 成功会覆盖；失败则快照保持可用。
func (a *App) restoreSnapshot() {
	snap, err := node.LoadSnapshot(a.nodesSnapshotPath())
	if err != nil {
		log.Printf("[snapshot] %v", err)
		return
	}
	if snap == nil || len(snap.Nodes) == 0 {
		return
	}
	var alive []*node.Node
	for _, n := range snap.Nodes {
		if n.Alive {
			alive = append(alive, n)
		}
	}
	if len(alive) == 0 {
		log.Printf("[snapshot] 快照 %d 个节点均标记失效，等待首次刷新", len(snap.Nodes))
		return
	}

	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	prev, err := pool.LoadSnapshot(a.snapshotPath())
	if err != nil {
		log.Printf("[alloc] load snapshot: %v (ignored)", err)
	}
	assigns := pool.Allocate(alive, a.cfg.PortRange[0], a.cfg.PortRange[1], prev)
	if err := a.regenerateLocked(assigns); err != nil {
		log.Printf("[snapshot] 快照节点生成配置失败（等待首次刷新）: %v", err)
		return
	}
	a.mu.Lock()
	a.nodes = snap.Nodes
	a.assigns = assigns
	a.mu.Unlock()
	log.Printf("[snapshot] 已从快照恢复 %d 个节点（%d 个可用，%d 个端口，保存于 %s）",
		len(snap.Nodes), len(alive), len(assigns), snap.SavedAt.Format("2006-01-02 15:04:05"))
}
