package app

// 代理域：订阅刷新流水线（拉取/健康检测/端口分配/热更新）与启动快照恢复。

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/proxy/core"
	"proxyd/internal/proxy/groupstate"
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
	willListener := core.MainInboundIsListener(cfg, assigns, a.Nodes())
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
func (a *App) applyConfigLocked(cfg *config.Config, assigns []pool.Assignment, imported []string) (resultErr error) {
	generation := a.proxyLifecycle.Begin()
	// 热更新和启动共用状态收尾；外围刷新另行记录拉取失败，不依赖仅在 HTTP 操作时更新。
	defer func() {
		running := a.runner.Running()
		phase, message := "idle", ""
		if running {
			phase = "running"
		}
		if cfg.ProxyDisabled && !running {
			phase = "disabled"
		}
		if resultErr != nil {
			phase, message = "failed", "代理配置应用失败，请检查监听端口、TUN 权限和运行日志"
			if running {
				phase = "degraded"
			}
		}
		a.proxyLifecycle.Complete(generation, phase, running, message, 0)
	}()
	// 所有配置热更新共用此门，停用期间修改规则或端口也不能意外恢复监听。
	if cfg.ProxyDisabled {
		// Suspend 会关闭 TUN listener，helper 注入的 fd 随 mihomo 一并关闭，
		// 这里只清状态，恢复代理时重新申请。
		a.releaseTUNFDLocked(false)
		return a.runner.Suspend()
	}
	// darwin 普通用户模式下经 tun-helper 申请 utun fd 并注入生成配置；
	// fd 不落盘（injectTUNFD 作用于运行时副本）。
	fd, acquired, err := a.ensureTUNFDLocked(cfg)
	if err != nil {
		return err
	}
	cfg = injectTUNFD(cfg, fd)
	releaseOnError := func() {
		// fd 未随配置成功应用时所有权仍在本层，回滚必须关闭，避免泄漏 utun。
		if acquired {
			a.releaseTUNFDLocked(true)
		}
	}
	// select 分组的持久化选中项随每次生成注入 mihomo default-selected；
	// 状态文件损坏仅打日志丢弃，mihomo 回退到组成员首位。
	selected, err := groupstate.Load(a.groupSelectedPath())
	if err != nil {
		log.Printf("[groupstate] %v (ignored)", err)
	}
	cfgYAML, err := core.GenerateWithState(cfg, assigns, a.Nodes(), imported, selected)
	if err != nil {
		releaseOnError()
		return fmt.Errorf("generate mihomo config: %w", err)
	}
	if err := a.runner.Reload(cfgYAML); err != nil {
		releaseOnError()
		return fmt.Errorf("apply mihomo config: %w", err)
	}
	// mihomo 的 ReCreateTun 在创建虚拟网卡失败时只写日志，不把错误返回给 hub.Parse。
	// 因此必须在 Reload 返回后读取 listener 实际状态，否则可能把 enable:true 持久化，
	// 但运行时 TUN 已关闭。状态不一致作为应用失败返回，上层会恢复旧配置。
	if active := a.runner.TUNEnabled(); active != cfg.TUN.Enable {
		releaseOnError()
		return fmt.Errorf("mihomo TUN 实际状态与请求不一致（期望 enable=%t，实际 active=%t）；请检查 TUN 日志、stack 与系统权限", cfg.TUN.Enable, active)
	}
	if !cfg.TUN.Enable {
		// TUN 已随本次应用关闭，fd 由 mihomo 关闭（所有权已移交），只清持有状态。
		a.releaseTUNFDLocked(false)
	}
	return nil
}

// Refresh 执行一轮代理节点流水线：fetch=true 时显式拉取订阅与规则源；fetch=false
// 时只使用内存节点、已确认的本地订阅缓存和当前配置里的手动节点，绝不访问订阅 URL。
// 步骤：按请求拉取或重建节点集合 → 健康检测 → 稳定端口分配 → 生成配置 → 热更新核心。
//
// 参数：
//   - ctx: context.Context，控制订阅下载、规则源下载、健康检测和链路验证的取消。
//   - fetch: bool，只有用户明确执行同步时才传 true；后台健康检查必须传 false。
//
// 返回值：error，节点来源为空、全部节点失效、下载失败且无缓存，或 mihomo 热更新失败时返回。
//
// 错误情况：fetch=false 即使没有内存节点也不会隐式下载订阅，这是“订阅只允许手动同步”
// 的关键边界；新安装尚未同步时会返回无节点错误，由调用方保留当前 DIRECT 运行配置。
func (a *App) Refresh(ctx context.Context, fetch bool) (resultErr error) {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	return a.refreshLocked(ctx, fetch)
}

// refreshLocked 执行完整节点刷新流水线，但不自行获取 refreshing 锁。
//
// 参数说明：
//   - ctx: context.Context，控制订阅下载、规则源下载、健康检测与运行态应用。
//   - fetch: bool，true 时显式下载订阅；false 时只使用现有内存与本地缓存。
//
// 返回值说明：error，节点来源为空、检测全部失败或 mihomo 热更新失败时返回。
//
// 错误情况：调用方必须已经持有 refreshing 锁。该拆分让 Tailscale 一体化接入能把
// “修改配置 → 刷新节点池 → 持久化”纳入同一事务，避免在锁间隙被其它刷新覆盖。
func (a *App) refreshLocked(ctx context.Context, fetch bool) (resultErr error) {
	a.mu.RLock()
	disabled := a.cfg.ProxyDisabled
	a.mu.RUnlock()
	// 模块关闭时暂停手动同步与周期健康探测，避免已停用代理继续访问外网。
	if disabled {
		return nil
	}

	a.proxyLifecycle.Begin()
	defer func() {
		// 收尾覆盖最终状态：期间 applyConfigLocked 可能已 Begin/Complete 过，
		// 这里以当前世代直接提交最终结果，不再新增一次尝试计数。
		phase, message := "running", ""
		running := a.runner.Running()
		if !running {
			phase = "idle"
		}
		if resultErr != nil {
			phase = "failed"
			message = "代理刷新失败，请检查节点健康状态与运行日志"
			if running {
				phase = "degraded"
			}
		}
		a.proxyLifecycle.CompleteCurrent(phase, running, message, 0)
	}()
	var nodes []*node.Node
	if fetch {
		fetchOptions := a.subscriptionFetchOptions()
		a.mu.RLock()
		subs := make([]config.Subscription, len(a.cfg.Subscriptions))
		copy(subs, a.cfg.Subscriptions)
		ruleURLs := make([]config.RuleURL, len(a.cfg.RuleURLs))
		copy(ruleURLs, a.cfg.RuleURLs)
		manualEntries := make([]any, len(a.cfg.ManualNodes))
		copy(manualEntries, a.cfg.ManualNodes)
		stateDir := a.cfg.StateDir
		a.mu.RUnlock()

		// 订阅与规则源并发拉取
		ruleCh := make(chan []ruleurl.Result, 1)
		go func() { ruleCh <- ruleurl.FetchAll(ctx, ruleURLs, stateDir) }()

		manual, manualErrs := subscribe.ParseManualNodes(manualEntries)
		for _, err := range manualErrs {
			if err != nil {
				log.Printf("[manual] %v", err)
			}
		}

		var errs []error
		var infos map[string]subscribe.UserInfo
		nodes, infos, errs = subscribe.FetchAllWithInfoAndFiltersOptions(ctx, subs, stateDir, a.includeRe, a.excludeRe, fetchOptions,
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
		// 非下载路径仍需重新解析手动节点，因为新增/删除手动节点后 API 会复用这条
		// 流水线更新运行态。订阅节点只能来自已提交的内存快照，绝不能因为内存为空
		// 而回退到网络；这样启动、定时健康检查和模块恢复均不会产生订阅请求。
		a.mu.RLock()
		manualEntries := append([]any(nil), a.cfg.ManualNodes...)
		subscriptions := append([]config.Subscription(nil), a.cfg.Subscriptions...)
		stateDir := a.cfg.StateDir
		currentInfos := cloneSubscriptionInfos(a.subInfos)
		a.mu.RUnlock()
		manual, manualErrs := subscribe.ParseManualNodes(manualEntries)
		for _, err := range manualErrs {
			if err != nil {
				log.Printf("[manual] %v", err)
			}
		}
		bySource := map[string][]*node.Node{subscribe.ManualSubscription: manual}
		for _, existing := range filterEnabledSubscriptionNodes(a.Nodes(), subscriptions) {
			if existing.Subscription == subscribe.ManualSubscription {
				continue
			}
			bySource[existing.Subscription] = append(bySource[existing.Subscription], existing)
		}
		infos := make(map[string]subscribe.UserInfo, len(subscriptions))
		for _, subscription := range subscriptions {
			if !subscription.IsEnabled() {
				continue
			}
			if info, ok := currentInfos[subscription.Name]; ok {
				infos[subscription.Name] = info
			}
			if len(bySource[subscription.Name]) > 0 {
				continue
			}
			// 合并去重可能让某个订阅暂时没有内存节点；删除其它订阅或重新启用时，
			// 只从该订阅最后一次已确认缓存恢复，绝不为了补齐来源访问网络。
			cached, info, err := subscribe.LoadCachedWithInfo(subscription, stateDir)
			if err != nil {
				continue
			}
			bySource[subscription.Name] = cached
			if !info.IsZero() {
				infos[subscription.Name] = info
			}
		}
		nodes = subscribe.MergeFiltered(bySource, a.includeRe, a.excludeRe)
		if len(nodes) == 0 {
			return fmt.Errorf("no cached or manual nodes available; sync a subscription manually first")
		}
		a.mu.Lock()
		a.subInfos = infos
		a.mu.Unlock()
	}

	a.checkNodes(ctx, nodes, a.dialerTargets()...)
	return a.applyNodes(ctx, nodes)
}

// Testing 报告当前是否正在进行节点健康检测（测速）。
//
// 参数：无。
//
// 返回值：bool，pool.Check 执行期间为 true。
//
// 错误情况：无；只读原子标记，供概览接口把延迟列降级为「测速中」。
func (a *App) Testing() bool {
	return a.testing.Load()
}

// checkNodes 包裹 pool.Check，检测期间置位 Testing 标记。
//
// 参数：
//   - ctx: context.Context，控制整轮检测的取消与超时。
//   - nodes: []*node.Node，待检测节点，结果原地写回。
//   - dialerTargets: ...string，可作为链式目标的策略组名称。
//
// 返回值：无；单节点失败通过节点状态表达。
//
// 错误情况：检测串行化由调用方的 refreshing 锁保证，标记不存在并发竞争；
// 即使 panic 也经 defer 复位，不会让「测速中」状态残留。
func (a *App) checkNodes(ctx context.Context, nodes []*node.Node, dialerTargets ...string) {
	a.testing.Store(true)
	defer a.testing.Store(false)
	a.mu.RLock()
	healthURL := a.cfg.HealthURL
	healthTimeout := a.cfg.HealthTimeout.D()
	stateDir := a.cfg.StateDir
	a.mu.RUnlock()
	pool.Check(ctx, nodes, healthURL, healthTimeout, 32, stateDir, dialerTargets...)
}

// applyNodes 执行健康检测后的流水线尾部，并完成 mihomo 托管节点的二阶段验证。
//
// 参数：
//   - ctx: context.Context，控制完整链路 URLTest 的取消与超时传播。
//   - nodes: []*node.Node，本轮订阅合并后的节点集合；健康状态会原地更新并保存快照。
//
// 返回值：error，没有任何可用节点、端口分配后的配置无法加载，或完整链路全部失败时返回。
//
// 错误情况：调用方必须持有 a.refreshing 锁。普通节点已经由 pool.Check 直接测速；
// dialer-proxy 节点先以依赖候选身份加载，Tailscale 则避免在核心外启动临时 tsnet；
// 配置了 Exit Node 的 Tailscale 与普通链式节点随后通过 Runner.URLTest 验证真实链路。
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
	// 隧道类节点不参与端口映射，容量提示只统计实际需要一对一端口的节点。
	mappable := 0
	for _, n := range alive {
		if !n.IsTunnel() {
			mappable++
		}
	}
	a.mu.RLock()
	capacity := a.cfg.Capacity()
	portLo, portHi := a.cfg.PortRange[0], a.cfg.PortRange[1]
	a.mu.RUnlock()
	if mappable > capacity {
		log.Printf("[alloc] %d alive nodes exceed port capacity %d, keeping the fastest", mappable, capacity)
	}

	prev, err := pool.LoadSnapshot(a.snapshotPath())
	if err != nil {
		log.Printf("[alloc] load snapshot: %v (ignored)", err)
	}
	assigns := pool.Allocate(alive, portLo, portHi, prev)
	if err := a.regenerateLocked(assigns); err != nil {
		return err
	}

	// pool.Check 无法在首次配置加载前解析 dialer-proxy 的运行时依赖，也不得为
	// Tailscale 单独启动第二个 tsnet。此处使用刚生效的 mihomo 代理表执行真实
	// URLTest；只在可用性变化时重生成，延迟变化不触发无意义的 listener 重建。
	if a.verifyMihomoManagedNodes(ctx, nodes) {
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
		assigns = pool.Allocate(alive, portLo, portHi, prev)
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
	// 顺带调和网关执行层：mihomo 入口刚热更新完毕，失败的 gateway 应用到点重试，
	// 被外部清除的规则（如 helper 看门狗）也在此收敛。
	a.reconcileGatewayLocked()
	a.ensureInteractiveTailscaleEnrollments(nodes)
	log.Printf("[refresh] done: %d nodes, %d alive, %d ports mapped", len(nodes), len(alive), len(assigns))
	return nil
}

// ensureInteractiveTailscaleEnrollments 为没有 auth-key 的 mihomo Tailscale 出站恢复
// 管理员审批状态观察，并在进程重启后主动触发一次 tsnet 登录。
//
// 参数说明：nodes 是刚成功加载到 mihomo 代理表的完整节点集合。
//
// 返回值说明：无；触发在有界后台协程执行，不阻塞刷新事务。
//
// 错误情况：已经存在注册状态的节点不会重复触发；网络或审批等待错误由 Runner
// 状态记录。这样待审批配置即使重启，也能重新在管理面获得同一身份的注册链接。
func (a *App) ensureInteractiveTailscaleEnrollments(nodes []*node.Node) {
	for _, candidate := range nodes {
		if candidate == nil || !candidate.Alive || !candidate.IsTailscale() {
			continue
		}
		authKey, _ := candidate.Mapping["auth-key"].(string)
		if strings.TrimSpace(authKey) != "" {
			continue
		}
		if _, exists := a.runner.TailscaleEnrollment(candidate.Name); exists {
			continue
		}
		a.runner.BeginTailscaleEnrollment(candidate.Name, "approval")
		go func(name string) {
			triggerCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			a.runner.TriggerTailscaleEnrollment(triggerCtx, name)
		}(candidate.Name)
	}
}

// verifyMihomoManagedNodes 使用已加载的 mihomo 代理表验证需要运行态探测的节点。
//
// 参数：
//   - ctx: context.Context，整轮刷新取消时终止后续测试。
//   - nodes: []*node.Node，包含普通、dialer-proxy 与 Tailscale 节点的本轮节点集合。
//
// 返回值：bool，只要任一候选从可用变为不可用就返回 true，提示调用方重生成配置。
//
// 错误情况：代理不存在、上游组不可用、网络失败与超时均写入 FailReason。检测串行执行，
// 因为 Runner 为保护 mihomo 全局代理表会持锁；这样避免并发 URLTest 与热更新产生竞态。
// 未配置 Exit Node 的 Tailscale 只承载 Tailnet/子网路由，公网 health-url 对其没有意义，
// 因此保持“mihomo 已加载”候选状态，实际连接仍由 mihomo 按首次流量懒启动。
func (a *App) verifyMihomoManagedNodes(ctx context.Context, nodes []*node.Node) bool {
	availabilityChanged := false
	a.mu.RLock()
	healthURL := a.cfg.HealthURL
	healthTimeout := a.cfg.HealthTimeout.D()
	a.mu.RUnlock()
	for _, n := range nodes {
		if !needsMihomoRuntimeProbe(n) {
			continue
		}
		// Tailscale 首次启动可能需要完成控制面登录与 DERP 协商；继续复用代理域的
		// 隧道超时策略，避免迁移到正式 mihomo 运行态后退化为普通节点的短超时。
		timeout := pool.ProbeTimeout(n, healthTimeout)
		delay, err := a.runner.URLTest(ctx, n.Name, healthURL, timeout)
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

// needsMihomoRuntimeProbe 判断候选节点是否需要在正式 mihomo 代理表中执行公网测速。
//
// 参数：
//   - n: *node.Node，已经完成预检查的节点。
//
// 返回值：bool，普通 dialer-proxy 节点或配置了 Exit Node 的 Tailscale 返回 true。
//
// 错误情况：无；nil、预检查失败及仅访问 Tailnet/子网路由的 Tailscale 返回 false。
// Tailscale 的判断优先于 dialer-proxy，避免“使用上游拨号但未配置 Exit Node”的节点
// 被拿公网 health-url 误判为失效。
func needsMihomoRuntimeProbe(n *node.Node) bool {
	if n == nil || !n.Alive {
		return false
	}
	if n.IsTailscale() {
		return n.TailscaleExitNode() != ""
	}
	return n.DialerProxy() != ""
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

// restoreSnapshot 启动时只加载 nodes.json 节点快照并生成 mihomo 配置，不访问任何
// 订阅地址。快照缺失、损坏或没有健康节点时仍应用一份 DIRECT 配置，使控制台和主端口
// 可以正常启动，用户随后可通过手动同步取得节点。
//
// 参数：无；快照路径来自当前配置的 state-dir。
//
// 返回值：error，仅在 mihomo 配置生成、热加载或 TUN 实际状态校验失败时返回。
//
// 错误情况：快照读取失败会记录日志并降级为空节点启动，不会触发订阅下载；应用失败
// 必须返回给 Run，使要求 TUN 的配置仍能执行流量绕过保护。
func (a *App) restoreSnapshot() error {
	snap, err := node.LoadSnapshot(a.nodesSnapshotPath())
	if err != nil {
		log.Printf("[snapshot] %v", err)
	}
	var nodes []*node.Node
	if snap != nil {
		nodes = filterEnabledSubscriptionNodes(snap.Nodes, a.Subscriptions())
	}
	var alive []*node.Node
	for _, n := range nodes {
		if n.Alive {
			alive = append(alive, n)
		}
	}

	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	prev, err := pool.LoadSnapshot(a.snapshotPath())
	if err != nil {
		log.Printf("[alloc] load snapshot: %v (ignored)", err)
	}
	assigns := pool.Allocate(alive, a.cfg.PortRange[0], a.cfg.PortRange[1], prev)
	if err := a.regenerateLocked(assigns); err != nil {
		return fmt.Errorf("从节点快照生成启动配置失败: %w", err)
	}
	a.mu.Lock()
	a.nodes = nodes
	a.assigns = assigns
	a.mu.Unlock()
	if snap == nil || len(nodes) == 0 {
		log.Printf("[snapshot] 没有可用节点快照，已按空节点启动；请在控制台手动同步订阅")
		return nil
	}
	log.Printf("[snapshot] 已从快照恢复 %d 个节点（%d 个可用，%d 个端口，保存于 %s）",
		len(nodes), len(alive), len(assigns), snap.SavedAt.Format("2006-01-02 15:04:05"))
	return nil
}
