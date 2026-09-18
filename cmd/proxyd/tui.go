package main

// 本文件实现 `proxyd ls` 的交互式终端控制台。
//
// 边界约束：快照轮询只调用 Web 控制台已经使用的 GET 接口，不直接访问 domain、
// application 或基础设施对象；写操作只由显式按键触发，经 Web 控制台同源 API 执行
// （见 tui_actions.go），危险操作需要 y/n 确认，TUI 不触碰任何 domain/app 对象。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"proxyd/internal/api"
	"proxyd/internal/app"
)

const (
	// tuiRefreshInterval 是概览、连接、日志和远程状态的自动刷新周期。
	// 三秒兼顾状态时效与本机 API 开销；实时速率使用独立流，不受该周期限制。
	tuiRefreshInterval = 3 * time.Second
	// tuiRequestTimeout 限制单个快照接口等待时间，避免某个上游状态接口阻塞整个界面。
	tuiRequestTimeout = 4 * time.Second
	// tuiTrafficRetryInterval 是实时流断开后的重连退避时间，避免守护进程离线时忙循环。
	tuiTrafficRetryInterval = 2 * time.Second
)

// tuiPage 定义交互控制台的一级视图。
type tuiPage int

const (
	tuiPageOverview tuiPage = iota
	tuiPageNodes
	tuiPageSubscriptions
	tuiPagePorts
	tuiPageRules
	tuiPageConnections
	tuiPageRemote
	tuiPageGateway
	tuiPageDesktop
	tuiPageLogs
)

// tuiTabs 是视图标识与可见名称的唯一映射，数字快捷键按切片顺序生成（第 10 页用 0）。
var tuiTabs = []struct {
	Name  string
	Short string
}{
	{Name: "运行概览", Short: "概览"},
	{Name: "代理节点", Short: "节点"},
	{Name: "订阅资源", Short: "订阅"},
	{Name: "代理入口", Short: "入口"},
	{Name: "访问规则", Short: "规则"},
	{Name: "活动连接", Short: "连接"},
	{Name: "远程连接", Short: "远程"},
	{Name: "LAN 网关", Short: "网关"},
	{Name: "远程桌面", Short: "桌面"},
	{Name: "运行日志", Short: "日志"},
}

// tuiTraffic 是 `/api/traffic` 单行 NDJSON 的稳定展示子集。
type tuiTraffic struct {
	Up        int64 `json:"up"`
	Down      int64 `json:"down"`
	UpTotal   int64 `json:"upTotal"`
	DownTotal int64 `json:"downTotal"`
}

// tuiRuleURLStat 是远程规则源列表的只读传输模型。
type tuiRuleURLStat struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Count int    `json:"count"`
	Error string `json:"error,omitempty"`
	Warn  string `json:"warn,omitempty"`
}

// tuiRemoteAllow 是远程连接白名单的只读传输模型。
type tuiRemoteAllow struct {
	Name string `json:"name,omitempty"`
	Key  string `json:"key"`
}

// tuiRemoteForward 是远程连接本地转发状态的只读传输模型。
type tuiRemoteForward struct {
	Name       string `json:"name"`
	Listen     string `json:"listen"`
	Remote     string `json:"remote"`
	RemotePort int    `json:"remote_port"`
	Enabled    bool   `json:"enabled"`
	Running    bool   `json:"running"`
	Active     int64  `json:"active"`
	LastError  string `json:"last_error,omitempty"`
}

// tuiRemoteStatus 是 `/api/remote` 的安全展示模型；Token 只接收后端返回的打码摘要。
type tuiRemoteStatus struct {
	Enabled        bool               `json:"enabled"`
	Running        bool               `json:"running"`
	Error          string             `json:"error,omitempty"`
	Token          string             `json:"token,omitempty"`
	ClientKey      string             `json:"client_key,omitempty"`
	Region         string             `json:"region,omitempty"`
	Serve          []int              `json:"serve"`
	Allow          []tuiRemoteAllow   `json:"allow"`
	BuiltinSSH     bool               `json:"builtin_ssh"`
	ClientActivity map[string]int64   `json:"client_activity,omitempty"`
	Forwards       []tuiRemoteForward `json:"forwards"`
}

// tuiRemotePeer 是已保存远端的只读摘要；Token 来自后端打码列表接口。
type tuiRemotePeer struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

// tuiRemotePeersResponse 是 `/api/remote/remotes` 的顶层传输模型。
type tuiRemotePeersResponse struct {
	Remotes []tuiRemotePeer `json:"remotes"`
}

// tuiAuditEntry 是远程连接审计事件的只读传输模型；仅保留展示所需字段，
// 与 tuiRemoteStatus 一样采用本地打码模型，不 import internal/remote。
type tuiAuditEntry struct {
	Time           time.Time `json:"time"`
	Action         string    `json:"action"`
	ClientName     string    `json:"client_name,omitempty"`
	ClientKey      string    `json:"client_key,omitempty"`
	SSHKeyName     string    `json:"ssh_key_name,omitempty"`
	SSHFingerprint string    `json:"ssh_fingerprint,omitempty"`
	TargetPort     int       `json:"target_port"`
	Reason         string    `json:"reason,omitempty"`
}

// tuiRemoteAuditResponse 是 `/api/remote/audit` 的顶层传输模型。
type tuiRemoteAuditResponse struct {
	Entries []tuiAuditEntry `json:"entries"`
}

// tuiSnapshotMsg 承载一次轮询得到的各块数据。
//
// 使用指针区分“接口成功返回空集合”和“本次接口失败”：失败时模型保留上一次成功
// 快照，避免局部故障把仍有价值的数据从屏幕上清空。
type tuiSnapshotMsg struct {
	Overview    *api.Overview
	Connections *connListResponse
	Logs        *api.LogsResponse
	Remote      *tuiRemoteStatus
	Peers       *tuiRemotePeersResponse
	RuleURLs    *[]tuiRuleURLStat
	System      *app.SystemStatus
	Modules     *[]app.ModuleState
	Gateway     *app.GatewayOverview
	Desktop     *app.DesktopStatus
	Audit       *tuiRemoteAuditResponse
	Warnings    []string
	LoadedAt    time.Time
}

// tuiRefreshMsg 通知模型开始下一轮只读快照刷新。
type tuiRefreshMsg time.Time

// tuiTrafficMsg 表示实时速率流收到了一条有效记录。
type tuiTrafficMsg tuiTraffic

// tuiTrafficErrorMsg 表示实时速率流中断；界面保留最后一次速率并标记离线。
type tuiTrafficErrorMsg struct {
	Err error
}

// tuiModel 保存终端展示状态、最近一次 API 快照与动作执行状态。
//
// client 是唯一外部数据入口；除动作执行（actionBusy/confirm 等）外的字段都属于
// 可丢弃的 presentation state，不会反向写入应用配置或运行态。
type tuiModel struct {
	client      *apiClient
	page        tuiPage
	width       int
	height      int
	scroll      int
	cursor      int
	loading     bool
	showHelp    bool
	overview    *api.Overview
	connections connListResponse
	logs        api.LogsResponse
	remote      tuiRemoteStatus
	peers       tuiRemotePeersResponse
	ruleURLs    []tuiRuleURLStat
	system      *app.SystemStatus
	modules     []app.ModuleState
	gateway     *app.GatewayOverview
	desktop     *app.DesktopStatus
	audit       []tuiAuditEntry
	traffic     tuiTraffic
	trafficUp   bool
	trafficErr  string
	lastError   string
	warnings    []string
	lastUpdated time.Time
	// 动作执行状态：busy 期间忽略新动作键，结果反馈在告警行区域展示一行。
	actionBusy  bool
	actionLabel string
	actionNote  string
	actionErr   bool
	confirm     *tuiConfirm
}

// newTUIModel 创建带安全默认尺寸的交互式终端模型。
//
// 参数说明：
//   - client: *apiClient，指向运行中 proxyd 自有 API 的客户端。
//
// 返回值说明：tuiModel，初始停留在运行概览页并等待第一次窗口尺寸与数据消息。
//
// 错误情况：无；配置读取和连接错误由 cmdLS 与刷新消息分别处理。
func newTUIModel(client *apiClient) tuiModel {
	return tuiModel{
		client:  client,
		page:    tuiPageOverview,
		width:   100,
		height:  30,
		loading: true,
	}
}

// cmdLS 启动 `proxyd ls` 交互式终端控制台。
//
// 参数说明：
//   - args: []string，支持通用 `-c <配置文件>`，不接受其它位置参数。
//
// 返回值说明：error，正常退出返回 nil；配置读取或终端运行失败时返回具体错误。
//
// 错误情况：配置不存在、参数多余、终端初始化失败时返回错误；Ctrl-C 视为正常退出。
func cmdLS(args []string) error {
	cfgFile, rest, err := parseCFlag("ls", args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("用法: proxyd ls [-c 配置]")
	}
	client, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}

	program := tea.NewProgram(newTUIModel(client))
	trafficCtx, cancelTraffic := context.WithCancel(context.Background())
	defer cancelTraffic()
	go streamTUITraffic(trafficCtx, client, program)
	if _, err := program.Run(); err != nil && !errors.Is(err, tea.ErrInterrupted) {
		return fmt.Errorf("启动 TUI 失败: %w", err)
	}
	return nil
}

// Init 启动首次快照拉取与周期刷新计时器。
//
// 参数说明：无；接收者中必须包含有效 apiClient。
//
// 返回值说明：tea.Cmd，并行执行首次 GET 快照与下一次刷新计时。
//
// 错误情况：命令本身不返回错误；接口失败会转换为 tuiSnapshotMsg 中的告警。
func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(fetchTUISnapshotCmd(m.client), scheduleTUIRefresh())
}

// Update 处理尺寸、键盘、轮询结果、动作结果与实时流消息。
//
// 参数说明：
//   - message: tea.Msg，Bubble Tea 发送的终端事件或后台命令结果。
//
// 返回值说明：更新后的 tea.Model 与可选后续命令。
//
// 错误情况：无；网络错误进入可见告警状态，未知消息与按键会被安全忽略。
func (m tuiModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = max(message.Width, 40)
		m.height = max(message.Height, 14)
		m.scroll = min(m.scroll, m.maxScroll())
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(message)
	case tuiRefreshMsg:
		if m.loading {
			return m, scheduleTUIRefresh()
		}
		m.loading = true
		return m, tea.Batch(fetchTUISnapshotCmd(m.client), scheduleTUIRefresh())
	case tuiSnapshotMsg:
		m.applySnapshot(message)
		return m, nil
	case tuiActionResultMsg:
		m.actionBusy = false
		m.actionLabel = ""
		if message.Err != nil {
			m.actionNote = message.Label + "失败：" + message.Err.Error()
			m.actionErr = true
		} else {
			m.actionNote = message.Text
			m.actionErr = false
		}
		// 动作改变了服务端状态，立刻重取快照让界面反映新值。
		m.loading = true
		return m, fetchTUISnapshotCmd(m.client)
	case tuiTrafficMsg:
		m.traffic = tuiTraffic(message)
		m.trafficUp = true
		m.trafficErr = ""
		return m, nil
	case tuiTrafficErrorMsg:
		m.trafficUp = false
		if message.Err != nil {
			m.trafficErr = message.Err.Error()
		}
		return m, nil
	default:
		return m, nil
	}
}

// handleKey 把键盘输入分派为确认态处理、本地导航、滚动、刷新、帮助、退出与写动作。
//
// 参数说明：
//   - message: tea.KeyPressMsg，当前按键事件。
//
// 返回值说明：更新后的 tuiModel 与可选刷新/动作/退出命令。
//
// 错误情况：无；未识别的按键安全忽略，写动作只经 dispatchAction 走同源 API。
func (m tuiModel) handleKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	// 确认态拥有最高优先级：只允许 y 执行、n/esc 取消，其余键（含 q）全部忽略。
	if m.confirm != nil {
		return m.handleConfirmKey(key)
	}
	if key == "ctrl+c" || key == "q" {
		return m, tea.Quit
	}
	if key == "?" {
		m.showHelp = !m.showHelp
		return m, nil
	}
	if m.showHelp {
		if key == "esc" {
			m.showHelp = false
		}
		return m, nil
	}

	previousPage := m.page
	handled := true
	switch key {
	case "left", "h", "shift+tab":
		m.page = tuiPage((int(m.page) - 1 + len(tuiTabs)) % len(tuiTabs))
	case "right", "l", "tab":
		m.page = tuiPage((int(m.page) + 1) % len(tuiTabs))
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		m.page = tuiPage(int(key[0] - '1'))
	case "0":
		// 0 是第 10 页的数字快捷键（沿用 1-9 之后的常见终端习惯）。
		if len(tuiTabs) >= 10 {
			m.page = tuiPage(9)
		}
	case "up", "k":
		m.moveVertical(-1)
	case "down", "j":
		m.moveVertical(1)
	case "pgup", "ctrl+u":
		m.moveVertical(-max(1, m.bodyHeight()/2))
	case "pgdown", "ctrl+d":
		m.moveVertical(max(1, m.bodyHeight()/2))
	case "home", "g":
		if m.pageSelectable() {
			m.cursor = 0
		} else {
			m.scroll = 0
		}
	case "end", "G":
		if m.pageSelectable() {
			m.cursor = max(0, m.cursorRowCount()-1)
		} else {
			m.scroll = m.maxScroll()
		}
	case "r":
		if !m.loading {
			m.loading = true
			return m, fetchTUISnapshotCmd(m.client)
		}
	default:
		handled = false
	}
	if previousPage != m.page {
		m.scroll = 0
		m.cursor = 0
	}
	if handled {
		return m, nil
	}
	return m.dispatchAction(key)
}

// pageSelectable 报告当前页是否提供可选中行（光标导航取代逐行滚动）。
//
// 参数说明：无；读取模型中的当前页。
//
// 返回值说明：bool，概览/节点/订阅/连接/远程/桌面为 true，其余纯滚动页为 false。
//
// 错误情况：无；未知页码按纯滚动页处理。
func (m tuiModel) pageSelectable() bool {
	switch m.page {
	case tuiPageOverview, tuiPageNodes, tuiPageSubscriptions,
		tuiPageConnections, tuiPageRemote, tuiPageDesktop:
		return true
	default:
		return false
	}
}

// cursorRowCount 返回当前页可选中表的行数，用于光标 clamp 与行级动作解析。
//
// 参数说明：无；读取模型中当前页对应的最新快照数据。
//
// 返回值说明：int，无数据或纯滚动页返回 0；节点页为节点表与手动节点表的统一索引长度。
//
// 错误情况：无；概览未加载时任何页都返回 0。
func (m tuiModel) cursorRowCount() int {
	if m.overview == nil {
		return 0
	}
	switch m.page {
	case tuiPageOverview:
		return len(m.modules)
	case tuiPageNodes:
		return len(m.overview.Nodes) + len(m.overview.ManualNodes)
	case tuiPageSubscriptions:
		return len(m.overview.Subs)
	case tuiPageConnections:
		return len(m.connections.Connections)
	case tuiPageRemote:
		return len(m.remote.Forwards)
	case tuiPageDesktop:
		if m.desktop == nil {
			return 0
		}
		return len(m.desktop.Sessions)
	default:
		return 0
	}
}

// moveVertical 在可选中页移动光标、在纯滚动页滚动内容。
//
// 参数说明：
//   - delta: int，带方向的行数增量（负数向上）。
//
// 返回值说明：无；通过指针接收者原位更新 cursor 或 scroll。
//
// 错误情况：无；光标被夹紧在 [0, 行数-1]，滚动被夹紧在 [0, maxScroll]。
func (m *tuiModel) moveVertical(delta int) {
	if m.pageSelectable() {
		rows := m.cursorRowCount()
		if rows == 0 {
			return
		}
		m.cursor = min(max(0, m.cursor+delta), rows-1)
		return
	}
	m.scroll = min(max(0, m.scroll+delta), m.maxScroll())
}

// applySnapshot 将成功的数据块合并进现有模型，并保留失败块的旧快照。
//
// 参数说明：
//   - message: tuiSnapshotMsg，一次刷新产生的分块结果与告警。
//
// 返回值说明：无；通过指针接收者原位更新 presentation state。
//
// 错误情况：Overview 失败会设置全局错误；其它接口失败只进入 warnings，不清空旧数据。
func (m *tuiModel) applySnapshot(message tuiSnapshotMsg) {
	m.loading = false
	m.warnings = append([]string(nil), message.Warnings...)
	m.lastUpdated = message.LoadedAt
	if message.Overview != nil {
		m.overview = message.Overview
		m.lastError = ""
	} else if len(message.Warnings) > 0 {
		m.lastError = message.Warnings[0]
	}
	if message.Connections != nil {
		m.connections = *message.Connections
	}
	if message.Logs != nil {
		m.logs = *message.Logs
	}
	if message.Remote != nil {
		m.remote = *message.Remote
	}
	if message.Peers != nil {
		m.peers = *message.Peers
	}
	if message.RuleURLs != nil {
		m.ruleURLs = append([]tuiRuleURLStat(nil), (*message.RuleURLs)...)
	}
	if message.System != nil {
		m.system = message.System
	}
	if message.Modules != nil {
		m.modules = append([]app.ModuleState(nil), (*message.Modules)...)
	}
	if message.Gateway != nil {
		m.gateway = message.Gateway
	}
	if message.Desktop != nil {
		m.desktop = message.Desktop
	}
	if message.Audit != nil {
		m.audit = append([]tuiAuditEntry(nil), message.Audit.Entries...)
	}
	m.scroll = min(m.scroll, m.maxScroll())
	// 列表可能随快照缩短，光标始终夹紧到当前页的合法行范围。
	if rows := m.cursorRowCount(); rows > 0 {
		m.cursor = min(m.cursor, rows-1)
	} else {
		m.cursor = 0
	}
}

// fetchTUISnapshotCmd 构造一次仅包含 GET 请求的快照命令。
//
// 参数说明：
//   - client: *apiClient，运行中 proxyd 的本地 API 客户端。
//
// 返回值说明：tea.Cmd，完成后始终发送 tuiSnapshotMsg；成功块与失败告警可同时存在。
//
// 错误情况：单个接口失败不会阻断后续接口，且不会触发重试风暴；下一周期自动再试。
func fetchTUISnapshotCmd(client *apiClient) tea.Cmd {
	return func() tea.Msg {
		result := tuiSnapshotMsg{LoadedAt: time.Now()}

		var overview api.Overview
		if err := client.doTimeout(http.MethodGet, "/api/overview", nil, &overview, tuiRequestTimeout); err != nil {
			result.Warnings = append(result.Warnings, "概览加载失败: "+err.Error())
		} else {
			result.Overview = &overview
		}

		var connections connListResponse
		if err := client.doTimeout(http.MethodGet, "/api/connections", nil, &connections, tuiRequestTimeout); err != nil {
			result.Warnings = append(result.Warnings, "连接数据暂不可用: "+err.Error())
		} else {
			result.Connections = &connections
		}

		var logs api.LogsResponse
		if err := client.doTimeout(http.MethodGet, "/api/logs?tail=300", nil, &logs, tuiRequestTimeout); err != nil {
			result.Warnings = append(result.Warnings, "日志数据暂不可用: "+err.Error())
		} else {
			result.Logs = &logs
		}

		var remote tuiRemoteStatus
		if err := client.doTimeout(http.MethodGet, "/api/remote", nil, &remote, tuiRequestTimeout); err != nil {
			result.Warnings = append(result.Warnings, "远程连接状态暂不可用: "+err.Error())
		} else {
			result.Remote = &remote
		}

		var peers tuiRemotePeersResponse
		if err := client.doTimeout(http.MethodGet, "/api/remote/remotes", nil, &peers, tuiRequestTimeout); err != nil {
			result.Warnings = append(result.Warnings, "远端列表暂不可用: "+err.Error())
		} else {
			result.Peers = &peers
		}

		var ruleURLs []tuiRuleURLStat
		if err := client.doTimeout(http.MethodGet, "/api/rule-urls", nil, &ruleURLs, tuiRequestTimeout); err != nil {
			result.Warnings = append(result.Warnings, "规则源状态暂不可用: "+err.Error())
		} else {
			result.RuleURLs = &ruleURLs
		}

		var system app.SystemStatus
		if err := client.doTimeout(http.MethodGet, "/api/system/status", nil, &system, tuiRequestTimeout); err != nil {
			result.Warnings = append(result.Warnings, "系统状态暂不可用: "+err.Error())
		} else {
			result.System = &system
		}

		var modules []app.ModuleState
		if err := client.doTimeout(http.MethodGet, "/api/modules", nil, &modules, tuiRequestTimeout); err != nil {
			result.Warnings = append(result.Warnings, "模块状态暂不可用: "+err.Error())
		} else {
			result.Modules = &modules
		}

		var gateway app.GatewayOverview
		if err := client.doTimeout(http.MethodGet, "/api/gateway", nil, &gateway, tuiRequestTimeout); err != nil {
			result.Warnings = append(result.Warnings, "网关状态暂不可用: "+err.Error())
		} else {
			result.Gateway = &gateway
		}

		var desktop app.DesktopStatus
		if err := client.doTimeout(http.MethodGet, "/api/desktop", nil, &desktop, tuiRequestTimeout); err != nil {
			result.Warnings = append(result.Warnings, "桌面状态暂不可用: "+err.Error())
		} else {
			result.Desktop = &desktop
		}

		var audit tuiRemoteAuditResponse
		if err := client.doTimeout(http.MethodGet, "/api/remote/audit?tail=30", nil, &audit, tuiRequestTimeout); err != nil {
			result.Warnings = append(result.Warnings, "远程审计暂不可用: "+err.Error())
		} else {
			result.Audit = &audit
		}
		return result
	}
}

// scheduleTUIRefresh 安排下一次快照轮询。
//
// 参数说明：无。
//
// 返回值说明：tea.Cmd，在固定间隔后发送 tuiRefreshMsg。
//
// 错误情况：无；Bubble Tea 退出时未消费的计时消息会随程序生命周期释放。
func scheduleTUIRefresh() tea.Cmd {
	return tea.Tick(tuiRefreshInterval, func(now time.Time) tea.Msg {
		return tuiRefreshMsg(now)
	})
}

// streamTUITraffic 维护 `/api/traffic` 的只读 NDJSON 长连接并自动重连。
//
// 参数说明：
//   - ctx: context.Context，TUI 退出时取消所有在途网络与退避等待。
//   - client: *apiClient，提供受控 API 基址。
//   - program: *tea.Program，接收流量与断流消息的终端程序。
//
// 返回值说明：无；持续运行直到 ctx 被取消。
//
// 错误情况：HTTP、状态码、JSON 或流读取失败会发送离线消息，等待两秒后重连。
func streamTUITraffic(ctx context.Context, client *apiClient, program *tea.Program) {
	for {
		err := streamTUITrafficOnce(ctx, client, program)
		if ctx.Err() != nil {
			return
		}
		program.Send(tuiTrafficErrorMsg{Err: err})
		timer := time.NewTimer(tuiTrafficRetryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

// streamTUITrafficOnce 建立一次实时流连接并逐行投递速率消息。
//
// 参数说明：
//   - ctx: context.Context，用于取消 HTTP 请求与 Scanner 读取。
//   - client: *apiClient，提供 `/api/traffic` 基址。
//   - program: *tea.Program，接收解析后的 tuiTrafficMsg。
//
// 返回值说明：error，流关闭或数据异常时返回原因；调用方决定是否重连。
//
// 错误情况：请求构造、网络、非 2xx、超长行、读取失败均返回错误；单行非法 JSON 被跳过。
func streamTUITrafficOnce(ctx context.Context, client *apiClient, program *tea.Program) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.base+"/api/traffic", nil)
	if err != nil {
		return fmt.Errorf("构造实时流请求失败: %w", err)
	}
	// 实时流为了保留 context 取消语义直接持有请求；认证仍必须复用
	// apiClient 的统一入口，否则配置 api-secret 后只有 TUI 流量面板持续 401。
	client.applyManagementAuth(request)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("实时流连接失败: %w", err)
	}
	defer response.Body.Close()
	if err := checkAPIError(response); err != nil {
		return fmt.Errorf("实时流不可用: %w", err)
	}

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 1024), 64*1024)
	for scanner.Scan() {
		var traffic tuiTraffic
		if err := json.Unmarshal(scanner.Bytes(), &traffic); err != nil {
			continue
		}
		program.Send(tuiTrafficMsg(traffic))
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取实时流失败: %w", err)
	}
	return fmt.Errorf("实时流已结束")
}

// joinTUIWarnings 把局部接口错误压缩成适合单行通知的文本。
//
// 参数说明：
//   - warnings: []string，当前刷新周期收集的错误列表。
//   - limit: int，最多展示的 Unicode 字符近似宽度。
//
// 返回值说明：string，空列表返回空串，多条错误以中文分号连接并安全截断。
//
// 错误情况：无；非法 limit 由 truncateTUIText 的下限保护处理。
func joinTUIWarnings(warnings []string, limit int) string {
	return truncateTUIText(strings.Join(warnings, "；"), limit)
}
