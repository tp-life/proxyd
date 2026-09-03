package app

// 代理域：端口区间、一对一端口映射、auto-port 与主端口设置用例。

import (
	"errors"
	"fmt"
	"log"

	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/pool"
	"proxyd/internal/proxy/sysproxy"
)

// Assignments 返回当前端口映射的只读快照（供 API 使用）。
func (a *App) Assignments() []pool.Assignment {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]pool.Assignment, len(a.assigns))
	copy(out, a.assigns)
	return out
}

// SetAutoPort 设置自动选优端口（0=关闭），持久化并热更新。
func (a *App) SetAutoPort(port int) error {
	a.mu.Lock()
	if err := a.cfg.CheckAutoPort(port); err != nil {
		a.mu.Unlock()
		return err
	}
	old := a.cfg.AutoPort
	a.cfg.AutoPort = port
	a.mu.Unlock()
	if err := a.Regenerate(); err != nil {
		a.mu.Lock()
		a.cfg.AutoPort = old
		a.mu.Unlock()
		_ = a.Regenerate()
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// SetMainAuto 开关「主端口使用最优节点」：开启后主端口跳过规则匹配、
// 固定走 AUTO url-test 组；关闭恢复规则模式。持久化并热更新。
// 主端口 mixed-port ↔ listener 形态转换的两阶段释放由 regenerateWithLocked 统一处理。
func (a *App) SetMainAuto(enabled bool) error {
	a.mu.Lock()
	old := a.cfg.MainAuto
	a.cfg.MainAuto = enabled
	a.mu.Unlock()
	if err := a.Regenerate(); err != nil {
		a.mu.Lock()
		a.cfg.MainAuto = old
		a.mu.Unlock()
		_ = a.Regenerate()
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// SetMainNode 设置主端口固定节点（node Key；空串 = 恢复规则模式），持久化并热更新。
// main-auto 开启时该设置被忽略（auto 优先），仍可保存。
// main-auto ↔ main-node（listener 同名 L<port>、仅 proxy 目标变化）
// 与 listener → mixed-port 方向由 mihomo 按 关闭→监听 顺序处理；
// mixed-port → listener 方向的过渡释放由 regenerateWithLocked 统一处理。
func (a *App) SetMainNode(key string) error {
	a.mu.Lock()
	old := a.cfg.MainNode
	a.cfg.MainNode = key
	a.mu.Unlock()
	if err := a.Regenerate(); err != nil {
		a.mu.Lock()
		a.cfg.MainNode = old
		a.mu.Unlock()
		_ = a.Regenerate()
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// SetMainPort 修改主端口（mixed-port），校验冲突后持久化并热更新；
// 系统代理当前已开启时自动重新绑定到新端口。
func (a *App) SetMainPort(port int) error {
	a.mu.Lock()
	if err := a.cfg.CheckMixedPort(port); err != nil {
		a.mu.Unlock()
		return err
	}
	old := a.cfg.MixedPort
	a.cfg.MixedPort = port
	sysOn := a.cfg.SystemProxy
	a.mu.Unlock()
	if err := a.Regenerate(); err != nil {
		a.mu.Lock()
		a.cfg.MixedPort = old
		a.mu.Unlock()
		_ = a.Regenerate()
		return err
	}
	if sysOn {
		if err := sysproxy.On("127.0.0.1", port); err != nil {
			log.Printf("[sysproxy] 主端口已改为 %d，但系统代理重绑失败: %v（可 proxyd sysproxy off 后重开）", port, err)
		} else {
			log.Printf("[sysproxy] 系统代理已跟随主端口重新指向 127.0.0.1:%d", port)
		}
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// SetPortRange 修改节点映射端口区间并持久化，随后用当前节点
// （沿用最近一次测速结果，不重新拉订阅/测速）重新分配端口并热更新。
func (a *App) SetPortRange(lo, hi int) error {
	if lo <= 0 || hi > 65535 || lo > hi {
		return fmt.Errorf("invalid port range [%d, %d]", lo, hi)
	}
	a.mu.Lock()
	if a.cfg.MixedPort >= lo && a.cfg.MixedPort <= hi {
		a.mu.Unlock()
		return fmt.Errorf("新区间 [%d, %d] 与主端口 %d 冲突", lo, hi, a.cfg.MixedPort)
	}
	if a.cfg.AutoPort != 0 && a.cfg.AutoPort >= lo && a.cfg.AutoPort <= hi {
		a.mu.Unlock()
		return fmt.Errorf("新区间 [%d, %d] 与 auto-port %d 冲突", lo, hi, a.cfg.AutoPort)
	}
	for _, g := range a.cfg.Groups {
		if g.Port >= lo && g.Port <= hi {
			a.mu.Unlock()
			return fmt.Errorf("新区间 [%d, %d] 与分组 %q 端口 %d 冲突", lo, hi, g.Name, g.Port)
		}
	}
	old := a.cfg.PortRange
	a.cfg.PortRange = [2]int{lo, hi}
	a.mu.Unlock()
	if err := a.reallocate(); err != nil {
		a.mu.Lock()
		a.cfg.PortRange = old
		a.mu.Unlock()
		_ = a.reallocate()
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// SetPortMapping 开关健康节点的一对一本地端口 listener，并以“热更新成功后再持久化”
// 的事务顺序提交。端口分配快照和 assignments 始终保留，因此重新开启时仍能恢复稳定端口。
//
// 参数：
//   - enabled: bool，true 表示创建每个健康节点对应的 mixed listener，false 表示只停用这些入口。
//
// 返回值：
//   - error：mihomo 配置生成/热更新失败或配置文件持久化失败时返回；成功返回 nil。
//
// 错误情况：任何失败都会恢复旧内存配置并重新应用旧运行态；若磁盘提交阶段失败，
// 还会尝试把旧配置重新写回。回滚中的多个错误使用 errors.Join 合并，避免掩盖原始原因。
func (a *App) SetPortMapping(enabled bool) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	a.mu.Lock()
	if a.cfg.PortMappingEnabled() == enabled {
		a.mu.Unlock()
		return nil
	}
	old := a.cfg.PortMapping
	a.cfg.PortMapping = new(bool)
	*a.cfg.PortMapping = enabled
	a.mu.Unlock()

	if err := a.regenerateCurrentLocked(); err != nil {
		return a.rollbackPortMappingLocked(old, err, false)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return a.rollbackPortMappingLocked(old, persistErr, true)
	}
	return nil
}

// rollbackPortMappingLocked 恢复端口映射开关对应的配置、运行态和可选磁盘状态。
// 调用方必须持有 refreshing 锁，确保回滚期间不会被订阅刷新或其它热更新穿插。
//
// 参数：
//   - old: *bool，变更前的原始指针；nil 表示沿用向后兼容的默认开启语义。
//   - cause: error，触发回滚的原始热更新或持久化错误。
//   - restoreDisk: bool，true 表示新配置已经尝试落盘，需要额外重写旧配置以消除不确定状态。
//
// 返回值：
//   - error：始终包含 cause；运行态或磁盘恢复失败时通过 errors.Join 一并返回。
//
// 错误情况：回滚失败不会被吞掉，API/CLI 可以明确告知用户人工检查运行态和配置文件。
func (a *App) rollbackPortMappingLocked(old *bool, cause error, restoreDisk bool) error {
	a.mu.Lock()
	a.cfg.PortMapping = old
	a.mu.Unlock()

	joined := cause
	if rollbackErr := a.regenerateCurrentLocked(); rollbackErr != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复旧端口映射运行态失败: %w", rollbackErr))
	}
	if restoreDisk {
		a.mu.Lock()
		rollbackErr := a.persistLocked()
		a.mu.Unlock()
		if rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复旧端口映射配置文件失败: %w", rollbackErr))
		}
	}
	return joined
}

// reallocate 用当前节点列表重新分配端口、保存快照并热更新核心（无网络操作）。
func (a *App) reallocate() error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	var alive []*node.Node
	for _, n := range a.Nodes() {
		if n.Alive {
			alive = append(alive, n)
		}
	}
	prev, err := pool.LoadSnapshot(a.snapshotPath())
	if err != nil {
		log.Printf("[alloc] load snapshot: %v (ignored)", err)
	}
	a.mu.RLock()
	lo, hi := a.cfg.PortRange[0], a.cfg.PortRange[1]
	a.mu.RUnlock()
	assigns := pool.Allocate(alive, lo, hi, prev)
	snap := &pool.Snapshot{Mapping: make(map[string]int, len(assigns))}
	for _, as := range assigns {
		snap.Mapping[as.Node.Key()] = as.Port
	}
	if err := pool.SaveSnapshot(a.snapshotPath(), snap); err != nil {
		log.Printf("[alloc] save snapshot: %v", err)
	}

	if err := a.regenerateLocked(assigns); err != nil {
		return err
	}

	a.mu.Lock()
	a.assigns = assigns
	a.mu.Unlock()
	log.Printf("[alloc] port range changed: %d ports mapped to %d-%d", len(assigns), lo, hi)
	return nil
}
