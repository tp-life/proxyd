package app

// 代理域：系统代理开关用例。

import (
	"proxyd/internal/proxy/sysproxy"
)

// SetSystemProxy 开关系统代理（指向主端口），立即应用并持久化配置。
func (a *App) SetSystemProxy(enabled bool) error {
	var err error
	if enabled {
		err = sysproxy.On("127.0.0.1", a.cfg.MixedPort)
	} else {
		err = sysproxy.Off()
	}
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg.SystemProxy = enabled
	err = a.persistLocked()
	a.mu.Unlock()
	return err
}
