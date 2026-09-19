package app

// 代理域：局域网共享开关（listen 热切换）用例。

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// 局域网共享开关切换时使用的监听地址。
const (
	lanShareListenOn  = "0.0.0.0"
	lanShareListenOff = "127.0.0.1"
)

// isLoopbackListen 判断 listen 地址是否为本机回环：127.0.0.0/8、::1 与 localhost
// 视为回环；域名等无法解析为 IP 的值按非回环处理（与 core.isLoopback 语义一致）。
func isLoopbackListen(addr string) bool {
	if strings.EqualFold(strings.TrimSpace(addr), "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(addr))
	return err == nil && ip.IsLoopback()
}

// LANShare 返回局域网共享开关状态与当前 listen 地址；开关状态即「listen 是否为回环」。
func (a *App) LANShare() (enabled bool, listen string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return !isLoopbackListen(a.cfg.Listen), a.cfg.Listen
}

// SetLANShare 以事务方式开关局域网共享：开启时 listen 切到 0.0.0.0（所有代理入口
// 绑全网卡、allow-lan 生效），关闭时切回 127.0.0.1。当前已是非回环自定义地址
// （如 192.168.x.x）时开启为幂等 no-op，不覆盖用户地址；关闭从任意非回环地址
// 都重置回 127.0.0.1。全程热更新生效，无需重启进程。
//
// 参数：
//   - enabled: bool，true 表示共享给局域网设备，false 表示仅本机使用。
//
// 返回值：
//   - error：mihomo 配置生成/热更新失败或配置文件持久化失败时返回；成功返回 nil。
//
// 错误情况：任何失败都会恢复旧 listen 并重新应用旧运行态；若磁盘提交阶段失败，
// 还会尝试把旧配置重新写回。回滚中的多个错误使用 errors.Join 合并。
func (a *App) SetLANShare(enabled bool) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	a.mu.Lock()
	old := a.cfg.Listen
	var next string
	if enabled {
		if !isLoopbackListen(old) {
			// 已是非回环（含自定义网卡地址）：视为已开启，保持用户地址不变。
			a.mu.Unlock()
			return nil
		}
		next = lanShareListenOn
	} else {
		next = lanShareListenOff
	}
	if next == old {
		a.mu.Unlock()
		return nil
	}
	a.cfg.Listen = next
	a.mu.Unlock()

	if err := a.regenerateCurrentLocked(); err != nil {
		return a.rollbackLANShareLocked(old, err, false)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return a.rollbackLANShareLocked(old, persistErr, true)
	}
	return nil
}

// rollbackLANShareLocked 恢复局域网共享变更前的 listen、运行态和可选磁盘状态。
// 调用方必须持有 refreshing 锁，确保回滚期间不会被订阅刷新或其它热更新穿插。
//
// 参数：
//   - old: string，变更前的 listen 地址。
//   - cause: error，触发回滚的原始热更新或持久化错误。
//   - restoreDisk: bool，true 表示新配置已经尝试落盘，需要额外重写旧配置以消除不确定状态。
//
// 返回值：
//   - error：始终包含 cause；运行态或磁盘恢复失败时通过 errors.Join 一并返回。
//
// 错误情况：回滚失败不会被吞掉，API/CLI 可以明确告知用户人工检查运行态和配置文件。
func (a *App) rollbackLANShareLocked(old string, cause error, restoreDisk bool) error {
	a.mu.Lock()
	a.cfg.Listen = old
	a.mu.Unlock()

	joined := cause
	if rollbackErr := a.regenerateCurrentLocked(); rollbackErr != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复旧 listen 运行态失败: %w", rollbackErr))
	}
	if restoreDisk {
		a.mu.Lock()
		rollbackErr := a.persistLocked()
		a.mu.Unlock()
		if rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复旧 listen 配置文件失败: %w", rollbackErr))
		}
	}
	return joined
}
