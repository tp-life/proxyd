package app

// 代理域：代理模式（rule/global/direct）查询与切换。

import (
	"fmt"
	"strings"

	"github.com/metacubex/mihomo/tunnel"
)

// Mode 返回当前代理模式（rule/global/direct）。
func (a *App) Mode() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.Mode
}

// SetMode 切换代理模式并持久化到配置文件，
// 防止下次热更新时被配置里的 mode 覆盖回去。
func (a *App) SetMode(mode string) error {
	m, ok := tunnel.ModeMapping[strings.ToLower(mode)]
	if !ok {
		return fmt.Errorf("invalid mode %q (rule|global|direct)", mode)
	}
	a.mu.Lock()
	a.cfg.Mode = m.String()
	err := a.persistLocked()
	a.mu.Unlock()
	if err != nil {
		return err
	}
	tunnel.SetMode(m)
	return nil
}
