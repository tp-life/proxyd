// Package remote 提供「远程连接」周边能力：基于 tailcat（Tailscale 数据面，
// 无控制面）的 WireGuard 隧道，把本机端口暴露给持有 token 的对端，或作为
// 客户端连接远端隧道端口（SSH 等场景）。本模块与代理功能完全独立，不经过 mihomo。
//
// tailcat 官方不承诺 API/wire format 稳定性，因此本项目只允许本包 import
// github.com/tailscale/tailcat，其余模块一律通过本包的抽象使用隧道能力。
package remote

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/tailscale/tailcat"

	"proxyd/internal/config"
)

// Status 是远程连接模块的运行时快照，供 API/Web 展示。
type Status struct {
	Enabled  bool            `json:"enabled"`          // 配置中的服务端开关
	Running  bool            `json:"running"`          // 隧道服务端是否在运行
	Error    string          `json:"error,omitempty"`  // 最近一次启动失败原因
	Token    string          `json:"token,omitempty"`  // 本机连接 token（tc...，运行中才有）
	Region   string          `json:"region,omitempty"` // 配置的 region 原值
	Serve    []int           `json:"serve"`            // 经隧道暴露的本机端口
	Forwards []ForwardStatus `json:"forwards"`         // 全部转发条目（含禁用项）
}

// ForwardStatus 是单条本地转发的运行时状态。
type ForwardStatus struct {
	Name       string `json:"name"`
	Listen     string `json:"listen"`        // 规范化后的本地监听地址
	Remote     string `json:"remote"`        // 配置原值（remotes 名称或 token）
	RemotePort int    `json:"remote_port"`
	Enabled    bool   `json:"enabled"`
	Running    bool   `json:"running"`
	Active     int64  `json:"active"` // 当前活动连接数
	LastError  string `json:"last_error,omitempty"`
}

// Manager 管理隧道服务端与本地转发的生命周期，跟随配置热更新做增量调和。
// 零值不可用，请用 NewManager 构造。
type Manager struct {
	stateDir string
	logf     func(format string, args ...any)

	mu       sync.Mutex
	cfg      config.RemoteConfig
	srv      *tailcat.Server
	token    string
	serveErr string
	forwards map[string]*forwardRunner
}

// NewManager 创建远程连接管理器；stateDir 用于持久化服务端密钥
// （<stateDir>/remote/server.private.json），logf 为 nil 时丢弃隧道内部日志。
func NewManager(stateDir string, logf func(format string, args ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{
		stateDir: stateDir,
		logf:     logf,
		forwards: map[string]*forwardRunner{},
	}
}

// Apply 按新配置调和运行状态：启停隧道服务端（serve/region 变化时重建），
// 增量增删本地转发。服务端启动失败时记录错误并返回，但转发仍会照常调和。
func (m *Manager) Apply(cfg config.RemoteConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var serveErr error
	if cfg.Enabled {
		if m.srv != nil && m.serverConfigEqual(cfg) {
			// 服务端配置未变，保留现有隧道（token 不变）。
		} else {
			m.stopServerLocked()
			if err := m.startServerLocked(cfg); err != nil {
				m.serveErr = err.Error()
				serveErr = fmt.Errorf("remote 服务端启动失败: %w", err)
			}
		}
	} else if m.srv != nil {
		m.stopServerLocked()
	}

	m.reconcileForwardsLocked(cfg)
	m.cfg = cfg.Clone()
	return serveErr
}

// serverConfigEqual 判断运行中的服务端是否与新配置等价（等价则无需重建隧道）。
func (m *Manager) serverConfigEqual(cfg config.RemoteConfig) bool {
	old := m.cfg
	if old.Region != cfg.Region || old.DERPMapURL != cfg.DERPMapURL {
		return false
	}
	if len(old.Serve) != len(cfg.Serve) {
		return false
	}
	ports := map[int]bool{}
	for _, p := range old.Serve {
		ports[p] = true
	}
	for _, p := range cfg.Serve {
		if !ports[p] {
			return false
		}
	}
	return true
}

// Status 返回当前运行时快照；配置中的禁用转发也会列出（Running=false）。
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := Status{
		Enabled: m.cfg.Enabled,
		Running: m.srv != nil,
		Error:   m.serveErr,
		Token:   m.token,
		Region:  m.cfg.Region,
		Serve:   append([]int(nil), m.cfg.Serve...),
	}
	if st.Serve == nil {
		st.Serve = []int{}
	}
	st.Forwards = make([]ForwardStatus, 0, len(m.cfg.Forwards))
	for _, f := range m.cfg.Forwards {
		fs := ForwardStatus{
			Name:       f.Name,
			Remote:     f.Remote,
			RemotePort: f.RemotePort,
			Enabled:    f.IsEnabled(),
		}
		fs.Listen, _ = config.NormalizeRemoteListen(f.Listen)
		if r, ok := m.forwards[f.Name]; ok {
			fs.Running = r.ln != nil
			fs.Active = r.active.Load()
			fs.LastError = r.lastError()
		}
		st.Forwards = append(st.Forwards, fs)
	}
	return st
}

// Close 停止服务端与全部转发，释放隧道资源。
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopServerLocked()
	for name, r := range m.forwards {
		r.stop()
		delete(m.forwards, name)
	}
}

// ResolveToken 把 remotes 名称解析为 token；以 "tc" 开头的输入按 token 原样返回。
func ResolveToken(remotes []config.RemotePeer, nameOrToken string) (string, error) {
	nameOrToken = strings.TrimSpace(nameOrToken)
	if nameOrToken == "" {
		return "", fmt.Errorf("远端不能为空")
	}
	if strings.HasPrefix(nameOrToken, "tc") {
		return nameOrToken, nil
	}
	for _, p := range remotes {
		if p.Name == nameOrToken {
			return p.Token, nil
		}
	}
	return "", fmt.Errorf("未找到远端 %q（也不是 tc... token）", nameOrToken)
}

// parseRegion 解析配置的 region 字段：空=自动就近；数字=DERP 区域 ID；
// 其余按自建 derper 主机名（逗号分隔）构造内嵌区域。
func parseRegion(s string) (regionID int, hosts []string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil, nil
	}
	if n, aerr := strconv.Atoi(s); aerr == nil {
		if n <= 0 {
			return 0, nil, fmt.Errorf("region 区域 ID 必须为正整数，got %d", n)
		}
		return n, nil, nil
	}
	if strings.Contains(s, ".") {
		for _, h := range strings.Split(s, ",") {
			h = strings.TrimSpace(h)
			if h == "" {
				return 0, nil, fmt.Errorf("region %q 含空主机名", s)
			}
			hosts = append(hosts, h)
		}
		return 0, hosts, nil
	}
	return 0, nil, fmt.Errorf("region %q 无效（留空自动 / 数字区域 ID / derper 主机名）", s)
}
