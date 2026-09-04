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
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"

	"proxyd/internal/config"
)

// Status 是远程连接模块的运行时快照，供 API/Web 展示。
type Status struct {
	Enabled         bool                      `json:"enabled"`                   // 配置中的服务端开关
	Running         bool                      `json:"running"`                   // 隧道服务端是否在运行
	Error           string                    `json:"error,omitempty"`           // 最近一次启动失败原因
	Token           string                    `json:"token,omitempty"`           // 本机连接 token（tc...，运行中才有）
	ClientKey       string                    `json:"client_key,omitempty"`      // 客户端 node 公钥（对端 --allow 白名单用；非凭据，不打码）
	Region          string                    `json:"region,omitempty"`          // 配置的 region 原值
	Serve           []int                     `json:"serve"`                     // 经隧道暴露的本机端口
	Allow           []config.RemoteAllowEntry `json:"allow"`                     // 客户端公钥白名单（含别名、TTL 与端口限制）
	AllowRestricted bool                      `json:"allow_restricted"`          // 授权清扫为空后是否继续保持拒绝模式
	TempKey         string                    `json:"temp_key,omitempty"`        // 临时身份公钥（应急 nodekey）
	KeyFile         string                    `json:"key_file"`                  // 实际使用的服务端密钥文件路径（内置托管或 key-file 指定）
	CustomKeyFile   string                    `json:"custom_key_file,omitempty"` // key-file 配置原值（空=内置托管密钥）
	BuiltinSSH      bool                      `json:"builtin_ssh"`               // 内嵌免密 SSH 服务开关（隧道 22 端口由进程内 SSH 处理）
	WebTerminal     bool                      `json:"web_terminal"`              // 浏览器终端总开关；默认关闭，API 层关闭时直接返回 404
	// ClientActivity 是各已知客户端公钥（白名单+临时身份）当前的入站活动连接数。
	ClientActivity map[string]int64  `json:"client_activity,omitempty"`
	Peers          []PeerObservation `json:"peers"`    // 已知入站客户端的连接路径与累计流量
	Forwards       []ForwardStatus   `json:"forwards"` // 全部转发条目（含禁用项）
}

// ForwardStatus 是单条本地转发的运行时状态。
type ForwardStatus struct {
	Name       string `json:"name"`
	Listen     string `json:"listen"` // 规范化后的本地监听地址
	Remote     string `json:"remote"` // 配置原值（remotes 名称或 token）
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

	mu               sync.Mutex
	cfg              config.RemoteConfig
	srv              *tailcat.Server
	token            string
	serveErr         string
	forwards         map[string]*forwardRunner
	activeClients    map[string]int64    // 已知客户端公钥 → 当前入站活动连接数
	audit            *auditLog           // 独立于代理日志的固定容量连接审计环
	clientLoaded     bool                // 是否已尝试加载客户端身份密钥
	clientPriv       key.NodePrivate     // 持久客户端身份（对端 --allow 白名单用）
	clientErr        error               // 客户端密钥加载失败原因（惰性加载只尝试一次）
	autoRegion       *tailcfg.DERPRegion // 自动就近模式的探测结果缓存（进程内粘性，防 token 漂移）
	autoRegionMapURL string              // 产生缓存时的 DERPMapURL
}

// NewManager 创建远程连接管理器；stateDir 用于持久化服务端密钥
// （<stateDir>/remote/server.private.json，可用配置 key-file 覆盖），
// logf 为 nil 时丢弃隧道内部日志。
func NewManager(stateDir string, logf func(format string, args ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{
		stateDir:      stateDir,
		logf:          logf,
		forwards:      map[string]*forwardRunner{},
		activeClients: map[string]int64{},
		audit:         newAuditLog(remoteAuditCapacity),
	}
}

// clientKeyLocked 惰性加载持久客户端身份密钥（首次调用时读盘/生成，之后缓存）。
// 失败时缓存错误并返回零值密钥，调用方回退为临时身份。调用方需持有 m.mu。
func (m *Manager) clientKeyLocked() key.NodePrivate {
	if !m.clientLoaded {
		m.clientLoaded = true
		m.clientPriv, m.clientErr = LoadOrCreateClientKey(m.stateDir)
		if m.clientErr != nil {
			m.logf("[remote] 客户端身份密钥加载失败，回退为临时身份: %v", m.clientErr)
		}
	}
	return m.clientPriv
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
// WebTerminal 只控制本机 HTTP/WS 入口，不改变 tailcat 数据面，因此不参与比较；
// 切换它不会中断已有隧道连接或改变 token。
func (m *Manager) serverConfigEqual(cfg config.RemoteConfig) bool {
	old := m.cfg
	if old.Region != cfg.Region || old.DERPMapURL != cfg.DERPMapURL || old.TempKey != cfg.TempKey || old.KeyFile != cfg.KeyFile || old.BuiltinSSH != cfg.BuiltinSSH || old.AllowRestricted != cfg.AllowRestricted {
		return false
	}
	if len(old.Serve) != len(cfg.Serve) || len(old.Allow) != len(cfg.Allow) {
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
	allowed := map[string]bool{}
	for _, e := range old.Allow {
		allowed[e.Key] = true
	}
	for _, e := range cfg.Allow {
		if !allowed[e.Key] {
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
		Enabled:         m.cfg.Enabled,
		Running:         m.srv != nil,
		Error:           m.serveErr,
		Token:           m.token,
		Region:          m.cfg.Region,
		Serve:           append([]int(nil), m.cfg.Serve...),
		Allow:           m.cfg.Clone().Allow,
		AllowRestricted: m.cfg.AllowRestricted,
		TempKey:         m.cfg.TempKey,
		KeyFile:         m.serverKeyPath(m.cfg),
		CustomKeyFile:   strings.TrimSpace(m.cfg.KeyFile),
		BuiltinSSH:      m.cfg.BuiltinSSH,
		WebTerminal:     m.cfg.WebTerminal,
	}
	if m.srv != nil {
		st.Peers = peerObservations(m.srv.Status(), m.cfg.Allow, m.cfg.TempKey, m.activeClients, time.Now())
	}
	if len(m.activeClients) > 0 {
		st.ClientActivity = make(map[string]int64, len(m.activeClients))
		for k, n := range m.activeClients {
			if n > 0 {
				st.ClientActivity[k] = n
			}
		}
	}
	if priv := m.clientKeyLocked(); !priv.IsZero() {
		st.ClientKey = priv.Public().String()
	}
	if st.Serve == nil {
		st.Serve = []int{}
	}
	if st.Allow == nil {
		st.Allow = []config.RemoteAllowEntry{}
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

// StartTransientForward 使用管理器持久化的客户端身份启动一条临时本地转发。
//
// 参数说明：
//   - token: string，远端完整 tailcat token。
//   - remotePort: int，远端实际 TCP 服务端口。
//
// 返回值说明：*TransientForward 和 error；成功对象由调用方负责 Close。
//
// 错误情况：token/端口非法、客户端身份文件不可用或本机回环监听失败时返回错误。
// 身份文件读取失败时沿用 remote 既有降级语义，使用临时身份继续尝试连接。
func (m *Manager) StartTransientForward(token string, remotePort int) (*TransientForward, error) {
	m.mu.Lock()
	clientKey := m.clientKeyLocked()
	m.mu.Unlock()
	return StartTransientForward(token, remotePort, clientKey)
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
