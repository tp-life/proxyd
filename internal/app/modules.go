package app

// 模块管理只编排各上下文的生命周期，配置与运行规则仍由所属模块实现。

import (
	"context"
	"errors"
	"fmt"
	"proxyd/internal/config"
	"proxyd/internal/lifecycle"
	"proxyd/internal/proxy/tunperm"
	"time"
)

// ModuleState 是管理端的模块开关快照，Enabled 表示持久化期望状态。
type ModuleState struct {
	lifecycle.State
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// Modules 返回全部可管理模块，不访问网络也不泄露业务凭据。
// 参数：无；返回 []ModuleState；无错误，配置读锁保证快照一致。
func (a *App) Modules() []ModuleState {
	a.mu.RLock()
	proxyEnabled, remoteEnabled := !a.cfg.ProxyDisabled, !a.cfg.Remote.Disabled
	a.mu.RUnlock()
	proxyState, remoteState := a.proxyLifecycle.Snapshot(), a.remoteLifecycle.Snapshot()
	if !proxyEnabled {
		proxyState.Phase = "disabled"
		proxyState.Running = false
		proxyState.NextRetryAt = nil
	}
	if !remoteEnabled {
		remoteState.Phase = "disabled"
		remoteState.Running = false
		remoteState.NextRetryAt = nil
	}
	return []ModuleState{{State: proxyState, ID: "proxy", Name: "代理", Enabled: proxyEnabled}, {State: remoteState, ID: "remote", Name: "远程访问", Enabled: remoteEnabled}}

}

// SetModuleEnabled 路由模块生命周期用例，保留模块内部的服务开关与对象配置。
// 参数：id 为模块标识，enabled 为目标状态；返回 error，未知模块或事务失败返回错误。
// 禁用会结束当前连接；回滚可恢复监听配置，但不会复活已结束的会话。
func (a *App) SetModuleEnabled(id string, enabled bool) error {
	switch id {
	case "proxy":
		if err := a.setProxyModuleEnabled(enabled); err != nil {
			return err
		}
		if enabled {
			select {
			case a.proxyResume <- struct{}{}:
			default:
			}
		}
		return nil
	case "remote":
		a.remoteMutationMu.Lock()
		defer a.remoteMutationMu.Unlock()
		if err := a.mutateRemoteLocked(func(r *config.RemoteConfig) error { r.Disabled = !enabled; return nil }); err != nil {
			return err
		}
		if !enabled && a.desktop != nil {
			for _, session := range a.desktop.List() {
				_ = a.desktop.Stop(session.ID)
			}
		}
		return nil
	default:
		return fmt.Errorf("未知模块 %q", id)
	}
}

// setProxyModuleEnabled 在已有系统代理与刷新锁顺序下切换代理数据面。
// 参数：enabled 为 bool；返回 error，权限、热更新、系统代理或落盘失败时回滚。
// 先撤销系统代理再释放监听，恢复时先建监听再恢复系统代理，避免指向停止的本地端口。
func (a *App) setProxyModuleEnabled(enabled bool) error {
	a.systemProxyMu.Lock()
	defer a.systemProxyMu.Unlock()
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.Lock()
	old := a.cfg.ProxyDisabled
	if old == !enabled {
		a.mu.Unlock()
		return nil
	}
	cfg := a.cfg.Clone()
	a.mu.Unlock()
	if enabled && cfg.TUN.Enable {
		if err := tunperm.Require(); err != nil {
			return err
		}
	}
	if enabled && len(cfg.Subscriptions) == 0 && len(cfg.ManualNodes) == 0 {
		return fmt.Errorf("启用代理前请先添加订阅或手动节点")
	}
	a.mu.Lock()
	a.cfg.ProxyDisabled = !enabled
	a.mu.Unlock()
	// apply 按目标状态调和资源。参数 disabled 为 bool；返回 error，OS 或核心失败向事务传播。
	// 此闭包同时用于正向应用与回滚，调用时已持有刷新锁，不能再次获取同一锁。
	apply := func(disabled bool) error {
		if disabled && cfg.SystemProxy {
			if err := a.applySystemProxy(false, cfg.MixedPort); err != nil {
				return err
			}
		}
		if err := a.regenerateCurrentLocked(); err != nil {
			return err
		}
		if !disabled && cfg.SystemProxy {
			return a.applySystemProxy(true, cfg.MixedPort)
		}
		return nil
	}
	err := apply(!enabled)
	if err == nil {
		a.mu.Lock()
		err = a.persistLocked()
		a.mu.Unlock()
	}
	if err == nil {
		return nil
	}
	a.mu.Lock()
	a.cfg.ProxyDisabled = old
	a.mu.Unlock()
	return errors.Join(err, apply(old))
}

// applyRemoteRuntime 将远程配置应用与观测状态封装为单一用例，调用者持有 remoteMutationMu。
// 参数 cfg 为独立配置；返回 error，失败仍记录实际可用子服务与重试信息。
func (a *App) applyRemoteRuntime(cfg config.RemoteConfig) error {
	generation := a.remoteLifecycle.Begin()
	err := a.remote.Apply(cfg)
	status := a.remote.Status()
	running := status.Running || status.WebTerminal
	message := ""
	phase := "idle"
	retry := time.Duration(0)
	for _, forward := range status.Forwards {
		running = running || forward.Running
		if forward.Enabled && forward.LastError != "" {
			message = "部分端口转发未启动，请检查监听端口"
		}
	}
	if running {
		phase = "running"
	}
	if message != "" {
		phase = "degraded"
		retry = 30 * time.Second
	}
	if err != nil {
		message = "远程服务启动失败，请运行连接诊断并检查运行日志"
		phase = "retrying"
		retry = 30 * time.Second
	}
	if cfg.Disabled {
		phase = "disabled"
		running = false
		message = ""
		retry = 0
	}
	a.remoteLifecycle.Complete(generation, phase, running, message, retry)
	return err
}

// RetryModule 立即重试模块，已禁用模块不隐式启用。
// 参数 ctx 为有界请求上下文，id 为模块标识；返回 error，网络失败保持真实运行状态。
func (a *App) RetryModule(ctx context.Context, id string) error {
	cfg := a.Config()
	switch id {
	case "proxy":
		if cfg.ProxyDisabled {
			return fmt.Errorf("代理模块已禁用")
		}
		return a.Refresh(ctx, true)
	case "remote":
		a.remoteMutationMu.Lock()
		defer a.remoteMutationMu.Unlock()
		a.mu.RLock()
		remoteCfg := a.cfg.Remote.Clone()
		a.mu.RUnlock()
		if remoteCfg.Disabled {
			return fmt.Errorf("远程访问模块已禁用")
		}
		return a.applyRemoteRuntime(remoteCfg)
	default:
		return fmt.Errorf("未知模块 %q", id)
	}
}
