// Package gateway 是「LAN 网关」旁路由模块的领域层：把登记设备的下游流量
// 经平台执行层（macOS pf anchor / Linux nftables）导入 mihomo 的 redir/tproxy
// 入口，并开关内核 IPv4 转发。数据面不依赖 TUN（docs/adr/0003 勘误）。
//
// 特权模型平台分治：macOS 经 unix socket 下发白名单指令给 root helper，
// Linux 依赖 setcap 能力位（预检指引同 tunperm 风格）；Windows 不支持。
package gateway

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"proxyd/internal/config"
)

// DefaultDNSListenPort 是 mihomo dns 的非特权监听端口；下游 53 端口被
// pf/nftables redirect 到这里（helper 白名单不含绑端口操作，53 属特权端口）。
// 规范定义在 config 包（core 生成层也引用），这里仅作别名保持网关域可读性。
const DefaultDNSListenPort = config.DefaultGatewayDNSListenPort

// helperHeartbeatInterval 是主进程向 helper 发送存活信号的间隔；
// helper 看门狗超时（90s）为其三倍，容忍偶发抖动。定义为变量以便测试缩小间隔。
var helperHeartbeatInterval = 30 * time.Second

// heartbeatPinger 是支持看门狗心跳的执行层（macOS helperRunner）；
// 无心跳语义的执行层（Linux nftables 在本进程内执行）不实现该接口。
type heartbeatPinger interface {
	Ping() error
}

// Status 是网关模块的运行态快照。
type Status struct {
	Platform   string `json:"platform"`      // runtime.GOOS
	Supported  bool   `json:"supported"`     // 当前平台是否支持（Windows 恒 false）
	Enabled    bool   `json:"enabled"`       // 配置未停用（disabled=false）
	Forwarding bool   `json:"forwarding"`    // 内核 IPv4 转发已由本模块开启
	Applied    bool   `json:"applied"`       // 转发规则（pf anchor / nftables table）已应用
	DeviceNum  int    `json:"device_num"`    // 登记设备数
	Err        string `json:"err,omitempty"` // 最近一次 helper/执行层错误
}

// Manager 是网关模块的领域管理器，形状对齐 internal/remote 的 Manager：
// Apply 调和目标配置，Status 返回跨锁快照，Close 尽力清理规则。
type Manager struct {
	stateDir string
	logf     func(format string, args ...any)

	mu         sync.Mutex
	runner     Runner
	cfg        config.GatewayConfig
	enabled    bool
	applied    bool
	forwarding bool
	lastErr    string
	closed     bool
	hbCancel   context.CancelFunc // 看门狗心跳协程（仅 helper 类执行层启动）
}

// NewManager 创建网关管理器。
//
// 参数说明：
//   - stateDir: string，运行态目录（helper 状态、ARP 学习缓存等，本切片暂未落盘）。
//   - logf: func(string, ...any)，运行日志；nil 时丢弃。
//
// 返回值说明：*Manager，已按当前平台选定执行层 Runner，尚未应用任何规则。
//
// 错误情况：无；平台不支持或特权缺失在 Apply 时以错误返回。
func NewManager(stateDir string, logf func(format string, args ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{
		stateDir: stateDir,
		logf:     logf,
		runner:   newRunner(),
	}
}

// Apply 把网关配置调和到运行态：启用时生成规则文本交给执行层应用并开启
// IPv4 转发；停用时清除已应用规则。
//
// 参数说明：
//   - cfg: config.GatewayConfig，目标配置（调用方须已通过 config 校验）。
//
// 返回值说明：error，执行层应用失败、特权不足或平台不支持时返回；
// 错误同时记入 Status().Err 供控制台展示。
//
// 错误情况：部分失败（规则已应用但转发开启失败）时按整体失败处理并记录，
// 配置事务层据此回滚；重复 Apply 是幂等的（执行层整体替换规则）。
func (m *Manager) Apply(cfg config.GatewayConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("gateway 管理器已关闭")
	}
	m.cfg = cfg.Clone()
	m.enabled = !cfg.Disabled
	if cfg.Disabled {
		if m.applied {
			if err := m.runner.Clear(); err != nil {
				m.logf("[gateway] 清除转发规则失败: %v", err)
			}
			m.applied = false
		}
		m.stopHeartbeatLocked()
		m.lastErr = ""
		return nil
	}
	text := renderRules(cfg, DefaultDNSListenPort)
	if err := m.runner.Apply(text); err != nil {
		m.lastErr = err.Error()
		return err
	}
	m.applied = true
	if err := m.runner.Forwarding(true); err != nil {
		// 规则已生效但转发开关失败时不能保留半套数据面：下游报文可能被截获，
		// 却无法按预期完成路由。先清理执行层规则，再把清理失败与原错误一并暴露；
		// application 事务随后仍会按旧配置做完整回滚。
		joined := err
		if clearErr := m.runner.Clear(); clearErr != nil {
			joined = errors.Join(joined, fmt.Errorf("IPv4 转发开启失败后清理规则失败: %w", clearErr))
		}
		m.applied = false
		m.forwarding = false
		m.lastErr = joined.Error()
		return joined
	}
	m.forwarding = true
	m.lastErr = ""
	// 规则已应用后启动看门狗心跳：helper 失联超时会自动清除规则，
	// 心跳保证正常运行中的主进程不被误判。
	m.startHeartbeatLocked()
	return nil
}

// startHeartbeatLocked 在规则已应用时启动看门狗心跳协程；调用方须持有 m.mu。
// 执行层不支持心跳（非 helper 模型）时是 no-op。
func (m *Manager) startHeartbeatLocked() {
	m.stopHeartbeatLocked()
	pinger, ok := m.runner.(heartbeatPinger)
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.hbCancel = cancel
	go func() {
		ticker := time.NewTicker(helperHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := pinger.Ping(); err != nil {
					m.logf("[gateway] helper 心跳失败: %v", err)
				}
			}
		}
	}()
}

// stopHeartbeatLocked 停止看门狗心跳协程；调用方须持有 m.mu。
func (m *Manager) stopHeartbeatLocked() {
	if m.hbCancel != nil {
		m.hbCancel()
		m.hbCancel = nil
	}
}

// Status 返回网关运行态快照。
//
// 参数：无。
//
// 返回值：
//   - Status：含平台、转发状态、规则是否已应用与最近一次执行层错误。
//
// 错误情况：无；快照为值拷贝，可跨锁安全读取。
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{
		Platform:   runtime.GOOS,
		Supported:  !isUnsupported(),
		Enabled:    m.enabled,
		Forwarding: m.forwarding,
		Applied:    m.applied,
		DeviceNum:  len(m.cfg.Devices),
		Err:        m.lastErr,
	}
}

// RunnerStatus 返回执行层自述状态（macOS helper 上报的转发状态 /
// Linux 的 nft 表与 ip_forward 摘要），供状态与诊断接口展示。
//
// 参数：无。
//
// 返回值：string，可读摘要；执行层不可达时返回其错误文本（含修复指引）。
//
// 错误情况：无；执行层错误被转换为文本而非 error 返回。macOS 上该调用会
// 拨号 helper socket（短超时），helper 未安装时快速失败。
func (m *Manager) RunnerStatus() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runner.Status()
}

// Close 关闭管理器并尽力清除已应用的转发规则。
//
// 参数：无。
//
// 返回值：无；清除失败只打日志（进程退出路径无法补救）。
// IPv4 转发开关保持原状不主动关闭，避免误伤用户既有转发用途。
//
// 错误情况：重复 Close 是安全的（幂等）。
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	m.stopHeartbeatLocked()
	if m.applied {
		if err := m.runner.Clear(); err != nil {
			m.logf("[gateway] 退出时清除转发规则失败: %v", err)
		}
		m.applied = false
	}
}
