package app

// 代理域：系统代理开关用例。

import (
	"errors"
	"fmt"

	"proxyd/internal/proxy/sysproxy"
)

// systemProxyController 定义应用层调整操作系统代理所需的最小端口。
// 平台命令细节仍由 proxy/sysproxy 基础设施适配器封装。
type systemProxyController interface {
	On(host string, port int) error
	Off() error
}

// platformSystemProxy 把应用层端口适配到当前操作系统实现。
type platformSystemProxy struct{}

// On 把系统 HTTP/HTTPS/SOCKS 代理指向给定地址。
//
// 参数说明：
//   - host: string，代理服务主机，当前固定为 127.0.0.1。
//   - port: int，当前 proxyd 主端口。
//
// 返回值说明：error，平台命令全部成功时为 nil。
//
// 错误情况：平台不支持、权限不足或任一系统网络服务设置失败时返回错误。
func (platformSystemProxy) On(host string, port int) error {
	return sysproxy.On(host, port)
}

// Off 关闭当前操作系统的 HTTP/HTTPS/SOCKS 代理。
//
// 参数说明：无。
//
// 返回值说明：error，平台命令全部成功时为 nil。
//
// 错误情况：平台不支持、权限不足或任一系统网络服务关闭失败时返回错误。
func (platformSystemProxy) Off() error {
	return sysproxy.Off()
}

// SetSystemProxy 以“OS 运行态 → 内存配置 → 磁盘配置”顺序开关系统代理。
//
// 参数说明：
//   - enabled: bool，true 指向当前主端口，false 关闭系统代理。
//
// 返回值说明：error，三层状态一致提交时为 nil。
//
// 错误情况：OS 设置或持久化失败时返回；持久化失败会恢复旧内存值
// 和 OS 状态，回滚失败通过 errors.Join 一并上报，不会出现 API 报错但系统已改变的静默漂移。
func (a *App) SetSystemProxy(enabled bool) error {
	a.systemProxyMu.Lock()
	defer a.systemProxyMu.Unlock()

	a.mu.RLock()
	oldEnabled := a.cfg.SystemProxy
	port := a.cfg.MixedPort
	a.mu.RUnlock()
	if err := a.applySystemProxy(enabled, port); err != nil {
		// macOS 会遍历多个网络服务，其中一项失败时前面的项可能
		// 已经改动；因此“首次调用返错”也要主动重放旧状态，不能
		// 假设底层命令具有原子性。
		if rollbackErr := a.applySystemProxy(oldEnabled, port); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("系统代理设置失败后恢复旧状态失败: %w", rollbackErr))
		}
		return err
	}
	a.mu.Lock()
	a.cfg.SystemProxy = enabled
	persistErr := a.persistLocked()
	if persistErr != nil {
		a.cfg.SystemProxy = oldEnabled
	}
	a.mu.Unlock()
	if persistErr == nil {
		return nil
	}
	if rollbackErr := a.applySystemProxy(oldEnabled, port); rollbackErr != nil {
		return errors.Join(persistErr, fmt.Errorf("恢复旧系统代理状态失败: %w", rollbackErr))
	}
	return persistErr
}

// applySystemProxy 把目标开关状态转换为平台端口调用。
//
// 参数说明：
//   - enabled: bool，目标开关状态。
//   - port: int，开启时应指向的 proxyd 主端口；关闭时忽略。
//
// 返回值说明：error，底层平台适配器的原始结果。
//
// 错误情况：操作系统不支持或设置失败时返回，由上层事务决定是否回滚。
func (a *App) applySystemProxy(enabled bool, port int) error {
	if enabled {
		return a.systemProxy.On("127.0.0.1", port)
	}
	return a.systemProxy.Off()
}
