package core

import (
	"fmt"
	"os"
	"sync"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub"
	"github.com/metacubex/mihomo/hub/executor"
)

// Runner 以库方式内嵌运行 mihomo 核心，支持启动、热更新与关闭。
type Runner struct {
	mu       sync.Mutex
	stateDir string
	started  bool
}

// NewRunner 创建 Runner。stateDir 作为 mihomo 的 home 目录（存 cache.db / geo 文件），
// 为空则沿用 mihomo 默认路径。
// 注意：SetHomeDir 必须在任何配置解析（含 Generate 的自检 ParseWithBytes）之前完成，
// 否则 geo 文件路径为空，GEOSITE/GEOIP 规则会因 "open :" 报错。
func NewRunner(stateDir string) *Runner {
	if stateDir != "" {
		if err := os.MkdirAll(stateDir, 0o755); err == nil {
			C.SetHomeDir(stateDir)
		}
	}
	return &Runner{stateDir: stateDir}
}

// Start 首次启动 mihomo 核心：整体应用配置。
func (r *Runner) Start(cfgYAML []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := hub.Parse(cfgYAML); err != nil {
		return fmt.Errorf("应用 mihomo 配置: %w", err)
	}
	r.started = true
	return nil
}

// Reload 热更新配置：hub.Parse 内部已 ApplyConfig（重建 REST API 与所有 listener）。
// 未 Start 时等价于 Start。
func (r *Runner) Reload(cfgYAML []byte) error {
	return r.Start(cfgYAML)
}

// Shutdown 关闭 mihomo 核心（清理 listener 等）。
func (r *Runner) Shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	executor.Shutdown()
	r.started = false
}
