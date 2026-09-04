package app

// 本文件承载「远程桌面」模块的应用编排：连接档案事务、桌面服务端口与 remote
// 暴露列表的跨配置事务，以及临时会话的创建和回收。核心协议规则位于 desktop 包，
// tailcat token 解析和实际转发仍只通过 remote 包完成。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"proxyd/internal/config"
	"proxyd/internal/desktop"
	"proxyd/internal/remote"
)

// DesktopServiceStatus 是 Web 展示的一项本机桌面服务状态。
type DesktopServiceStatus struct {
	Protocol  string `json:"protocol"`
	Name      string `json:"name"`
	Port      int    `json:"port"`
	Default   int    `json:"default_port"`
	Listening bool   `json:"listening"`
	Exposed   bool   `json:"exposed"`
}

// DesktopStatus 是远程桌面页面的一次完整只读快照。
type DesktopStatus struct {
	Services      []DesktopServiceStatus     `json:"services"`
	Connections   []config.DesktopConnection `json:"connections"`
	Sessions      []desktop.Session          `json:"sessions"`
	RemoteEnabled bool                       `json:"remote_enabled"`
	APILoopback   bool                       `json:"api_loopback"`
}

// initDesktop 创建桌面会话管理器，并注入 remote 临时转发适配器。
//
// 参数说明：无；在 initRemote 之后由 New 调用。
//
// 返回值说明：无；管理器被保存到 App，后台只启动一个固定清扫协程。
//
// 错误情况：构造阶段不执行网络 I/O；真正的远端解析或监听错误由 StartDesktopSession 返回。
func (a *App) initDesktop() {
	a.desktop = desktop.NewManager(func(spec desktop.SessionSpec) (desktop.Forward, error) {
		a.mu.RLock()
		remotes := append([]config.RemotePeer(nil), a.cfg.Remote.Remotes...)
		a.mu.RUnlock()
		token, err := remote.ResolveToken(remotes, spec.Remote)
		if err != nil {
			return nil, err
		}
		return a.remote.StartTransientForward(token, spec.RemotePort)
	}, desktop.Options{})
}

// stopDesktop 幂等停止全部桌面会话及后台清扫协程。
//
// 参数说明：无。
//
// 返回值说明：无。
//
// 错误情况：无；底层关闭采用尽力而为策略，必须在 stopRemote 之前调用，确保临时
// tailcat 客户端仍能按正常顺序释放。
func (a *App) stopDesktop() {
	if a.desktop != nil {
		a.desktop.Close()
	}
}

// DesktopStatus 返回服务端检测、保存档案和活动会话的组合快照。
//
// 参数说明：ctx 为 HTTP 请求上下文，取消时本机端口探测尽快结束。
//
// 返回值说明：DesktopStatus；服务顺序固定为 RDP、VNC，连接档案按名称排序。
//
// 错误情况：本机端口拒绝或超时只表现为 Listening=false，不让整个页面加载失败。
func (a *App) DesktopStatus(ctx context.Context) DesktopStatus {
	a.mu.RLock()
	configuration := a.cfg.Desktop.Clone()
	remoteConfiguration := a.cfg.Remote.Clone()
	apiListen := a.cfg.APIListen
	a.mu.RUnlock()

	specs := []desktop.ServiceSpec{
		{Protocol: desktop.ProtocolRDP, Port: configuration.ServicePort(config.DesktopProtocolRDP)},
		{Protocol: desktop.ProtocolVNC, Port: configuration.ServicePort(config.DesktopProtocolVNC)},
	}
	probes := desktop.ProbeLocalServices(ctx, specs)
	services := make([]DesktopServiceStatus, 0, len(probes))
	for _, probe := range probes {
		name := "Windows / Linux RDP"
		defaultPort := config.DefaultDesktopRDPPort
		if probe.Protocol == desktop.ProtocolVNC {
			name = "macOS 屏幕共享 / VNC"
			defaultPort = config.DefaultDesktopVNCPort
		}
		services = append(services, DesktopServiceStatus{
			Protocol:  string(probe.Protocol),
			Name:      name,
			Port:      probe.Port,
			Default:   defaultPort,
			Listening: probe.Listening,
			Exposed:   containsDesktopPort(remoteConfiguration.Serve, probe.Port),
		})
	}
	connections := append([]config.DesktopConnection(nil), configuration.Connections...)
	sort.Slice(connections, func(i, j int) bool { return connections[i].Name < connections[j].Name })
	return DesktopStatus{
		Services:      services,
		Connections:   connections,
		Sessions:      a.desktop.List(),
		RemoteEnabled: remoteConfiguration.Enabled,
		APILoopback:   config.IsLoopbackAPIListen(apiListen),
	}
}

// SetDesktopService 更新实际系统服务端口及其 tailcat 暴露状态。
//
// 参数说明：protocol 为 rdp/vnc；port 为系统真实监听端口；exposed 表示是否加入 remote.serve。
//
// 返回值说明：error；桌面配置、remote 运行态与配置文件全部提交成功时返回 nil。
//
// 错误情况：协议/端口非法、remote 服务重建或落盘失败时返回错误，并恢复桌面配置、
// remote 配置及旧运行态。修改已开放服务的端口会原子地移除旧端口、加入新端口。
func (a *App) SetDesktopService(protocol string, port int, exposed bool) error {
	parsedProtocol, err := desktop.ParseProtocol(protocol)
	if err != nil {
		return err
	}
	if err := desktop.ValidateServiceSpec(desktop.ServiceSpec{Protocol: parsedProtocol, Port: port}); err != nil {
		return err
	}

	a.desktopMutationMu.Lock()
	defer a.desktopMutationMu.Unlock()
	a.remoteMutationMu.Lock()
	defer a.remoteMutationMu.Unlock()

	a.mu.Lock()
	oldDesktop := a.cfg.Desktop.Clone()
	oldRemote := a.cfg.Remote.Clone()
	nextDesktop := oldDesktop.Clone()
	nextRemote := oldRemote.Clone()
	oldPort := nextDesktop.ServicePort(string(parsedProtocol))
	if err := nextDesktop.SetServicePort(string(parsedProtocol), port); err != nil {
		a.mu.Unlock()
		return err
	}
	if oldPort != port && containsDesktopPort(nextRemote.Serve, oldPort) {
		nextRemote.Serve = removeDesktopPort(nextRemote.Serve, oldPort)
	}
	if exposed {
		nextRemote.Serve = addDesktopPort(nextRemote.Serve, port)
	} else {
		nextRemote.Serve = removeDesktopPort(nextRemote.Serve, port)
	}
	if err := nextDesktop.Validate(); err != nil {
		a.mu.Unlock()
		return err
	}
	if err := config.ValidateRemoteServe(nextRemote.Serve); err != nil {
		a.mu.Unlock()
		return err
	}
	a.cfg.Desktop = nextDesktop
	a.cfg.Remote = nextRemote
	a.mu.Unlock()

	if err := a.remote.Apply(nextRemote.Clone()); err != nil {
		return a.rollbackDesktopAndRemote(oldDesktop, oldRemote, err)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return a.rollbackDesktopAndRemote(oldDesktop, oldRemote, persistErr)
	}
	return nil
}

// AddDesktopConnection 保存一条新的客户端连接档案。
//
// 参数说明：connection 包含名称、远端引用、协议、端口及可选用户名。
//
// 返回值说明：error；校验和原子落盘成功时返回 nil。
//
// 错误情况：字段非法或名称重复时拒绝；落盘失败恢复旧内存配置。远端名称允许暂时
// 不存在，但启动会话时必须能由 remote 上下文解析。
func (a *App) AddDesktopConnection(connection config.DesktopConnection) error {
	connection = normalizeDesktopConnection(connection)
	return a.mutateDesktop(func(configuration *config.DesktopConfig) error {
		for _, existing := range configuration.Connections {
			if existing.Name == connection.Name {
				return fmt.Errorf("桌面连接 %q 已存在", connection.Name)
			}
		}
		configuration.Connections = append(configuration.Connections, connection)
		return configuration.Validate()
	})
}

// UpdateDesktopConnection 原位更新或重命名一条连接档案。
//
// 参数说明：currentName 是 URL 中的原名称；connection 是完整目标状态。
//
// 返回值说明：error；找到原档案且新状态持久化成功时返回 nil。
//
// 错误情况：原档案不存在、新名称冲突、字段非法或落盘失败时返回错误。已运行会话
// 使用启动时快照继续存在，不被档案编辑突然中断。
func (a *App) UpdateDesktopConnection(currentName string, connection config.DesktopConnection) error {
	currentName = strings.TrimSpace(currentName)
	connection = normalizeDesktopConnection(connection)
	return a.mutateDesktop(func(configuration *config.DesktopConfig) error {
		found := -1
		for index, existing := range configuration.Connections {
			if existing.Name == currentName {
				found = index
			}
			if existing.Name == connection.Name && existing.Name != currentName {
				return fmt.Errorf("桌面连接 %q 已存在", connection.Name)
			}
		}
		if found < 0 {
			return fmt.Errorf("未找到桌面连接 %q", currentName)
		}
		configuration.Connections[found] = connection
		return configuration.Validate()
	})
}

// DeleteDesktopConnection 删除一条保存的连接档案。
//
// 参数说明：name 是待删除档案名称。
//
// 返回值说明：error；成功移除并持久化时返回 nil。
//
// 错误情况：档案不存在或落盘失败时返回错误。活动会话不会被隐式中断，用户仍可在
// 当前会话列表中显式断开，或等待空闲回收。
func (a *App) DeleteDesktopConnection(name string) error {
	name = strings.TrimSpace(name)
	return a.mutateDesktop(func(configuration *config.DesktopConfig) error {
		for index, existing := range configuration.Connections {
			if existing.Name == name {
				configuration.Connections = append(configuration.Connections[:index], configuration.Connections[index+1:]...)
				return nil
			}
		}
		return fmt.Errorf("未找到桌面连接 %q", name)
	})
}

// StartDesktopSession 为已保存档案创建或复用临时 tailcat 转发。
//
// 参数说明：name 是连接档案名称。
//
// 返回值说明：desktop.Session 和 error；成功包含浏览器可交给系统客户端的本地地址。
//
// 错误情况：档案或远端不存在、token 非法、本地监听失败或会话管理器关闭时返回错误。
func (a *App) StartDesktopSession(name string) (desktop.Session, error) {
	name = strings.TrimSpace(name)
	a.mu.RLock()
	var found *config.DesktopConnection
	for _, connection := range a.cfg.Desktop.Connections {
		if connection.Name == name {
			copyOfConnection := connection
			found = &copyOfConnection
			break
		}
	}
	a.mu.RUnlock()
	if found == nil {
		return desktop.Session{}, fmt.Errorf("未找到桌面连接 %q", name)
	}
	protocol, err := desktop.ParseProtocol(found.Protocol)
	if err != nil {
		return desktop.Session{}, err
	}
	return a.desktop.Start(desktop.SessionSpec{
		ConnectionName: found.Name,
		Remote:         found.Remote,
		Protocol:       protocol,
		RemotePort:     found.RemotePort,
		Username:       found.Username,
	})
}

// StopDesktopSession 显式断开桌面会话并释放本地端口及 tailcat 客户端。
//
// 参数说明：id 为会话 ID。
//
// 返回值说明：error；会话存在且关闭完成时返回 nil。
//
// 错误情况：会话不存在时返回 desktop.ErrSessionNotFound；底层关闭错误原样返回。
func (a *App) StopDesktopSession(id string) error {
	return a.desktop.Stop(id)
}

// DesktopSession 查询单个桌面会话，供 RDP 文件下载端点读取地址与用户名。
//
// 参数说明：id 为会话 ID。
//
// 返回值说明：desktop.Session 和 error；会话存在时返回最新快照。
//
// 错误情况：会话不存在或已经超时回收时返回 desktop.ErrSessionNotFound。
func (a *App) DesktopSession(id string) (desktop.Session, error) {
	return a.desktop.Get(id)
}

// mutateDesktop 是连接档案配置变更的统一事务入口。
//
// 参数说明：mutate 只修改当前桌面配置的独立克隆。
//
// 返回值说明：error；内存配置和配置文件同时提交成功时返回 nil。
//
// 错误情况：领域校验或落盘失败时返回错误，落盘失败会恢复旧内存配置。档案变更不
// 改变运行态，因此不需要 remote 调和或补偿动作。
func (a *App) mutateDesktop(mutate func(configuration *config.DesktopConfig) error) error {
	a.desktopMutationMu.Lock()
	defer a.desktopMutationMu.Unlock()
	a.mu.Lock()
	old := a.cfg.Desktop.Clone()
	next := old.Clone()
	if err := mutate(&next); err != nil {
		a.mu.Unlock()
		return err
	}
	a.cfg.Desktop = next
	if err := a.persistLocked(); err != nil {
		a.cfg.Desktop = old
		a.mu.Unlock()
		return err
	}
	a.mu.Unlock()
	return nil
}

// rollbackDesktopAndRemote 恢复失败的跨模块服务配置事务。
//
// 参数说明：oldDesktop/oldRemote 是事务前快照；cause 是原始失败。
//
// 返回值说明：error，保留原始错误并合并可能的 remote 运行态恢复错误。
//
// 错误情况：旧 remote 重新应用也可能失败；错误通过 errors.Join 完整返回，避免掩盖
// 第一现场。调用方必须同时持有 desktopMutationMu 和 remoteMutationMu。
func (a *App) rollbackDesktopAndRemote(oldDesktop config.DesktopConfig, oldRemote config.RemoteConfig, cause error) error {
	a.mu.Lock()
	a.cfg.Desktop = oldDesktop
	a.cfg.Remote = oldRemote
	a.mu.Unlock()
	if rollbackErr := a.remote.Apply(oldRemote.Clone()); rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("恢复旧桌面服务运行态失败: %w", rollbackErr))
	}
	return cause
}

// normalizeDesktopConnection 规范化来自 API 的连接档案文本并补默认端口。
//
// 参数说明：connection 是尚未信任的输入对象。
//
// 返回值说明：config.DesktopConnection；名称、远端、协议、用户名去除首尾空白，协议
// 转为小写，零端口按协议默认值补齐。
//
// 错误情况：无；非法协议补出的端口为 0，随后由 DesktopConfig.Validate 返回定位错误。
func normalizeDesktopConnection(connection config.DesktopConnection) config.DesktopConnection {
	connection.Name = strings.TrimSpace(connection.Name)
	connection.Remote = strings.TrimSpace(connection.Remote)
	connection.Protocol = strings.ToLower(strings.TrimSpace(connection.Protocol))
	connection.Username = strings.TrimSpace(connection.Username)
	if connection.RemotePort == 0 {
		connection.RemotePort = config.DefaultDesktopPort(connection.Protocol)
	}
	return connection
}

// containsDesktopPort 判断端口列表是否精确包含目标端口。
//
// 参数说明：ports 为 remote.serve；target 为桌面服务端口。
// 返回值说明：bool，存在时为 true。
// 错误情况：无；非法端口不会被特殊处理，配置校验在事务提交前完成。
func containsDesktopPort(ports []int, target int) bool {
	for _, port := range ports {
		if port == target {
			return true
		}
	}
	return false
}

// addDesktopPort 幂等加入端口并排序，保证配置文件输出稳定。
//
// 参数说明：ports 是源列表；target 是待加入端口。
// 返回值说明：[]int，新切片不与源列表共享，结果升序且不重复。
// 错误情况：无；端口范围由调用方预先校验。
func addDesktopPort(ports []int, target int) []int {
	out := append([]int(nil), ports...)
	if !containsDesktopPort(out, target) {
		out = append(out, target)
	}
	sort.Ints(out)
	return out
}

// removeDesktopPort 从列表中移除所有目标端口并返回独立切片。
//
// 参数说明：ports 是源列表；target 是待移除端口。
// 返回值说明：[]int，保持其余元素原顺序。
// 错误情况：无；目标不存在时返回内容相同的新切片。
func removeDesktopPort(ports []int, target int) []int {
	out := make([]int, 0, len(ports))
	for _, port := range ports {
		if port != target {
			out = append(out, port)
		}
	}
	return out
}
