// Package desktop 提供远程桌面连接会话的领域模型与生命周期管理。
//
// 本包不依赖 tailcat、HTTP、配置文件或操作系统 GUI；调用方通过 ForwardFactory
// 注入临时 TCP 隧道实现，从而保持桌面会话规则与 remote 数据面相互隔离。
package desktop

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Protocol 是桌面会话使用的应用协议值对象。
type Protocol string

const (
	// ProtocolRDP 表示 Microsoft Remote Desktop Protocol。
	ProtocolRDP Protocol = "rdp"
	// ProtocolVNC 表示 Virtual Network Computing / Remote Framebuffer。
	ProtocolVNC Protocol = "vnc"
)

var (
	// ErrManagerClosed 表示桌面会话管理器已经关闭，不再接受新会话。
	ErrManagerClosed = errors.New("远程桌面会话管理器已关闭")
	// ErrSessionNotFound 表示指定会话不存在或已经被自动回收。
	ErrSessionNotFound = errors.New("远程桌面会话不存在")
)

// ParseProtocol 把外部文本转换为受约束的桌面协议值对象。
//
// 参数说明：value 为 API、CLI 或配置传入的协议文本，忽略大小写与首尾空白。
//
// 返回值说明：Protocol 和 error；rdp/vnc 返回对应常量。
//
// 错误情况：未知协议返回可读错误，调用方不得继续创建会话。
func ParseProtocol(value string) (Protocol, error) {
	protocol := Protocol(strings.ToLower(strings.TrimSpace(value)))
	switch protocol {
	case ProtocolRDP, ProtocolVNC:
		return protocol, nil
	default:
		return "", fmt.Errorf("桌面协议 %q 不受支持（可选 rdp 或 vnc）", value)
	}
}

// SessionSpec 是创建桌面会话所需的不可变输入快照。
//
// Remote 保存 remote 上下文中的远端引用；领域层不会读取 token，也不会解释远端身份。
type SessionSpec struct {
	ConnectionName string
	Remote         string
	Protocol       Protocol
	RemotePort     int
	Username       string
}

// Validate 校验创建会话所需的业务不变量。
//
// 参数说明：无。
//
// 返回值说明：error；名称、远端、协议和端口均合法时返回 nil。
//
// 错误情况：空名称、空远端、未知协议或端口越界会返回明确错误，且不会调用隧道工厂。
func (s SessionSpec) Validate() error {
	if strings.TrimSpace(s.ConnectionName) == "" {
		return fmt.Errorf("桌面连接名称不能为空")
	}
	if strings.TrimSpace(s.Remote) == "" {
		return fmt.Errorf("桌面连接远端不能为空")
	}
	if s.Protocol != ProtocolRDP && s.Protocol != ProtocolVNC {
		return fmt.Errorf("桌面协议 %q 不受支持", s.Protocol)
	}
	if s.RemotePort < 1 || s.RemotePort > 65535 {
		return fmt.Errorf("桌面远端端口 %d 超出 1-65535", s.RemotePort)
	}
	return nil
}

// Forward 是桌面会话依赖的最小临时隧道端口。
//
// remote 适配器负责实现实际 tailcat 连接；领域层只关心本地地址、活动连接数与释放动作。
type Forward interface {
	Address() string
	ActiveConnections() int64
	Close() error
}

// ForwardFactory 是应用层注入的临时隧道创建端口。
//
// 参数说明：spec 是已经通过 Validate 的桌面会话输入。
//
// 返回值说明：Forward 和 error；成功对象的所有权移交给 Manager。
//
// 错误情况：远端解析、NAT/DERP 初始化或本地监听失败时返回错误，工厂必须自行清理
// 失败过程中创建的资源。
type ForwardFactory func(spec SessionSpec) (Forward, error)

// Session 是对 API/Web 可见的桌面会话快照。
type Session struct {
	ID                string
	ConnectionName    string
	Protocol          Protocol
	RemotePort        int
	Username          string
	LocalAddress      string
	StartedAt         time.Time
	ActiveConnections int64
}

// Options 控制遗忘会话的自动回收边界。
//
// StartupGrace 是等待桌面客户端首次连接的时间；IdleTimeout 是已有连接全部断开后的
// 保留时间；MaxLifetime 是无论客户端状态如何都必须回收的硬上限；SweepInterval 控制
// 单个固定清扫协程的检查频率，避免为每个会话创建定时器。
type Options struct {
	StartupGrace  time.Duration
	IdleTimeout   time.Duration
	MaxLifetime   time.Duration
	SweepInterval time.Duration
}

// managedSession 保存领域快照之外的隧道所有权与空闲判定状态。
type managedSession struct {
	Session
	forward    Forward
	hadActive  bool
	lastActive time.Time
}

// Manager 管理进程内临时桌面会话，保证显式断开、空闲超时和应用关闭都释放隧道。
type Manager struct {
	mu        sync.Mutex
	factory   ForwardFactory
	options   Options
	now       func() time.Time
	sessions  map[string]*managedSession
	stop      chan struct{}
	done      chan struct{}
	closed    bool
	startOnce sync.Once
	closeOnce sync.Once
}

// NewManager 创建桌面会话管理器；后台清扫循环会在首个会话成功建立时惰性启动。
//
// 参数说明：factory 创建底层临时隧道；options 控制回收时间，零值字段使用保守默认值。
//
// 返回值说明：*Manager；调用方在应用退出时必须调用 Close。
//
// 错误情况：构造本身不执行网络操作，因此不返回错误；nil factory 会在 Start 时返回错误。
func NewManager(factory ForwardFactory, options Options) *Manager {
	options = normalizeOptions(options)
	return &Manager{
		factory:  factory,
		options:  options,
		now:      time.Now,
		sessions: map[string]*managedSession{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// normalizeOptions 为缺失或非正的回收参数填入默认值。
//
// 参数说明：options 为调用方配置。
//
// 返回值说明：Options，所有字段都为正数且 MaxLifetime 不小于其它超时。
//
// 错误情况：无；不合理的小 MaxLifetime 会提升到启动宽限与空闲超时中的较大值。
func normalizeOptions(options Options) Options {
	if options.StartupGrace <= 0 {
		options.StartupGrace = 2 * time.Minute
	}
	if options.IdleTimeout <= 0 {
		options.IdleTimeout = time.Minute
	}
	if options.MaxLifetime <= 0 {
		options.MaxLifetime = 12 * time.Hour
	}
	if options.SweepInterval <= 0 {
		options.SweepInterval = 15 * time.Second
	}
	minimumLifetime := options.StartupGrace
	if options.IdleTimeout > minimumLifetime {
		minimumLifetime = options.IdleTimeout
	}
	if options.MaxLifetime < minimumLifetime {
		options.MaxLifetime = minimumLifetime
	}
	return options
}

// Start 创建连接档案对应的临时桌面会话，同一档案已有会话时直接复用。
//
// 参数说明：spec 是从持久化连接档案生成的不可变快照。
//
// 返回值说明：Session 和 error；成功返回本地回环地址及会话 ID。
//
// 错误情况：输入非法、管理器关闭、随机 ID 生成失败或隧道工厂失败时返回错误。并发
// 启动同一档案时只保留一条会话，多创建的隧道会立即关闭，避免端口和协程泄漏。
func (m *Manager) Start(spec SessionSpec) (Session, error) {
	if err := spec.Validate(); err != nil {
		return Session{}, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Session{}, ErrManagerClosed
	}
	if existing := m.findByConnectionLocked(spec.ConnectionName); existing != nil {
		snapshot := m.snapshotLocked(existing)
		m.mu.Unlock()
		return snapshot, nil
	}
	m.mu.Unlock()

	id, err := newSessionID()
	if err != nil {
		return Session{}, err
	}
	if m.factory == nil {
		return Session{}, fmt.Errorf("未配置远程桌面隧道工厂")
	}
	forward, err := m.factory(spec)
	if err != nil {
		return Session{}, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = forward.Close()
		return Session{}, ErrManagerClosed
	}
	if existing := m.findByConnectionLocked(spec.ConnectionName); existing != nil {
		snapshot := m.snapshotLocked(existing)
		m.mu.Unlock()
		_ = forward.Close()
		return snapshot, nil
	}
	now := m.now()
	session := &managedSession{
		Session: Session{
			ID:             id,
			ConnectionName: strings.TrimSpace(spec.ConnectionName),
			Protocol:       spec.Protocol,
			RemotePort:     spec.RemotePort,
			Username:       strings.TrimSpace(spec.Username),
			LocalAddress:   forward.Address(),
			StartedAt:      now,
		},
		forward:    forward,
		lastActive: now,
	}
	m.sessions[id] = session
	snapshot := m.snapshotLocked(session)
	m.mu.Unlock()
	m.startCleanup()
	return snapshot, nil
}

// List 返回当前所有桌面会话快照，按启动时间从新到旧排序。
//
// 参数说明：无。
//
// 返回值说明：[]Session；没有会话时返回空切片而不是 nil，便于 JSON 稳定编码。
//
// 错误情况：无；读取过程中会刷新活动连接数，但不会执行网络 I/O。
func (m *Manager) List() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		out = append(out, m.snapshotLocked(session))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out
}

// Get 返回指定会话的当前快照。
//
// 参数说明：id 为 Start 返回的不可猜测会话标识。
//
// 返回值说明：Session 和 error；存在时包含最新活动连接数。
//
// 错误情况：ID 为空、会话不存在或已被回收时返回 ErrSessionNotFound。
func (m *Manager) Get(id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[strings.TrimSpace(id)]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return m.snapshotLocked(session), nil
}

// Stop 显式结束一个桌面会话并释放底层隧道。
//
// 参数说明：id 为待结束的会话标识。
//
// 返回值说明：error；找到并完成关闭时返回 Forward.Close 的结果。
//
// 错误情况：会话不存在返回 ErrSessionNotFound；即使底层 Close 返回错误，会话仍从
// 管理器移除，避免一个失效资源永远占据档案的唯一会话位置。
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	session, ok := m.sessions[strings.TrimSpace(id)]
	if !ok {
		m.mu.Unlock()
		return ErrSessionNotFound
	}
	delete(m.sessions, session.ID)
	m.mu.Unlock()
	return session.forward.Close()
}

// Close 幂等关闭管理器、停止清扫协程并释放全部会话。
//
// 参数说明：无。
//
// 返回值说明：无；底层关闭为尽力而为，应用退出路径不因单个隧道错误阻塞。
//
// 错误情况：重复调用安全；第一次调用会等待固定清扫协程退出，但不会等待网络连接超时。
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		// 即使从未建立会话，也通过同一个 once 启动清扫协程，使 done 的关闭协议保持
		// 单一，不需要用额外布尔值在 Close 与并发 Start 之间做脆弱判断。
		m.startCleanup()
		m.mu.Lock()
		m.closed = true
		forwards := make([]Forward, 0, len(m.sessions))
		for id, session := range m.sessions {
			forwards = append(forwards, session.forward)
			delete(m.sessions, id)
		}
		close(m.stop)
		m.mu.Unlock()
		// Forward.Close 可能等待连接协程收口，必须在管理器锁外执行，否则会阻塞并发
		// Get/Stop，甚至与正准备进入 sweep 的清扫协程形成退出死锁。
		for _, forward := range forwards {
			_ = forward.Close()
		}
		<-m.done
	})
}

// startCleanup 至多启动一次固定清扫协程。
//
// 参数说明：无。
//
// 返回值说明：无；首次调用异步启动 cleanupLoop，后续调用立即返回。
//
// 错误情况：无；Close 与 Start 可以并发调用，sync.Once 保证 done 只由唯一协程关闭。
func (m *Manager) startCleanup() {
	m.startOnce.Do(func() {
		go m.cleanupLoop()
	})
}

// cleanupLoop 用单个 ticker 回收未连接、已空闲或超过最长寿命的会话。
//
// 参数说明：无；使用构造时规范化后的 SweepInterval。
//
// 返回值说明：无；收到 stop 后关闭 done，供 Close 等待协程收口。
//
// 错误情况：无；Forward.Close 错误不会终止清扫，其资源已经从管理映射中移除。
func (m *Manager) cleanupLoop() {
	ticker := time.NewTicker(m.options.SweepInterval)
	defer ticker.Stop()
	defer close(m.done)
	for {
		select {
		case <-m.stop:
			return
		case now := <-ticker.C:
			m.sweep(now)
		}
	}
}

// sweep 根据活动连接状态执行一次超时回收。
//
// 参数说明：now 为统一判定时间，测试可以传入固定时刻验证边界。
//
// 返回值说明：无；到期会话会从映射移除并在锁外关闭。
//
// 错误情况：底层 Close 错误被忽略，因为超时回收不能无限重试一个已失效会话；
// 关闭动作放在锁外，避免慢速资源释放阻塞列表和显式断开请求。
func (m *Manager) sweep(now time.Time) {
	m.mu.Lock()
	expired := make([]Forward, 0)
	for id, session := range m.sessions {
		active := session.forward.ActiveConnections()
		if active > 0 {
			session.hadActive = true
			session.lastActive = now
		}
		startupExpired := !session.hadActive && now.Sub(session.StartedAt) >= m.options.StartupGrace
		idleExpired := session.hadActive && active == 0 && now.Sub(session.lastActive) >= m.options.IdleTimeout
		lifetimeExpired := now.Sub(session.StartedAt) >= m.options.MaxLifetime
		if startupExpired || idleExpired || lifetimeExpired {
			expired = append(expired, session.forward)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
	for _, forward := range expired {
		_ = forward.Close()
	}
}

// findByConnectionLocked 查找指定连接档案当前唯一会话；调用方必须持有 mu。
//
// 参数说明：name 为连接档案名称。
//
// 返回值说明：*managedSession；不存在时返回 nil。
//
// 错误情况：无；名称按去除首尾空白后的精确文本匹配，保持配置标识稳定。
func (m *Manager) findByConnectionLocked(name string) *managedSession {
	name = strings.TrimSpace(name)
	for _, session := range m.sessions {
		if session.ConnectionName == name {
			return session
		}
	}
	return nil
}

// snapshotLocked 从托管会话生成不暴露 Forward 的只读快照；调用方必须持有 mu。
//
// 参数说明：session 为映射中的有效托管会话。
//
// 返回值说明：Session，ActiveConnections 在返回前从底层转发刷新。
//
// 错误情况：无；Forward 接口由构造成功的工厂保证非 nil。
func (m *Manager) snapshotLocked(session *managedSession) Session {
	snapshot := session.Session
	snapshot.ActiveConnections = session.forward.ActiveConnections()
	return snapshot
}

// newSessionID 使用密码学随机数生成不包含用户信息的会话标识。
//
// 参数说明：无。
//
// 返回值说明：string 和 error；成功返回 24 个十六进制字符。
//
// 错误情况：操作系统随机源不可用时返回错误，不创建隧道，避免可预测 ID 被管理 API 猜中。
func newSessionID() (string, error) {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成桌面会话 ID 失败: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}
