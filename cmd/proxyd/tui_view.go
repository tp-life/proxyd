package main

// 本文件集中实现交互式 TUI 的视觉系统与十个数据视图。
// 所有布局都由当前终端尺寸即时计算，窄窗口会压缩列或改为纵向卡片；
// 快照渲染只读，写操作仅由显式按键经 Web 控制台同源 API 触发（见 tui_actions.go）。

import (
	"fmt"
	"image/color"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"proxyd/internal/api"
	"proxyd/internal/app"
)

// TUI 色板以深蓝黑为底，青色表示健康，琥珀色表示注意，红色表示错误。
// Lip Gloss 会根据终端能力自动降级真彩色，基础 ANSI 终端仍能保持信息层级。
var (
	tuiBackground = lipgloss.Color("#07111F")
	tuiSurface    = lipgloss.Color("#0D1B2A")
	tuiSurfaceAlt = lipgloss.Color("#10243A")
	tuiBorder     = lipgloss.Color("#25415E")
	tuiText       = lipgloss.Color("#E6EDF5")
	tuiMuted      = lipgloss.Color("#7890A8")
	tuiCyan       = lipgloss.Color("#35D0BA")
	tuiBlue       = lipgloss.Color("#61A8FF")
	tuiPurple     = lipgloss.Color("#A78BFA")
	tuiAmber      = lipgloss.Color("#F5B942")
	tuiRed        = lipgloss.Color("#FF6B7A")
)

// tuiColumn 描述自适应文本表格的一列；Width 为 0 表示占用剩余宽度。
type tuiColumn struct {
	Title string
	Width int
	Right bool
}

// tuiFact 描述概览面板中的一项标签和值。
type tuiFact struct {
	Label string
	Value string
}

// tuiStat 描述概览卡片的主指标、辅助说明与强调色。
type tuiStat struct {
	Value  string
	Detail string
	Accent color.Color
}

// View 将当前模型渲染为全屏交互终端视图。
//
// 参数说明：无；布局读取模型中的窗口尺寸、当前页、滚动位置、光标和数据快照。
//
// 返回值说明：tea.View，启用 alternate screen 并设置终端标题与背景色。
//
// 错误情况：无；数据未加载或局部失败时渲染明确空状态/告警，而不是 panic。
func (m tuiModel) View() tea.View {
	width := max(38, m.width-2)
	header := m.renderHeader(width)
	tabs := m.renderTabs(width)

	notice := ""
	if len(m.warnings) > 0 {
		notice = lipgloss.NewStyle().Foreground(tuiAmber).Render("△ "+joinTUIWarnings(m.warnings, max(12, width-3))) + "\n"
	}
	if m.actionNote != "" {
		noteColor, noteIcon := tuiCyan, "✓ "
		if m.actionErr {
			noteColor, noteIcon = tuiRed, "✗ "
		}
		notice += lipgloss.NewStyle().Foreground(noteColor).Render(noteIcon+truncateTUIText(m.actionNote, max(12, width-3))) + "\n"
	}

	bodyHeight := m.bodyHeight()
	page, cursorLine := m.renderCurrentPage(width)
	scroll := m.scroll
	switch {
	case m.showHelp:
		page = m.renderHelp(width)
		scroll = min(scroll, max(0, lipgloss.Height(page)-bodyHeight))
	case m.confirm != nil:
		page = m.renderConfirm(width, bodyHeight)
		scroll = 0
	default:
		// 可选中页让滚动自动跟随光标，保证光标行落在可见区。
		scroll = m.followCursor(cursorLine, bodyHeight)
	}
	maximum := max(0, lipgloss.Height(page)-bodyHeight)
	visible := visibleTUILines(page, scroll, bodyHeight)
	visible += strings.Repeat("\n", max(0, bodyHeight-lipgloss.Height(visible)))

	footer := m.renderFooter(width, scroll, maximum)
	content := header + "\n" + tabs + "\n" + notice + visible + "\n" + footer
	shell := lipgloss.NewStyle().
		Background(tuiBackground).
		Foreground(tuiText).
		Width(width).
		Height(max(1, m.height)).
		Render(content)

	view := tea.NewView(shell)
	view.AltScreen = true
	view.WindowTitle = "proxyd · 实时控制台"
	view.BackgroundColor = tuiBackground
	view.ForegroundColor = tuiText
	return view
}

// followCursor 计算保证光标行可见的滚动偏移。
//
// 参数说明：
//   - cursorLine: int，光标行在完整页面内容中的绝对行号（-1 表示纯滚动页）。
//   - bodyHeight: int，当前可见区域行数。
//
// 返回值说明：int，滚动偏移；光标在可见区内时保持原值，否则贴边跟随并夹紧到 maxScroll。
//
// 错误情况：无；纯滚动页原样返回当前滚动值。
func (m tuiModel) followCursor(cursorLine, bodyHeight int) int {
	scroll := m.scroll
	if cursorLine >= 0 {
		if cursorLine < scroll {
			scroll = cursorLine
		} else if bottom := scroll + bodyHeight - 1; cursorLine > bottom {
			scroll = cursorLine - bodyHeight + 1
		}
	}
	return min(scroll, m.maxScroll())
}

// bodyHeight 计算页面内容在固定头部、导航、通知、反馈和底栏之间可用的行数。
//
// 参数说明：无。
//
// 返回值说明：int，至少为 3 行，保证极小终端仍能显示关键提示。
//
// 错误情况：无；过小的终端高度通过下限保护处理。
func (m tuiModel) bodyHeight() int {
	// 页头、一级导航和底栏各占一行；告警与动作反馈存在时再各为其保留一行。
	// 这里按真实物理高度计算，确保底栏始终贴近终端底部且不会留下无意义空区。
	chromeHeight := 3
	if len(m.warnings) > 0 {
		chromeHeight++
	}
	if m.actionNote != "" {
		chromeHeight++
	}
	return max(3, m.height-chromeHeight)
}

// maxScroll 根据当前页渲染高度计算最大滚动偏移。
//
// 参数说明：无。
//
// 返回值说明：int，页面短于可见区域时返回 0。
//
// 错误情况：无；未加载数据时空状态同样可测量。
func (m tuiModel) maxScroll() int {
	width := max(38, m.width-2)
	page, _ := m.renderCurrentPage(width)
	if m.showHelp {
		page = m.renderHelp(width)
	}
	return max(0, lipgloss.Height(page)-m.bodyHeight())
}

// renderHeader 渲染品牌、在线标识、待重启徽标与最近更新时间。
//
// 参数说明：
//   - width: int，当前可用内容宽度。
//
// 返回值说明：string，单行 ANSI 样式标题；窄终端会省略次要说明。
//
// 错误情况：无；未加载概览时状态显示为“连接中”。
func (m tuiModel) renderHeader(width int) string {
	brand := lipgloss.NewStyle().Bold(true).Foreground(tuiCyan).Render("PROXYD")
	subtitle := lipgloss.NewStyle().Foreground(tuiMuted).Render(" / LIVE OBSERVATORY")
	if width < 72 {
		subtitle = ""
	}
	statusLabel := "连接中"
	statusColor := tuiAmber
	if m.overview != nil {
		statusLabel = "在线"
		statusColor = tuiCyan
	} else if m.lastError != "" {
		statusLabel = "实例离线"
		statusColor = tuiRed
	}
	status := renderTUIPill(statusLabel, statusColor)
	if m.system != nil && m.system.PendingRestart {
		status += " " + renderTUIPill("待重启", tuiAmber)
	}
	updated := ""
	if !m.lastUpdated.IsZero() && width >= 80 {
		updated = lipgloss.NewStyle().Foreground(tuiMuted).Render("更新 " + m.lastUpdated.Format("15:04:05"))
	}
	right := status
	if updated != "" {
		right += "  " + updated
	}
	gap := strings.Repeat(" ", max(1, width-lipgloss.Width(brand+subtitle)-lipgloss.Width(right)))
	return brand + subtitle + gap + right
}

// renderTabs 渲染十个一级视图及数字快捷键（第 10 页显示为 0）。
//
// 参数说明：
//   - width: int，当前可用内容宽度。
//
// 返回值说明：string，宽终端显示全部标签，窄终端只显示相邻导航与当前页。
//
// 错误情况：无；page 越界时通过取模恢复到合法索引。
func (m tuiModel) renderTabs(width int) string {
	if width < 76 {
		return m.renderCompactTabs()
	}
	parts := make([]string, 0, len(tuiTabs))
	for index, tab := range tuiTabs {
		label := fmt.Sprintf("%d %s", (index+1)%10, tab.Short)
		style := lipgloss.NewStyle().Padding(0, 1).Foreground(tuiMuted)
		if tuiPage(index) == m.page {
			style = style.Bold(true).Foreground(tuiBackground).Background(tuiBlue)
		}
		parts = append(parts, style.Render(label))
	}
	joined := strings.Join(parts, " ")
	// ANSI 样式序列不能按普通 rune 截断，否则可能把终端留在错误的颜色状态。
	// 主题或中文字体导致完整导航超宽时，直接回退到紧凑导航，保持输出序列完整。
	if lipgloss.Width(joined) > width {
		return m.renderCompactTabs()
	}
	return joined
}

// renderCompactTabs 渲染仅包含当前页与左右导航提示的窄屏导航。
//
// 参数说明：无；读取模型中的当前页。
//
// 返回值说明：string，单行 ANSI 样式紧凑导航。
//
// 错误情况：无；非法页码通过取模映射回可用标签范围。
func (m tuiModel) renderCompactTabs() string {
	current := int(m.page) % len(tuiTabs)
	label := fmt.Sprintf("‹ h  %d %s  l ›", current+1, tuiTabs[current].Name)
	return lipgloss.NewStyle().Bold(true).Foreground(tuiBlue).Render(label)
}

// renderFooter 渲染按页上下文快捷键提示、动作状态与滚动进度。
//
// 参数说明：
//   - width: int，当前可用内容宽度。
//   - scroll: int，实际生效的滚动偏移（可选中页为跟随光标后的值）。
//   - maximum: int，当前页最大滚动偏移。
//
// 返回值说明：string，适配宽度的单行底栏。
//
// 错误情况：无；页面不可滚动且无动作时不显示右侧状态。
func (m tuiModel) renderFooter(width, scroll, maximum int) string {
	leftText := m.footerHints(width)
	rightText := ""
	switch {
	case m.actionBusy:
		rightText = "执行中: " + m.actionLabel
	case m.loading:
		rightText = "刷新中…"
	case maximum > 0:
		rightText = fmt.Sprintf("%d%%", min(100, scroll*100/maximum))
	}
	// 最窄支持宽度下也为右侧状态预留完整空间；只截断无状态的快捷键提示，
	// 防止超宽底栏让终端自动换行并覆盖上一行内容。
	leftText = truncateTUIText(leftText, max(1, width-lipgloss.Width(rightText)-1))
	left := lipgloss.NewStyle().Foreground(tuiMuted).Render(leftText)
	right := lipgloss.NewStyle().Foreground(tuiCyan).Render(rightText)
	gap := strings.Repeat(" ", max(1, width-lipgloss.Width(left)-lipgloss.Width(right)))
	return left + gap + right
}

// footerHints 返回按当前页上下文变化的底栏快捷键提示。
//
// 参数说明：
//   - width: int，当前可用内容宽度；窄终端返回精简版。
//
// 返回值说明：string，导航 + 页级行操作 + 全局操作的单行提示。
//
// 错误情况：无；纯滚动页没有行操作时只保留导航与全局操作。
func (m tuiModel) footerHints(width int) string {
	move := "j/k 滚动"
	if m.pageSelectable() {
		move = "j/k 选择"
	}
	row := ""
	switch m.page {
	case tuiPageOverview:
		row = "enter 重试 space 启停  "
	case tuiPageNodes:
		row = "enter 出口 a AUTO d 删手动  "
	case tuiPageSubscriptions:
		row = "enter 刷新 e 启停  "
	case tuiPageConnections:
		row = "x 关闭 X 全关  "
	case tuiPageRemote:
		row = "space 启停转发  "
	case tuiPageGateway:
		row = "e 开关网关  "
	case tuiPageDesktop:
		row = "x 关闭会话  "
	}
	if width < 72 {
		if row != "" {
			row = strings.TrimRight(row, " ") + "  "
		}
		return "h/l 页  " + row + "m/s/u/R/t  ? 帮助  q 退出"
	}
	return "h/l 切页  " + move + "  " + row + "m 模式  s 系代  u TUN  R 刷新  t 测速  ? 帮助  q 退出"
}

// renderCurrentPage 根据当前导航状态分派到对应的页面渲染器。
//
// 参数说明：
//   - width: int，页面可用宽度。
//
// 返回值说明：
//   - string：完整但尚未按滚动位置裁切的页面内容。
//   - int：光标行在完整内容中的绝对行号；纯滚动页与空状态返回 -1。
//
// 错误情况：无；未知页码回退到运行概览。
func (m tuiModel) renderCurrentPage(width int) (string, int) {
	if m.overview == nil {
		return m.renderEmptyState(width), -1
	}
	switch m.page {
	case tuiPageNodes:
		return m.renderNodesPage(width)
	case tuiPageSubscriptions:
		return m.renderSubscriptionsPage(width)
	case tuiPagePorts:
		return m.renderPortsPage(width), -1
	case tuiPageRules:
		return m.renderRulesPage(width), -1
	case tuiPageConnections:
		return m.renderConnectionsPage(width)
	case tuiPageRemote:
		return m.renderRemotePage(width)
	case tuiPageGateway:
		return m.renderGatewayPage(width), -1
	case tuiPageDesktop:
		return m.renderDesktopPage(width)
	case tuiPageLogs:
		return m.renderLogsPage(width), -1
	default:
		return m.renderOverviewPage(width)
	}
}

// renderEmptyState 在首次连接或实例离线时提供可执行的诊断提示。
//
// 参数说明：
//   - width: int，页面可用宽度。
//
// 返回值说明：string，包含连接状态与启动命令的面板。
//
// 错误情况：无；原始网络错误会截断展示，避免破坏布局。
func (m tuiModel) renderEmptyState(width int) string {
	title := "正在连接 proxyd"
	detail := "读取 Web 控制台使用的本地 API"
	body := "请确认守护进程已经启动：\n\n  proxyd start\n\nTUI 会自动重试，无需退出。"
	if m.lastError != "" {
		title = "暂时无法读取运行实例"
		detail = truncateTUIText(m.lastError, max(20, width-10))
	}
	return renderTUIPanel(width, title, detail, body)
}

// renderOverviewPage 渲染运行摘要、有效路由、系统开关、版本状态与模块状态表。
//
// 参数说明：
//   - width: int，页面可用宽度。
//
// 返回值说明：
//   - string：由自适应统计卡与详情面板组成的内容。
//   - int：光标行（模块状态表）在完整内容中的绝对行号；无模块数据时返回 -1。
//
// 错误情况：无；节点、版本或系统字段缺失时使用保守占位文本。
func (m tuiModel) renderOverviewPage(width int) (string, int) {
	overview := m.overview
	alive := countTUIAliveNodes(overview.Nodes)
	ready := alive > 0 && overview.MixedPort > 0
	status := "需要检查"
	statusDetail := "当前没有健康出口"
	statusColor := tuiAmber
	switch {
	case overview.Testing:
		status = "测速中"
		statusDetail = "节点健康检测进行中"
		statusColor = tuiBlue
	case ready:
		status = "代理已就绪"
		statusDetail = fmt.Sprintf("主入口 127.0.0.1:%d", overview.MixedPort)
		statusColor = tuiCyan
	}
	trafficValue := "等待数据"
	trafficDetail := "实时流正在连接"
	if m.trafficUp {
		trafficValue = "↑ " + formatBytes(m.traffic.Up) + "/s"
		trafficDetail = "↓ " + formatBytes(m.traffic.Down) + "/s"
	} else if m.trafficErr != "" {
		trafficDetail = "实时流暂不可用"
	}
	cards := []tuiStat{
		{Value: status, Detail: statusDetail, Accent: statusColor},
		{Value: trafficValue, Detail: trafficDetail, Accent: tuiBlue},
		{Value: fmt.Sprintf("%d / %d 可用", alive, len(overview.Nodes)), Detail: "代理节点", Accent: tuiPurple},
		{Value: fmt.Sprintf("%d 条连接", len(m.connections.Connections)), Detail: "内存 " + formatBytes(int64(m.connections.Memory)), Accent: tuiAmber},
	}
	cardGrid := layoutTUIStatCards(width, cards)

	policy, exit := resolveTUIMainRoute(overview)
	route := lipgloss.NewStyle().Bold(true).Foreground(tuiBlue).Render("本机应用") +
		lipgloss.NewStyle().Foreground(tuiMuted).Render("  →  ") +
		lipgloss.NewStyle().Bold(true).Foreground(tuiText).Render(fmt.Sprintf("127.0.0.1:%d", overview.MixedPort)) +
		lipgloss.NewStyle().Foreground(tuiMuted).Render("  →  ") +
		lipgloss.NewStyle().Bold(true).Foreground(tuiPurple).Render(policy) +
		lipgloss.NewStyle().Foreground(tuiMuted).Render("  →  ") +
		lipgloss.NewStyle().Bold(true).Foreground(tuiCyan).Render(exit)
	routePanel := renderTUIPanel(width, "当前有效路由", "展示主入口的真实策略优先级", lipgloss.Wrap(route, max(20, width-6), ""))

	version := overview.Version.Current
	if version == "" {
		version = "dev"
	}
	if overview.Version.Latest != "" && overview.Version.State == "available" {
		version += " → " + overview.Version.Latest + " 可更新"
	}
	if overview.Version.Message != "" {
		version += " · " + overview.Version.Message
	}
	facts := []tuiFact{
		{Label: "运行模式", Value: tuiModeLabel(overview.Mode)},
		{Label: "系统代理", Value: tuiOnOff(overview.SystemProxy)},
		{Label: "TUN", Value: tuiTUNState(overview.TUN)},
		{Label: "DNS", Value: tuiDNSLabel(overview.DNSPreset, overview.DNSCustom)},
		{Label: "节点端口映射", Value: tuiOnOff(overview.PortMappingEnabled)},
		{Label: "自动选优端口", Value: tuiPortOrOff(overview.AutoPort)},
		{Label: "系统自启", Value: tuiAutostartState(overview)},
		{Label: "版本", Value: version},
		{Label: "运行时长", Value: formatTUIUptime(m.system)},
	}
	statusPanel := renderTUIPanel(width, "系统状态", "只读快照 · 行操作见底栏", renderTUIFacts(tuiPanelBodyWidth(width), facts))

	prefix := cardGrid + "\n" + routePanel + "\n" + statusPanel + "\n"
	moduleRows := make([][]string, 0, len(m.modules))
	for _, module := range m.modules {
		phase := defaultTUIText(module.Phase, "—")
		if module.Running {
			phase += " · 运行中"
		}
		nextRetry := "—"
		if module.NextRetryAt != nil {
			nextRetry = module.NextRetryAt.Local().Format("15:04:05")
		}
		moduleRows = append(moduleRows, []string{
			module.Name, tuiOnOff(module.Enabled), phase, defaultTUIText(module.Error, "—"), nextRetry,
		})
	}
	moduleColumns := []tuiColumn{
		{Title: "模块", Width: 16},
		{Title: "启用", Width: 6},
		{Title: "相位", Width: 12},
		{Title: "错误", Width: 0},
		{Title: "下次重试", Width: 10},
	}
	highlight := -1
	if m.cursor < len(moduleRows) {
		highlight = m.cursor
	}
	modulePanel := renderTUIPanel(width, "模块状态", fmt.Sprintf("%d 个模块 · enter 重试 space 启停", len(moduleRows)),
		renderTUITableHighlight(moduleColumns, moduleRows, tuiPanelBodyWidth(width), highlight))
	return prefix + modulePanel, tuiTableCursorLine(prefix, highlight)
}

// renderNodesPage 渲染按健康状态和延迟排序的节点表与手动节点表。
//
// 参数说明：
//   - width: int，页面可用宽度。
//
// 返回值说明：
//   - string：节点统计摘要与两张表；手动节点 URL 只显示打码摘要。
//   - int：光标行的绝对行号；光标统一索引 0..len(nodes)-1 为节点表，之后映射手动节点表。
//
// 错误情况：无；空节点集合显示明确空状态，失败原因仅在离线节点上展示。
func (m tuiModel) renderNodesPage(width int) (string, int) {
	nodes := sortedTUINodes(m.overview.Nodes)
	rows := make([][]string, 0, len(nodes))
	for _, node := range nodes {
		state := "在线"
		if !node.Alive {
			state = "离线"
		}
		detail := node.Type
		if !node.Alive && node.FailReason != "" {
			detail = node.FailReason
		}
		if node.Tunnel {
			detail += "·隧道"
		}
		rows = append(rows, []string{
			state,
			node.Name,
			node.Subscription,
			detail,
			formatTUIDelay(node.Delay, node.Alive),
			tuiPortOrDash(node.Port),
		})
	}
	columns := []tuiColumn{
		{Title: "状态", Width: 6},
		{Title: "节点", Width: 0},
		{Title: "来源", Width: 16},
		{Title: "协议 / 原因", Width: 16},
		{Title: "延迟", Width: 9, Right: true},
		{Title: "端口", Width: 7, Right: true},
	}
	summary := fmt.Sprintf("%d 个健康 · %d 个异常 · 映射区间 %d–%d",
		countTUIAliveNodes(nodes), len(nodes)-countTUIAliveNodes(nodes), m.overview.PortRange[0], m.overview.PortRange[1])
	if m.overview.Testing {
		summary = "测速中…"
	}
	nodeHighlight := -1
	if m.cursor < len(rows) {
		nodeHighlight = m.cursor
	}
	prefix := renderTUIPanel(width, "代理节点", summary, renderTUITableHighlight(columns, rows, tuiPanelBodyWidth(width), nodeHighlight)) + "\n"

	manualRows := make([][]string, 0, len(m.overview.ManualNodes))
	for _, manual := range m.overview.ManualNodes {
		entryType := manual.Type
		source := "结构化出站"
		if manual.URL != "" {
			entryType = defaultTUIText(entryType, "url")
			source = maskTUISourceURL(manual.URL)
		}
		manualRows = append(manualRows, []string{defaultTUIText(manual.Name, "—"), entryType, source})
	}
	manualColumns := []tuiColumn{
		{Title: "名称", Width: 24},
		{Title: "类型", Width: 12},
		{Title: "URL 摘要", Width: 0},
	}
	manualHighlight := -1
	if index := m.cursor - len(rows); index >= 0 && index < len(manualRows) {
		manualHighlight = index
	}
	manualPanel := renderTUIPanel(width, "手动节点", fmt.Sprintf("%d 条 · 地址凭据已隐藏 · d 删除", len(manualRows)),
		renderTUITableHighlight(manualColumns, manualRows, tuiPanelBodyWidth(width), manualHighlight))

	cursorLine := tuiTableCursorLine("", nodeHighlight)
	if nodeHighlight < 0 {
		cursorLine = tuiTableCursorLine(prefix, manualHighlight)
	}
	return prefix + manualPanel, cursorLine
}

// sortedTUINodes 返回节点页渲染与行级动作共用的稳定排序副本。
//
// 参数说明：
//   - nodes: []api.NodeEntry，概览快照中的原始节点顺序。
//
// 返回值说明：[]api.NodeEntry，健康优先、按延迟升序的新切片；未测出延迟排在最后。
//
// 错误情况：无；nil 输入返回空切片，调用方无需判空。
func sortedTUINodes(nodes []api.NodeEntry) []api.NodeEntry {
	sorted := append([]api.NodeEntry(nil), nodes...)
	sort.SliceStable(sorted, func(left, right int) bool {
		if sorted[left].Alive != sorted[right].Alive {
			return sorted[left].Alive
		}
		leftDelay := sorted[left].Delay
		rightDelay := sorted[right].Delay
		if leftDelay == 0 {
			leftDelay = ^uint16(0)
		}
		if rightDelay == 0 {
			rightDelay = ^uint16(0)
		}
		return leftDelay < rightDelay
	})
	return sorted
}

// renderSubscriptionsPage 渲染订阅状态、启停、端口映射、健康度、流量额度与到期时间。
//
// 参数说明：
//   - width: int，页面可用宽度。
//
// 返回值说明：
//   - string：完整订阅表；URL 只显示 host 摘要以避免泄露 token。
//   - int：光标行（订阅表）在完整内容中的绝对行号；无订阅时返回 -1。
//
// 错误情况：无；服务端未提供用量信息时显示破折号。
func (m tuiModel) renderSubscriptionsPage(width int) (string, int) {
	rows := make([][]string, 0, len(m.overview.Subs))
	for _, subscription := range m.overview.Subs {
		used := "—"
		expires := "—"
		if subscription.UserInfo != nil {
			usedBytes := subscription.UserInfo.Upload + subscription.UserInfo.Download
			if subscription.UserInfo.Total > 0 {
				used = fmt.Sprintf("%s / %s", formatBytes(usedBytes), formatBytes(subscription.UserInfo.Total))
			} else if usedBytes > 0 {
				used = formatBytes(usedBytes)
			}
			if subscription.UserInfo.Expire > 0 {
				expires = time.Unix(subscription.UserInfo.Expire, 0).Format("2006-01-02")
			}
		}
		rows = append(rows, []string{
			subscription.Name,
			tuiSubscriptionState(subscription.State),
			tuiOnOff(subscription.Enabled),
			tuiOnOff(subscription.PortMapping),
			strings.ToUpper(subscription.Type),
			fmt.Sprintf("%d / %d", subscription.Alive, subscription.Total),
			used,
			expires,
			maskTUISourceURL(subscription.URL),
		})
	}
	columns := []tuiColumn{
		{Title: "订阅", Width: 18},
		{Title: "状态", Width: 9},
		{Title: "启用", Width: 6},
		{Title: "端口映射", Width: 8},
		{Title: "格式", Width: 7},
		{Title: "可用", Width: 8, Right: true},
		{Title: "流量", Width: 19, Right: true},
		{Title: "到期", Width: 11},
		{Title: "来源", Width: 0},
	}
	highlight := -1
	if m.cursor < len(rows) {
		highlight = m.cursor
	}
	panel := renderTUIPanel(width, "订阅资源", fmt.Sprintf("%d 个来源 · 地址凭据已隐藏", len(rows)),
		renderTUITableHighlight(columns, rows, tuiPanelBodyWidth(width), highlight))
	return panel, tuiTableCursorLine("", highlight)
}

// renderPortsPage 渲染主入口、自动选优端口、节点映射和策略分组。
//
// 参数说明：
//   - width: int，页面可用宽度。
//
// 返回值说明：string，入口概览和两张明细表。
//
// 错误情况：无；端口映射关闭时展示稳定分配快照并明确标记“未监听”。
func (m tuiModel) renderPortsPage(width int) string {
	policy, _ := resolveTUIMainRoute(m.overview)
	facts := []tuiFact{
		{Label: "主代理入口", Value: fmt.Sprintf("%s:%d", m.overview.Listen, m.overview.MixedPort)},
		{Label: "主入口策略", Value: policy},
		{Label: "自动选优", Value: tuiPortOrOff(m.overview.AutoPort)},
		{Label: "节点映射", Value: tuiOnOff(m.overview.PortMappingEnabled)},
	}
	entryPanel := renderTUIPanel(width, "代理入口", "所有地址均为 HTTP + SOCKS5 混合监听", renderTUIFacts(tuiPanelBodyWidth(width), facts))

	portEntries := m.overview.Ports
	portState := "正在监听"
	if !m.overview.PortMappingEnabled {
		portEntries = m.overview.PortAssignments
		portState = "稳定分配已保留 · 当前未监听"
	}
	portRows := make([][]string, 0, len(portEntries))
	for _, entry := range portEntries {
		state := "健康"
		if !entry.Alive {
			state = "不可用"
		}
		if !m.overview.PortMappingEnabled {
			state = "未监听"
		}
		portRows = append(portRows, []string{
			strconv.Itoa(entry.Port), entry.Node, entry.Subscription, formatTUIDelay(entry.Delay, entry.Alive), state,
		})
	}
	portColumns := []tuiColumn{
		{Title: "端口", Width: 8, Right: true},
		{Title: "节点", Width: 0},
		{Title: "来源", Width: 18},
		{Title: "延迟", Width: 9, Right: true},
		{Title: "监听", Width: 9},
	}
	portPanel := renderTUIPanel(width, "节点专属端口", portState, renderTUITable(portColumns, portRows, tuiPanelBodyWidth(width)))

	groupRows := make([][]string, 0, len(m.overview.Groups))
	for _, group := range m.overview.Groups {
		members := fmt.Sprintf("%d 个固定节点", len(group.Nodes))
		if group.Subscription != "" {
			members = "订阅 " + group.Subscription
		}
		groupRows = append(groupRows, []string{group.Name, strconv.Itoa(group.Port), tuiGroupType(group.Type), members, defaultTUIText(group.Selected, "—")})
	}
	groupColumns := []tuiColumn{
		{Title: "策略分组", Width: 0},
		{Title: "端口", Width: 8, Right: true},
		{Title: "类型", Width: 16},
		{Title: "成员来源", Width: 24},
		{Title: "选中出口", Width: 18},
	}
	groupPanel := renderTUIPanel(width, "策略分组入口", fmt.Sprintf("%d 个独立监听", len(groupRows)), renderTUITable(groupColumns, groupRows, tuiPanelBodyWidth(width)))
	return strings.Join([]string{entryPanel, portPanel, groupPanel}, "\n")
}

// renderRulesPage 渲染自定义规则顺序及远程规则源状态。
//
// 参数说明：
//   - width: int，页面可用宽度。
//
// 返回值说明：string，两张只读规则表；顺序与实际匹配优先级一致。
//
// 错误情况：无；规则源地址只显示安全摘要，拉取错误以状态列呈现。
func (m tuiModel) renderRulesPage(width int) string {
	ruleRows := make([][]string, 0, len(m.overview.CustomRules))
	for index, rule := range m.overview.CustomRules {
		ruleRows = append(ruleRows, []string{strconv.Itoa(index + 1), rule})
	}
	ruleColumns := []tuiColumn{{Title: "优先级", Width: 8, Right: true}, {Title: "自定义规则", Width: 0}}
	rulesPanel := renderTUIPanel(width, "自定义访问规则", "从上到下匹配 · 首条命中后停止", renderTUITable(ruleColumns, ruleRows, tuiPanelBodyWidth(width)))

	sourceRows := make([][]string, 0, len(m.ruleURLs))
	for _, source := range m.ruleURLs {
		state := "正常"
		if source.Error != "" {
			state = "错误: " + source.Error
		} else if source.Warn != "" {
			state = "缓存: " + source.Warn
		}
		sourceRows = append(sourceRows, []string{source.Name, strconv.Itoa(source.Count), state, maskTUISourceURL(source.URL)})
	}
	sourceColumns := []tuiColumn{
		{Title: "远程规则源", Width: 18},
		{Title: "规则数", Width: 8, Right: true},
		{Title: "状态", Width: 0},
		{Title: "来源", Width: 24},
	}
	sourcesPanel := renderTUIPanel(width, "远程规则源", fmt.Sprintf("%d 个来源", len(sourceRows)), renderTUITable(sourceColumns, sourceRows, tuiPanelBodyWidth(width)))
	return rulesPanel + "\n" + sourcesPanel
}

// renderConnectionsPage 渲染连接总量、累计流量、内存与活动连接明细。
//
// 参数说明：
//   - width: int，页面可用宽度。
//
// 返回值说明：
//   - string：连接统计卡与按当前流量降序排列的表格。
//   - int：光标行（连接表）在完整内容中的绝对行号；无连接时返回 -1。
//
// 错误情况：无；目标域名缺失时回退目的 IP，开始时间非法时显示破折号。
func (m tuiModel) renderConnectionsPage(width int) (string, int) {
	connections := sortedTUIConnections(m.connections.Connections)
	cards := []tuiStat{
		{Value: fmt.Sprintf("%d 条", len(connections)), Detail: "活动连接", Accent: tuiCyan},
		{Value: "↑ " + formatBytes(m.connections.UploadTotal), Detail: "累计上传", Accent: tuiBlue},
		{Value: "↓ " + formatBytes(m.connections.DownloadTotal), Detail: "累计下载", Accent: tuiPurple},
		{Value: formatBytes(int64(m.connections.Memory)), Detail: "内核内存", Accent: tuiAmber},
	}

	rows := make([][]string, 0, len(connections))
	for _, connection := range connections {
		chain := "—"
		if len(connection.Chains) > 0 {
			chain = connection.Chains[0]
		}
		source := connection.Metadata.SourceIP
		if connection.Metadata.SourcePort != "" {
			source += ":" + connection.Metadata.SourcePort
		}
		rows = append(rows, []string{
			shortID(connection.ID),
			connectionDestination(connection),
			source,
			chain,
			connection.Rule,
			formatBytes(connection.Upload) + " / " + formatBytes(connection.Download),
			connAge(connection.Start),
		})
	}
	columns := []tuiColumn{
		{Title: "ID", Width: 9},
		{Title: "目标", Width: 0},
		{Title: "来源", Width: 20},
		{Title: "出口", Width: 16},
		{Title: "规则", Width: 14},
		{Title: "↑ / ↓", Width: 19, Right: true},
		{Title: "存活", Width: 8, Right: true},
	}
	highlight := -1
	if m.cursor < len(rows) {
		highlight = m.cursor
	}
	prefix := layoutTUIStatCards(width, cards)
	detail := renderTUITableHighlight(columns, rows, tuiPanelBodyWidth(width), highlight)
	panel := renderTUIPanel(width, "活动连接", "按当前累计流量排序 · x 关闭选中 X 关闭全部", detail)
	return prefix + panel, tuiTableCursorLine(prefix, highlight)
}

// sortedTUIConnections 返回连接页渲染与关闭动作共用的稳定排序副本。
//
// 参数说明：
//   - connections: []connEntry，连接快照中的原始顺序。
//
// 返回值说明：[]connEntry，按当前累计流量（上传+下载）降序的新切片。
//
// 错误情况：无；nil 输入返回空切片，调用方无需判空。
func sortedTUIConnections(connections []connEntry) []connEntry {
	sorted := append([]connEntry(nil), connections...)
	sort.SliceStable(sorted, func(left, right int) bool {
		return sorted[left].Upload+sorted[left].Download > sorted[right].Upload+sorted[right].Download
	})
	return sorted
}

// renderRemotePage 渲染 tailcat 服务端、已保存远端、本地转发状态与审计事件。
//
// 参数说明：
//   - width: int，页面可用宽度。
//
// 返回值说明：
//   - string：远程模块统计、服务状态和三张明细表。
//   - int：光标行（本地转发表）在完整内容中的绝对行号；无转发时返回 -1。
//
// 错误情况：无；连接 token 始终使用 API 返回的打码摘要，不请求完整凭据端点。
func (m tuiModel) renderRemotePage(width int) (string, int) {
	service := "已关闭"
	serviceColor := tuiMuted
	if m.remote.Running {
		service = "运行中"
		serviceColor = tuiCyan
	} else if m.remote.Enabled {
		service = "启动异常"
		serviceColor = tuiRed
	}
	activeForwards := int64(0)
	runningForwards := 0
	for _, forward := range m.remote.Forwards {
		activeForwards += forward.Active
		if forward.Running {
			runningForwards++
		}
	}
	cards := []tuiStat{
		{Value: service, Detail: "隧道服务", Accent: serviceColor},
		{Value: fmt.Sprintf("%d 个", len(m.remote.Serve)), Detail: "暴露端口", Accent: tuiBlue},
		{Value: fmt.Sprintf("%d / %d", runningForwards, len(m.remote.Forwards)), Detail: "运行中转发", Accent: tuiPurple},
		{Value: fmt.Sprintf("%d 条", activeForwards), Detail: "转发活动连接", Accent: tuiAmber},
	}
	region := m.remote.Region
	if region == "" {
		region = "自动就近"
	}
	servePorts := "—"
	if len(m.remote.Serve) > 0 {
		values := make([]string, 0, len(m.remote.Serve))
		for _, port := range m.remote.Serve {
			values = append(values, strconv.Itoa(port))
		}
		servePorts = strings.Join(values, ", ")
	}
	facts := []tuiFact{
		{Label: "服务状态", Value: service},
		{Label: "DERP 区域", Value: region},
		{Label: "暴露端口", Value: servePorts},
		{Label: "内嵌 SSH", Value: tuiOnOff(m.remote.BuiltinSSH)},
		{Label: "白名单", Value: fmt.Sprintf("%d 个身份", len(m.remote.Allow))},
		{Label: "本机 Token", Value: defaultTUIText(m.remote.Token, "—")},
	}
	if m.remote.Error != "" {
		facts = append(facts, tuiFact{Label: "最近错误", Value: m.remote.Error})
	}
	statusPanel := renderTUIPanel(width, "远程服务", "独立于代理数据面 · 敏感凭据仅显示摘要", renderTUIFacts(tuiPanelBodyWidth(width), facts))

	peerRows := make([][]string, 0, len(m.peers.Remotes))
	for _, peer := range m.peers.Remotes {
		peerRows = append(peerRows, []string{peer.Name, peer.Token})
	}
	peerPanel := renderTUIPanel(width, "已保存远端", fmt.Sprintf("%d 台设备", len(peerRows)), renderTUITable(
		[]tuiColumn{{Title: "名称", Width: 24}, {Title: "Token 摘要", Width: 0}}, peerRows, tuiPanelBodyWidth(width)))

	forwardRows := make([][]string, 0, len(m.remote.Forwards))
	for _, forward := range m.remote.Forwards {
		state := "已停用"
		if forward.Running {
			state = "运行中"
		} else if forward.Enabled {
			state = "异常"
		}
		remoteTarget := maskTUIRemoteTarget(forward.Remote) + ":" + strconv.Itoa(forward.RemotePort)
		lastError := defaultTUIText(forward.LastError, "—")
		forwardRows = append(forwardRows, []string{
			forward.Name, forward.Listen, remoteTarget, state, strconv.FormatInt(forward.Active, 10), lastError,
		})
	}
	forwardColumns := []tuiColumn{
		{Title: "转发", Width: 16},
		{Title: "本地监听", Width: 20},
		{Title: "远端", Width: 0},
		{Title: "状态", Width: 8},
		{Title: "活动", Width: 6, Right: true},
		{Title: "最近错误", Width: 20},
	}
	highlight := -1
	if m.cursor < len(forwardRows) {
		highlight = m.cursor
	}
	prefix := layoutTUIStatCards(width, cards) + statusPanel + "\n" + peerPanel + "\n"
	forwardPanel := renderTUIPanel(width, "本地转发", fmt.Sprintf("%d 条配置 · space 启停", len(forwardRows)),
		renderTUITableHighlight(forwardColumns, forwardRows, tuiPanelBodyWidth(width), highlight))

	auditRows := make([][]string, 0, len(m.audit))
	for _, entry := range m.audit {
		port := "—"
		if entry.TargetPort > 0 {
			port = strconv.Itoa(entry.TargetPort)
		}
		auditRows = append(auditRows, []string{
			entry.Time.Local().Format("15:04:05"), entry.Action, tuiAuditPeer(entry), port, defaultTUIText(entry.Reason, "—"),
		})
	}
	auditColumns := []tuiColumn{
		{Title: "时间", Width: 9},
		{Title: "动作", Width: 10},
		{Title: "对端", Width: 0},
		{Title: "端口", Width: 7, Right: true},
		{Title: "原因", Width: 18},
	}
	auditPanel := renderTUIPanel(width, "审计事件", fmt.Sprintf("最近 %d 条", len(auditRows)), renderTUITable(auditColumns, auditRows, tuiPanelBodyWidth(width)))
	return prefix + forwardPanel + "\n" + auditPanel, tuiTableCursorLine(prefix, highlight)
}

// renderGatewayPage 渲染 LAN 网关的运行状态与登记设备表。
//
// 参数说明：
//   - width: int，页面可用宽度。
//
// 返回值说明：string，状态 facts 与设备表；本页为纯滚动页，光标行恒为 -1。
//
// 错误情况：无；状态未加载或平台不支持时显示 precheck 风格的说明面板。
func (m tuiModel) renderGatewayPage(width int) string {
	gateway := m.gateway
	if gateway == nil {
		return renderTUIPanel(width, "LAN 网关", "状态未加载", "网关状态暂不可用，等待下一次快照刷新。")
	}
	if !gateway.Supported {
		return renderTUIPanel(width, "LAN 网关", "当前平台不支持",
			fmt.Sprintf("平台 %s 不支持 LAN 网关旁路由（仅 macOS / Linux）。\n网关页级开关键（e）在此平台上不可用。", gateway.Platform))
	}
	dnsListen := tuiOnOff(gateway.DNSRedirect)
	if gateway.DNSRedirect && gateway.DNSListenPort > 0 {
		dnsListen += " · 端口 " + strconv.Itoa(gateway.DNSListenPort)
	}
	facts := []tuiFact{
		{Label: "平台", Value: gateway.Platform},
		{Label: "模块开关", Value: tuiOnOff(gateway.Enabled)},
		{Label: "相位", Value: tuiGatewayPhase(*gateway)},
		{Label: "内核转发", Value: tuiOnOff(gateway.Forwarding)},
		{Label: "规则应用", Value: tuiOnOff(gateway.Applied)},
		{Label: "执行层", Value: defaultTUIText(gateway.Runner, "—")},
		{Label: "redir 端口", Value: tuiPortOrOff(gateway.RedirPort)},
		{Label: "tproxy 端口", Value: tuiPortOrOff(gateway.TProxyPort)},
		{Label: "DNS 劫持", Value: dnsListen},
	}
	if gateway.Error != "" {
		facts = append(facts, tuiFact{Label: "相位错误", Value: gateway.Error})
	}
	if gateway.Err != "" {
		facts = append(facts, tuiFact{Label: "执行层错误", Value: gateway.Err})
	}
	statusPanel := renderTUIPanel(width, "网关状态", "LAN 旁路由 · e 开关网关", renderTUIFacts(tuiPanelBodyWidth(width), facts))

	deviceRows := make([][]string, 0, len(gateway.Devices))
	for _, device := range gateway.Devices {
		deviceRows = append(deviceRows, []string{
			device.Name, device.IP, defaultTUIText(device.MAC, "—"), defaultTUIText(device.Policy, "proxy"),
		})
	}
	deviceColumns := []tuiColumn{
		{Title: "名称", Width: 20},
		{Title: "IP", Width: 18},
		{Title: "MAC", Width: 20},
		{Title: "策略", Width: 0},
	}
	devicePanel := renderTUIPanel(width, "登记设备", fmt.Sprintf("%d 台设备", len(deviceRows)), renderTUITable(deviceColumns, deviceRows, tuiPanelBodyWidth(width)))
	return statusPanel + "\n" + devicePanel
}

// renderDesktopPage 渲染远程桌面的服务监听、连接档案与活动会话。
//
// 参数说明：
//   - width: int，页面可用宽度。
//
// 返回值说明：
//   - string：三张明细表；远端 token 只显示打码摘要。
//   - int：光标行（活动会话表）在完整内容中的绝对行号；无会话时返回 -1。
//
// 错误情况：无；状态未加载时显示明确空状态面板。
func (m tuiModel) renderDesktopPage(width int) (string, int) {
	if m.desktop == nil {
		return renderTUIPanel(width, "远程桌面", "状态未加载", "桌面状态暂不可用，等待下一次快照刷新。"), -1
	}
	desktop := m.desktop
	serviceRows := make([][]string, 0, len(desktop.Services))
	for _, service := range desktop.Services {
		port := "—"
		if service.Port > 0 {
			port = strconv.Itoa(service.Port)
		}
		listening := "未监听"
		if service.Listening {
			listening = "监听中"
		}
		serviceRows = append(serviceRows, []string{
			strings.ToUpper(service.Protocol), port, listening, tuiOnOff(service.Exposed),
		})
	}
	serviceColumns := []tuiColumn{
		{Title: "协议", Width: 10},
		{Title: "端口", Width: 8, Right: true},
		{Title: "监听", Width: 10},
		{Title: "经隧道暴露", Width: 0},
	}
	remoteState := "远程模块已停用"
	if desktop.RemoteEnabled {
		remoteState = "远程模块已启用"
	}
	servicePanel := renderTUIPanel(width, "桌面服务", remoteState, renderTUITable(serviceColumns, serviceRows, tuiPanelBodyWidth(width)))

	profileRows := make([][]string, 0, len(desktop.Connections))
	for _, profile := range desktop.Connections {
		profileRows = append(profileRows, []string{
			profile.Name,
			maskTUIRemoteTarget(profile.Remote),
			strings.ToUpper(profile.Protocol),
			strconv.Itoa(profile.RemotePort),
			defaultTUIText(profile.Username, "—"),
		})
	}
	profileColumns := []tuiColumn{
		{Title: "名称", Width: 18},
		{Title: "远端", Width: 0},
		{Title: "协议", Width: 8},
		{Title: "远端端口", Width: 9, Right: true},
		{Title: "用户名", Width: 14},
	}
	profilePanel := renderTUIPanel(width, "连接档案", fmt.Sprintf("%d 条 · 不保存密码", len(profileRows)), renderTUITable(profileColumns, profileRows, tuiPanelBodyWidth(width)))

	sessionRows := make([][]string, 0, len(desktop.Sessions))
	for _, session := range desktop.Sessions {
		sessionRows = append(sessionRows, []string{
			session.ConnectionName,
			strings.ToUpper(string(session.Protocol)),
			session.LocalAddress,
			strconv.FormatInt(session.ActiveConnections, 10),
			formatTUISessionAge(session.StartedAt),
		})
	}
	sessionColumns := []tuiColumn{
		{Title: "连接", Width: 18},
		{Title: "协议", Width: 8},
		{Title: "本机地址", Width: 0},
		{Title: "活跃连接", Width: 9, Right: true},
		{Title: "时长", Width: 10, Right: true},
	}
	highlight := -1
	if m.cursor < len(sessionRows) {
		highlight = m.cursor
	}
	prefix := servicePanel + "\n" + profilePanel + "\n"
	sessionPanel := renderTUIPanel(width, "活动会话", fmt.Sprintf("%d 个 · x 关闭会话", len(sessionRows)),
		renderTUITableHighlight(sessionColumns, sessionRows, tuiPanelBodyWidth(width), highlight))
	return prefix + sessionPanel, tuiTableCursorLine(prefix, highlight)
}

// renderLogsPage 渲染进程内最近日志，颜色层级由日志等级文本表达。
//
// 参数说明：
//   - width: int，页面可用宽度。
//
// 返回值说明：string，按时间从旧到新排列的日志表。
//
// 错误情况：无；无法解析时间时保留 API 原值，空日志显示表头和空状态。
func (m tuiModel) renderLogsPage(width int) string {
	rows := make([][]string, 0, len(m.logs.Entries))
	for _, entry := range m.logs.Entries {
		rows = append(rows, []string{formatTUILogTime(entry.Time), strings.ToUpper(defaultTUIText(entry.Level, "INFO")), entry.Line})
	}
	columns := []tuiColumn{
		{Title: "时间", Width: 9},
		{Title: "等级", Width: 8},
		{Title: "日志内容", Width: 0},
	}
	return renderTUIPanel(width, "运行日志", fmt.Sprintf("最近 %d 条 · 每 3 秒刷新", len(rows)), renderTUITable(columns, rows, tuiPanelBodyWidth(width)))
}

// renderHelp 渲染交互说明、键位总表与安全边界。
//
// 参数说明：
//   - width: int，页面可用宽度。
//
// 返回值说明：string，替代当前内容区的帮助面板。
//
// 错误情况：无；帮助只改变本地 showHelp 状态。
func (m tuiModel) renderHelp(width int) string {
	body := strings.Join([]string{
		"导航",
		"  1–9, 0        直接打开对应视图（0 = 日志）",
		"  h / l         上一个 / 下一个视图（Tab 同 l）",
		"",
		"光标与滚动",
		"  j / k         可选中页移动光标；入口/规则/网关/日志页滚动",
		"  PgDn / PgUp   半屏移动 / 滚动",
		"  g / G         首行 / 尾行（纯滚动页为页首 / 页尾）",
		"",
		"全局操作（任意页）",
		"  m             代理模式循环：规则 → 全局 → 直连",
		"  s             系统代理开关",
		"  u             TUN 开关",
		"  R             后台刷新全部订阅并测速",
		"  t             后台测速（测速进行中会被拒绝）",
		"  r             立即重新读取快照",
		"",
		"行操作（按当前页生效）",
		"  概览  enter 重试模块 · space 启停模块（停用需确认）",
		"  节点  enter 设为默认出口 · a 默认出口→AUTO · d 删除手动节点（需确认）",
		"  订阅  enter 刷新该订阅 · e / space 启停",
		"  连接  x 关闭选中连接 · X 关闭全部（需确认）",
		"  远程  space 启停选中转发",
		"  网关  e 开关网关（停用需确认）",
		"  桌面  x 关闭选中会话（需确认）",
		"",
		"确认",
		"  y / n / Esc   危险操作：执行 / 取消 / 取消",
		"",
		"状态",
		"  ? / Esc       打开 / 关闭帮助",
		"  q / Ctrl-C    退出并恢复原终端画面",
		"",
		"安全边界",
		"  快照轮询只读：自动刷新只调用 GET 接口；写操作仅由上述显式按键",
		"  触发，全部经 Web 控制台同源 API 执行（服务端配置变更有事务保证）。",
		"  凭据仍打码：完整 token/secret 端点不会被请求，订阅地址只显示摘要。",
	}, "\n")
	return renderTUIPanel(width, "键盘与操作边界", "按 Esc 或 ? 返回", body)
}

// renderConfirm 在内容区中部渲染危险操作的确认面板。
//
// 参数说明：
//   - width: int，页面可用宽度。
//   - bodyHeight: int，内容区可见行数，用于垂直居中。
//
// 返回值说明：string，水平与垂直居中的确认面板，等待 y/n 输入。
//
// 错误情况：无；确认文本由动作构造器给出，过长时截断展示。
func (m tuiModel) renderConfirm(width, bodyHeight int) string {
	boxWidth := min(width, 64)
	text := lipgloss.Wrap(m.confirm.Text, max(20, boxWidth-8), "")
	box := renderTUIPanel(boxWidth, "确认操作", "危险操作", text+"\n\ny 执行 · n / Esc 取消")
	padding := max(0, (bodyHeight-lipgloss.Height(box))/2)
	return strings.Repeat("\n", padding) + lipgloss.PlaceHorizontal(width, lipgloss.Center, box)
}

// renderTUIPill 渲染紧凑状态标签。
//
// 参数说明：
//   - label: string，状态文字。
//   - accent: color.Color，标签背景强调色。
//
// 返回值说明：string，单行高对比度 ANSI 标签。
//
// 错误情况：无；空标签仍渲染最小边框。
func renderTUIPill(label string, accent color.Color) string {
	return lipgloss.NewStyle().
		Bold(true).
		Foreground(tuiBackground).
		Background(accent).
		Padding(0, 1).
		Render(label)
}

// renderTUIPanel 生成统一圆角内容面板。
//
// 参数说明：
//   - width: int，期望面板总宽度。
//   - title: string，主标题。
//   - detail: string，右侧或下一行的辅助说明。
//   - body: string，已排版的正文。
//
// 返回值说明：string，带边框、内边距和背景色的 ANSI 块。
//
// 错误情况：无；极窄宽度使用 12 列下限以避免负尺寸。
func renderTUIPanel(width int, title, detail, body string) string {
	width = max(12, width)
	innerWidth := max(8, width-4)
	titleText := lipgloss.NewStyle().Bold(true).Foreground(tuiText).Render(title)
	header := titleText
	if detail != "" {
		// 辅助说明紧随标题，避免用填满整行的空格模拟右对齐；后者在部分终端
		// 对全角字符宽度判断不一致时会触发意外换行，破坏面板的行高计算。
		detailWidth := max(1, innerWidth-lipgloss.Width(titleText)-3)
		detailText := lipgloss.NewStyle().Foreground(tuiMuted).Render(
			truncateTUIText("· "+detail, detailWidth),
		)
		header += "  " + detailText
	}
	if strings.TrimSpace(body) == "" {
		body = lipgloss.NewStyle().Foreground(tuiMuted).Render("暂无数据")
	}
	return lipgloss.NewStyle().
		Width(innerWidth).
		Foreground(tuiText).
		Background(tuiSurface).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tuiBorder).
		Padding(0, 1).
		Render(header + "\n" + body)
}

// tuiPanelBodyWidth 计算面板内容区（扣除边框与左右内边距）可用的正文宽度。
//
// 参数说明：
//   - width: int，传入 renderTUIPanel 的页面宽度。
//
// 返回值说明：int，表格/事实网格的安全宽度；行内容不超过它才不会在面板内换行，
//   光标行号计算（一行 = 一条记录）也因此成立。
//
// 错误情况：无；极窄宽度使用 8 列下限以避免负尺寸。
func tuiPanelBodyWidth(width int) int {
	return max(8, width-8)
}

// renderTUIStatCard 渲染固定三行的指标卡；width 在布局阶段统一覆盖。
//
// 参数说明：
//   - width: int，初始总宽度；小于 12 时使用下限。
//   - value: string，强调展示的主指标。
//   - detail: string，指标含义或补充值。
//   - accent: lipgloss 可用颜色，作为左边框与主指标颜色。
//
// 返回值说明：string，带强调边框的卡片。
//
// 错误情况：无；超长内容会在最终布局时截断。
func renderTUIStatCard(width int, value, detail string, accent color.Color) string {
	width = max(12, width)
	innerWidth := max(8, width-4)
	return lipgloss.NewStyle().
		Width(innerWidth).
		Height(2).
		Background(tuiSurfaceAlt).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
		Padding(0, 1).
		Render(
			lipgloss.NewStyle().Bold(true).Foreground(accent).Render(truncateTUIText(value, innerWidth)) + "\n" +
				lipgloss.NewStyle().Foreground(tuiMuted).Render(truncateTUIText(detail, innerWidth)),
		)
}

// layoutTUIStatCards 根据终端宽度把四张指标卡排成四列、两列或单列。
//
// 参数说明：
//   - width: int，页面总宽度。
//   - cards: []tuiStat，待渲染的指标卡数据。
//
// 返回值说明：string，自适应卡片网格，末尾保留一行与下一面板分隔。
//
// 错误情况：无；卡片数量不是四时仍按实际数量顺序布局。
func layoutTUIStatCards(width int, cards []tuiStat) string {
	if len(cards) == 0 {
		return ""
	}
	columns := 1
	if width >= 96 {
		columns = 4
	} else if width >= 58 {
		columns = 2
	}
	gap := 1
	cardWidth := max(12, (width-gap*(columns-1))/columns)
	rendered := make([]string, 0, len(cards))
	for _, card := range cards {
		rendered = append(rendered, renderTUIStatCard(cardWidth, card.Value, card.Detail, card.Accent))
	}
	rows := make([]string, 0, (len(rendered)+columns-1)/columns)
	for start := 0; start < len(rendered); start += columns {
		end := min(len(rendered), start+columns)
		parts := make([]string, 0, (end-start)*2-1)
		for index := start; index < end; index++ {
			if index > start {
				parts = append(parts, strings.Repeat(" ", gap))
			}
			parts = append(parts, rendered[index])
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, parts...))
	}
	return strings.Join(rows, "\n") + "\n"
}

// renderTUIFacts 把标签和值组织为响应式两列信息网格。
//
// 参数说明：
//   - width: int，面板正文宽度。
//   - facts: []tuiFact，按阅读顺序排列的状态项。
//
// 返回值说明：string，宽终端两列、窄终端单列的文本块。
//
// 错误情况：无；空集合返回空串，由面板统一显示空状态。
func renderTUIFacts(width int, facts []tuiFact) string {
	if len(facts) == 0 {
		return ""
	}
	columns := 1
	if width >= 62 {
		columns = 2
	}
	cellWidth := max(12, (width-(columns-1)*3)/columns)
	lines := make([]string, 0, (len(facts)+columns-1)/columns)
	for start := 0; start < len(facts); start += columns {
		parts := make([]string, 0, columns)
		for index := start; index < min(len(facts), start+columns); index++ {
			labelWidth := min(14, max(8, cellWidth/3))
			label := lipgloss.NewStyle().Foreground(tuiMuted).Render(fitTUIText(facts[index].Label, labelWidth, false))
			value := lipgloss.NewStyle().Foreground(tuiText).Render(fitTUIText(facts[index].Value, cellWidth-labelWidth, false))
			parts = append(parts, label+value)
		}
		lines = append(lines, strings.Join(parts, "   "))
	}
	return strings.Join(lines, "\n")
}

// renderTUITable 渲染无需交互选择的自适应只读表格。
//
// 参数说明：
//   - columns: []tuiColumn，列标题、建议宽度和对齐方式。
//   - rows: [][]string，原始单元格文本。
//   - width: int，可用表格宽度。
//
// 返回值说明：string，包含表头、分隔线和全部数据行的 ANSI 文本。
//
// 错误情况：无；行列不齐、窄终端和换行文本都会被安全规整与截断。
func renderTUITable(columns []tuiColumn, rows [][]string, width int) string {
	return renderTUITableHighlight(columns, rows, width, -1)
}

// renderTUITableHighlight 渲染自适应表格，并用反色高亮指定选中行。
//
// 参数说明：
//   - columns: []tuiColumn，列标题、建议宽度和对齐方式。
//   - rows: [][]string，原始单元格文本。
//   - width: int，可用表格宽度。
//   - highlight: int，选中行下标（0 基）；负数表示无选中行，与 renderTUITable 一致。
//
// 返回值说明：string，包含表头、分隔线和全部数据行的 ANSI 文本。
//
// 错误情况：无；highlight 越界时安全忽略，不标记任何行。
func renderTUITableHighlight(columns []tuiColumn, rows [][]string, width, highlight int) string {
	if len(columns) == 0 {
		return ""
	}
	widths := resolveTUIColumnWidths(columns, width)
	headerCells := make([]string, len(columns))
	for index, column := range columns {
		headerCells[index] = fitTUIText(column.Title, widths[index], column.Right)
	}
	header := lipgloss.NewStyle().Bold(true).Foreground(tuiBlue).Render(strings.Join(headerCells, "  "))
	separator := lipgloss.NewStyle().Foreground(tuiBorder).Render(strings.Repeat("─", min(width, tableTUIWidth(widths))))
	lines := []string{header, separator}
	if len(rows) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(tuiMuted).Render("暂无数据"))
		return strings.Join(lines, "\n")
	}
	for rowIndex, row := range rows {
		cells := make([]string, len(columns))
		for columnIndex, column := range columns {
			value := ""
			if columnIndex < len(row) {
				value = row[columnIndex]
			}
			cells[columnIndex] = fitTUIText(value, widths[columnIndex], column.Right)
		}
		style := lipgloss.NewStyle().Foreground(tuiText)
		if rowIndex%2 == 1 {
			style = style.Foreground(lipgloss.Color("#B8C7D9"))
		}
		if rowIndex == highlight {
			style = lipgloss.NewStyle().Bold(true).Foreground(tuiBackground).Background(tuiBlue)
		}
		lines = append(lines, style.Render(strings.Join(cells, "  ")))
	}
	return strings.Join(lines, "\n")
}

// tuiTableCursorLine 计算光标行在完整页面内容中的绝对行号。
//
// 参数说明：
//   - prefix: string，光标所在面板之前的全部内容（必须以 "\n" 结尾或为空）。
//   - rowIndex: int，光标在表体中的行号（0 基）；负数表示本页无可选中行。
//
// 返回值说明：int，绝对行号；面板结构为上边框 + 标题 + 表头两行，故行偏移为 4。
//
// 错误情况：无；rowIndex 为负时返回 -1，调用方按纯滚动页处理。
func tuiTableCursorLine(prefix string, rowIndex int) int {
	if rowIndex < 0 {
		return -1
	}
	return strings.Count(prefix, "\n") + 4 + rowIndex
}

// resolveTUIColumnWidths 把建议列宽压缩或扩展到实际终端宽度。
//
// 参数说明：
//   - columns: []tuiColumn，Width=0 的列优先吸收剩余空间。
//   - width: int，包含列间距的总可用宽度。
//
// 返回值说明：[]int，与 columns 等长且每列至少 3 个终端单元格。
//
// 错误情况：无；极窄窗口会逐列等比退让，最终仍保持正宽度。
func resolveTUIColumnWidths(columns []tuiColumn, width int) []int {
	widths := make([]int, len(columns))
	flex := make([]int, 0, len(columns))
	available := max(len(columns)*3, width-(len(columns)-1)*2)
	used := 0
	for index, column := range columns {
		if column.Width <= 0 {
			widths[index] = max(6, lipgloss.Width(column.Title))
			flex = append(flex, index)
		} else {
			widths[index] = max(3, column.Width)
		}
		used += widths[index]
	}
	for used > available {
		largest := -1
		for index, current := range widths {
			if current > 3 && (largest < 0 || current > widths[largest]) {
				largest = index
			}
		}
		if largest < 0 {
			break
		}
		widths[largest]--
		used--
	}
	if used < available {
		target := len(widths) - 1
		if len(flex) > 0 {
			target = flex[0]
		}
		widths[target] += available - used
	}
	return widths
}

// tableTUIWidth 计算列宽与双空格间隔构成的物理表格宽度。
//
// 参数说明：
//   - widths: []int，各列终端单元格宽度。
//
// 返回值说明：int，总宽度；空切片返回 0。
//
// 错误情况：无；负列宽不会由 resolveTUIColumnWidths 产生。
func tableTUIWidth(widths []int) int {
	total := max(0, len(widths)-1) * 2
	for _, width := range widths {
		total += width
	}
	return total
}

// fitTUIText 把纯文本截断并填充到固定终端宽度。
//
// 参数说明：
//   - value: string，待格式化文本；内部换行会替换为空格。
//   - width: int，目标终端单元格宽度。
//   - right: bool，是否右对齐。
//
// 返回值说明：string，显示宽度恰好等于 width 的文本。
//
// 错误情况：无；width 小于 1 时返回空串。
func fitTUIText(value string, width int, right bool) string {
	if width < 1 {
		return ""
	}
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\n", " "), "\r", " ")
	value = truncateTUIText(value, width)
	padding := strings.Repeat(" ", max(0, width-lipgloss.Width(value)))
	if right {
		return padding + value
	}
	return value + padding
}

// truncateTUIText 按终端显示宽度截断 Unicode 文本并添加省略号。
//
// 参数说明：
//   - value: string，不应包含尚未闭合的 ANSI 序列的纯文本。
//   - width: int，允许的最大终端单元格宽度。
//
// 返回值说明：string，原文未超宽时原样返回，超宽时以单字符省略号结尾。
//
// 错误情况：无；width 小于等于 0 返回空串，width 为 1 只返回省略号。
func truncateTUIText(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	var builder strings.Builder
	used := 0
	for _, character := range value {
		characterWidth := lipgloss.Width(string(character))
		if used+characterWidth > width-1 {
			break
		}
		builder.WriteRune(character)
		used += characterWidth
	}
	return builder.String() + "…"
}

// visibleTUILines 按滚动偏移裁切已经渲染的多行 ANSI 文本。
//
// 参数说明：
//   - content: string，完整页面内容。
//   - offset: int，零基起始行。
//   - height: int，最多返回的行数。
//
// 返回值说明：string，可直接放入主视图的可见切片。
//
// 错误情况：无；越界 offset 与非法 height 都会被夹紧。
func visibleTUILines(content string, offset, height int) string {
	lines := strings.Split(content, "\n")
	offset = min(max(0, offset), len(lines))
	end := min(len(lines), offset+max(0, height))
	return strings.Join(lines[offset:end], "\n")
}

// countTUIAliveNodes 统计健康节点数量。
//
// 参数说明：
//   - nodes: []api.NodeEntry，概览返回的节点快照。
//
// 返回值说明：int，Alive 为 true 的记录数。
//
// 错误情况：无；nil 切片返回 0。
func countTUIAliveNodes(nodes []api.NodeEntry) int {
	count := 0
	for _, node := range nodes {
		if node.Alive {
			count++
		}
	}
	return count
}

// resolveTUIMainRoute 解析主入口策略与默认出口：主端口恒为规则模式；
// global/direct 模式下出口由模式本身决定，规则模式的默认出口取内置 PROXY 组的
// 持久化选中项（未选择时落成员首位，选中节点不可用时同样回退成员首位）。
//
// 参数说明：
//   - overview: *api.Overview，包含 mode、groups（含内置 PROXY 项）与节点快照。
//
// 返回值说明：string, string，分别是策略标签和当前默认出口描述。
//
// 错误情况：无；选中节点不可用时明确显示回退成员首位，而不误报原节点生效。
func resolveTUIMainRoute(overview *api.Overview) (string, string) {
	if overview == nil {
		return "未知策略", "暂无数据"
	}
	label := tuiModeLabel(overview.Mode)
	if overview.Mode != "rule" {
		return label, tuiModeExit(overview.Mode)
	}
	selected := ""
	for _, g := range overview.Groups {
		if g.Name == "PROXY" && g.Builtin {
			selected = g.Selected
			break
		}
	}
	switch {
	case selected == "":
		return label, "成员首位（未选择）"
	case selected == "AUTO":
		return label, "AUTO 自动最快"
	case selected == "DIRECT":
		return label, "DIRECT 直连"
	}
	for _, n := range overview.Nodes {
		if n.Name == selected && n.Alive {
			return label, selected
		}
	}
	return label, selected + "（不可用，已回退成员首位）"
}

// connectionDestination 生成活动连接的目标地址。
//
// 参数说明：
//   - connection: connEntry，mihomo 连接记录。
//
// 返回值说明：string，优先使用 host，缺失时用目标 IP，并附加目标端口。
//
// 错误情况：无；目标字段全空时返回破折号。
func connectionDestination(connection connEntry) string {
	destination := connection.Metadata.Host
	if destination == "" {
		destination = connection.Metadata.DestinationIP
	}
	if destination == "" {
		destination = "—"
	}
	if connection.Metadata.DestinationPort != "" {
		destination += ":" + connection.Metadata.DestinationPort
	}
	return destination
}

// maskTUISourceURL 将订阅或规则源地址缩减为不含凭据的来源摘要。
//
// 参数说明：
//   - rawURL: string，API 返回的原始来源地址，可能含用户信息、路径 token 或查询 token。
//
// 返回值说明：string，仅包含 host 与固定省略标记；非 URL 输入返回“已隐藏”。
//
// 错误情况：解析失败不会透出原文，避免终端录屏或日志意外泄露凭据。
func maskTUISourceURL(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Hostname() == "" {
		return "已隐藏"
	}
	host := parsed.Hostname()
	if port := parsed.Port(); port != "" {
		host += ":" + port
	}
	return host + "/…"
}

// maskTUIRemoteTarget 隐藏转发配置中可能直接填写的 tailcat token。
//
// 参数说明：
//   - target: string，已保存远端名称或 `tc...` token。
//
// 返回值说明：string，普通名称原样返回，token 只保留首尾摘要。
//
// 错误情况：无；过短 token 整体替换为三个星号。
func maskTUIRemoteTarget(target string) string {
	if !strings.HasPrefix(target, "tc") {
		return target
	}
	if len(target) <= 14 {
		return "***"
	}
	return target[:6] + "…" + target[len(target)-6:]
}

// formatTUIDelay 将节点延迟与健康状态转换为短文本。
//
// 参数说明：
//   - delay: uint16，毫秒延迟。
//   - alive: bool，节点健康状态。
//
// 返回值说明：string，健康且有测量值时返回“123ms”，否则返回破折号。
//
// 错误情况：无；零延迟不会被误报为真实测速结果。
func formatTUIDelay(delay uint16, alive bool) string {
	if !alive || delay == 0 {
		return "—"
	}
	return fmt.Sprintf("%dms", delay)
}

// formatTUILogTime 将 RFC3339 日志时间压缩为本地时分秒。
//
// 参数说明：
//   - value: string，日志 API 返回的时间字符串。
//
// 返回值说明：string，解析成功返回 HH:MM:SS，失败返回安全截断的原文。
//
// 错误情况：解析错误被吸收，仅影响展示格式。
func formatTUILogTime(value string) string {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return truncateTUIText(value, 8)
	}
	return parsed.Local().Format("15:04:05")
}

// tuiModeLabel 返回代理模式的中文展示名称。
//
// 参数说明：
//   - mode: string，rule/global/direct 或未来扩展值。
//
// 返回值说明：string，已知枚举使用中文，未知值保留原文以便诊断。
//
// 错误情况：无；空值返回“未知模式”。
func tuiModeLabel(mode string) string {
	switch mode {
	case "rule":
		return "规则分流"
	case "global":
		return "全局代理"
	case "direct":
		return "全部直连"
	default:
		return defaultTUIText(mode, "未知模式")
	}
}

// tuiModeExit 返回指定模式下无法进一步确定节点时的出口描述。
//
// 参数说明：
//   - mode: string，当前主端口模式。
//
// 返回值说明：string，描述规则动态出口、PROXY 组或直连。
//
// 错误情况：无；未知枚举返回“由内核决定”。
func tuiModeExit(mode string) string {
	switch mode {
	case "rule":
		return "按规则动态选择"
	case "global":
		return "PROXY 选择组"
	case "direct":
		return "直接连接"
	default:
		return "由内核决定"
	}
}

// tuiSubscriptionState 把订阅状态枚举转换为中文。
//
// 参数说明：
//   - state: string，disabled/empty/error/degraded/healthy。
//
// 返回值说明：string，适合表格的短状态文本。
//
// 错误情况：无；未知值保留原文。
func tuiSubscriptionState(state string) string {
	switch state {
	case "disabled":
		return "已停用"
	case "empty":
		return "无节点"
	case "error":
		return "异常"
	case "degraded":
		return "缓存降级"
	case "healthy":
		return "正常"
	default:
		return defaultTUIText(state, "未知")
	}
}

// tuiGroupType 把 mihomo 分组类型转换为更易读的中文标签。
//
// 参数说明：
//   - groupType: string，url-test/fallback/load-balance 或空值。
//
// 返回值说明：string，已知类型的中文语义。
//
// 错误情况：无；空值沿用历史默认“自动测速”。
func tuiGroupType(groupType string) string {
	switch groupType {
	case "", "url-test":
		return "自动测速"
	case "fallback":
		return "故障转移"
	case "load-balance":
		return "负载均衡"
	case "select":
		return "手动选择"
	default:
		return groupType
	}
}

// tuiDNSLabel 表达 DNS 预设及手写配置覆盖关系。
//
// 参数说明：
//   - preset: string，off/fake-ip/redir-host。
//   - custom: bool，是否存在优先级更高的手写 DNS 段。
//
// 返回值说明：string，当前有效来源的可见说明。
//
// 错误情况：无；未知预设保留原值。
func tuiDNSLabel(preset string, custom bool) string {
	if custom {
		return "手写配置"
	}
	return defaultTUIText(preset, "off")
}

// tuiOnOff 将布尔配置状态转换为开启/关闭。
//
// 参数说明：
//   - enabled: bool，配置开关。
//
// 返回值说明：string，开启或关闭。
//
// 错误情况：无。
func tuiOnOff(enabled bool) string {
	if enabled {
		return "开启"
	}
	return "关闭"
}

// tuiActiveState 同时表达配置意图与实际运行状态。
//
// 参数说明：
//   - enabled: bool，配置是否开启。
//   - active: bool，运行时是否真正生效。
//
// 返回值说明：string，关闭、已生效或已配置未生效。
//
// 错误情况：无。
func tuiActiveState(enabled, active bool) string {
	if active {
		return "已生效"
	}
	if enabled {
		return "已配置 · 未生效"
	}
	return "关闭"
}

// tuiTUNState 在 TUN 运行状态后补充权限提示。
//
// 参数说明：
//   - tun: app.TUNStatus，TUN 开关与当前进程权限快照。
//
// 返回值说明：string，已配置但无权限时附带平台修复提示。
//
// 错误情况：无；Permission 为空时退化为纯运行状态文本。
func tuiTUNState(tun app.TUNStatus) string {
	state := tuiActiveState(tun.Enabled, tun.Active)
	if tun.Enabled && !tun.Allowed && tun.Permission != "" {
		state += " · " + tun.Permission
	}
	return state
}

// tuiAutostartState 在自启开关后附带托管进程运行态。
//
// 参数说明：
//   - overview: *api.Overview，包含 Autostart 与 AutostartRuntime 快照。
//
// 返回值说明：string，如“开启 · 运行中 (pid 1234)”；未注册时返回关闭。
//
// 错误情况：无；运行态字段缺失时退化为开关文本。
func tuiAutostartState(overview *api.Overview) string {
	if !overview.Autostart {
		return "关闭"
	}
	runtime := overview.AutostartRuntime
	switch {
	case runtime.Running && runtime.PID > 0:
		return fmt.Sprintf("开启 · 运行中 (pid %d)", runtime.PID)
	case runtime.Message != "":
		return "开启 · " + runtime.Message
	case runtime.State != "":
		return "开启 · " + runtime.State
	default:
		return "开启"
	}
}

// tuiGatewayPhase 组合网关模块相位与运行标记。
//
// 参数说明：
//   - gateway: app.GatewayOverview，网关完整状态视图。
//
// 返回值说明：string，如“running · 运行中”；相位为空时返回破折号。
//
// 错误情况：无。
func tuiGatewayPhase(gateway app.GatewayOverview) string {
	phase := defaultTUIText(gateway.Phase, "—")
	if gateway.Running {
		phase += " · 运行中"
	}
	return phase
}

// formatTUIUptime 把系统运行秒数压缩为中文时长。
//
// 参数说明：
//   - system: *app.SystemStatus，系统状态快照；nil 表示尚未加载。
//
// 返回值说明：string，如“2 天 3 小时”；未加载返回破折号。
//
// 错误情况：无；负值按 0 秒处理。
func formatTUIUptime(system *app.SystemStatus) string {
	if system == nil {
		return "—"
	}
	seconds := max(0, system.UptimeSeconds)
	switch {
	case seconds < 60:
		return fmt.Sprintf("%d 秒", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%d 分钟", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%d 小时 %d 分", seconds/3600, seconds%3600/60)
	default:
		return fmt.Sprintf("%d 天 %d 小时", seconds/86400, seconds%86400/3600)
	}
}

// formatTUISessionAge 把桌面会话的开始时间转换为已持续时长。
//
// 参数说明：
//   - started: time.Time，会话启动时间。
//
// 返回值说明：string，如“45s”“12m”“3h5m”；零值返回破折号。
//
// 错误情况：无；未来时间按 0s 处理。
func formatTUISessionAge(started time.Time) string {
	if started.IsZero() {
		return "—"
	}
	elapsed := max(0, int64(time.Since(started).Seconds()))
	switch {
	case elapsed < 60:
		return fmt.Sprintf("%ds", elapsed)
	case elapsed < 3600:
		return fmt.Sprintf("%dm", elapsed/60)
	default:
		return fmt.Sprintf("%dh%dm", elapsed/3600, elapsed%3600/60)
	}
}

// tuiAuditPeer 生成审计事件的对端展示名。
//
// 参数说明：
//   - entry: tuiAuditEntry，远程审计事件。
//
// 返回值说明：string，优先 SSH 公钥别名，其次客户端名，最后为密钥首尾摘要。
//
// 错误情况：无；对端字段全空返回破折号，过短密钥整体替换为三个星号。
func tuiAuditPeer(entry tuiAuditEntry) string {
	if entry.SSHKeyName != "" {
		return entry.SSHKeyName
	}
	if entry.ClientName != "" {
		return entry.ClientName
	}
	key := entry.SSHFingerprint
	if key == "" {
		key = entry.ClientKey
	}
	if key == "" {
		return "—"
	}
	if len(key) <= 14 {
		return "***"
	}
	return key[:6] + "…" + key[len(key)-6:]
}

// tuiPortOrOff 将可选端口转换为端口号或关闭状态。
//
// 参数说明：
//   - port: int，0 表示关闭。
//
// 返回值说明：string，正数端口或“关闭”。
//
// 错误情况：无；负值也按关闭处理，避免异常快照污染界面。
func tuiPortOrOff(port int) string {
	if port <= 0 {
		return "关闭"
	}
	return strconv.Itoa(port)
}

// tuiPortOrDash 将节点未映射端口显示为破折号。
//
// 参数说明：
//   - port: int，节点端口；0 表示没有监听。
//
// 返回值说明：string，正数端口或破折号。
//
// 错误情况：无；负值按未映射处理。
func tuiPortOrDash(port int) string {
	if port <= 0 {
		return "—"
	}
	return strconv.Itoa(port)
}

// defaultTUIText 为展示字符串提供空值占位。
//
// 参数说明：
//   - value: string，可能为空的原值。
//   - fallback: string，空值时使用的文本。
//
// 返回值说明：string，去除首尾空白后的 value 或 fallback。
//
// 错误情况：无。
func defaultTUIText(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
