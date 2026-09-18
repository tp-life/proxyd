package main

// 本文件实现 TUI 的写操作层：动作框架、键位分派、危险操作确认与执行反馈。
//
// 所有动作都通过 apiClient 调用 Web 控制台同源 API（服务端配置变更有事务保证），
// 只由显式按键触发；快照轮询保持 GET-only，动作完成后立即重取一次快照反映新状态。

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	tea "charm.land/bubbletea/v2"
)

// tuiSubRefreshTimeout 放宽单个订阅同步刷新的等待上限（服务端最长执行 3 分钟）。
const tuiSubRefreshTimeout = 190 * time.Second

// tuiAction 描述一次可执行的 API 写操作。
type tuiAction struct {
	Label   string                          // 反馈行与底栏「执行中」展示名
	Confirm string                          // 非空则需 y/n 确认（确认文案）
	Run     func(c *apiClient) (string, error) // 执行并返回成功反馈文本
}

// tuiActionResultMsg 汇报一次动作的执行结果。
type tuiActionResultMsg struct {
	Label string
	Text  string
	Err   error
}

// tuiConfirm 保存等待 y/n 确认的危险动作。
type tuiConfirm struct {
	Text   string
	Action tuiAction
}

// runTUIActionCmd 把动作包装成后台命令，完成后发送结果消息。
//
// 参数说明：
//   - client: *apiClient，运行中 proxyd 的本地 API 客户端。
//   - action: tuiAction，待执行的写操作。
//
// 返回值说明：tea.Cmd，始终发送 tuiActionResultMsg；服务端错误文本原样进入 Err。
//
// 错误情况：网络或服务端错误不会 panic，统一由结果消息带回模型置反馈行。
func runTUIActionCmd(client *apiClient, action tuiAction) tea.Cmd {
	return func() tea.Msg {
		text, err := action.Run(client)
		return tuiActionResultMsg{Label: action.Label, Text: text, Err: err}
	}
}

// queueAction 是动作的统一入口：需要确认的先进入确认态，否则立即执行。
//
// 参数说明：
//   - action: tuiAction，已解析出目标与请求体的动作。
//
// 返回值说明：更新后的 tea.Model 与可选动作命令。
//
// 错误情况：无；执行阶段的错误经 tuiActionResultMsg 异步返回。
func (m tuiModel) queueAction(action tuiAction) (tea.Model, tea.Cmd) {
	if action.Confirm != "" {
		m.confirm = &tuiConfirm{Text: action.Confirm, Action: action}
		return m, nil
	}
	return m.runAction(action)
}

// runAction 标记执行中并启动后台动作命令，同时清除上一条反馈。
//
// 参数说明：
//   - action: tuiAction，无需确认或已通过确认的动作。
//
// 返回值说明：更新后的 tea.Model 与动作命令。
//
// 错误情况：无；busy 期间 handleKey 会忽略新的动作键。
func (m tuiModel) runAction(action tuiAction) (tea.Model, tea.Cmd) {
	m.actionBusy = true
	m.actionLabel = action.Label
	m.actionNote = ""
	m.actionErr = false
	return m, runTUIActionCmd(m.client, action)
}

// handleConfirmKey 处理确认态按键：y 执行、n/esc 取消，其余键忽略。
//
// 参数说明：
//   - key: string，当前按键的规范化文本。
//
// 返回值说明：更新后的 tea.Model 与可选动作命令。
//
// 错误情况：无；取消只清除确认态并给出中性反馈，不产生任何请求。
func (m tuiModel) handleConfirmKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "y":
		pending := m.confirm
		m.confirm = nil
		return m.runAction(pending.Action)
	case "n", "esc":
		m.confirm = nil
		m.actionNote = "已取消"
		m.actionErr = false
		return m, nil
	default:
		return m, nil
	}
}

// dispatchAction 把非导航按键按当前页与光标行解析为具体动作并排队执行。
//
// 参数说明：
//   - key: string，当前按键的规范化文本。
//
// 返回值说明：更新后的 tea.Model 与可选动作命令。
//
// 错误情况：无；busy 期间、目标为空（如无选中行）或未绑定该键时安全忽略。
func (m tuiModel) dispatchAction(key string) (tea.Model, tea.Cmd) {
	if m.actionBusy {
		return m, nil
	}
	if action, ok := m.buildGlobalAction(key); ok {
		return m.queueAction(action)
	}
	action, ok := m.buildRowAction(key)
	if !ok {
		return m, nil
	}
	return m.queueAction(action)
}

// buildGlobalAction 构造任意页都可用的全局动作（模式/系统代理/TUN/刷新/测速）。
//
// 参数说明：
//   - key: string，当前按键的规范化文本。
//
// 返回值说明：tuiAction 与 bool，键未绑定或概览未加载时 ok 为 false。
//
// 错误情况：无；Testing 中的测速请求被本地拒绝并以反馈文本提示，不发请求。
func (m tuiModel) buildGlobalAction(key string) (tuiAction, bool) {
	if m.overview == nil {
		return tuiAction{}, false
	}
	switch key {
	case "m":
		next := tuiNextMode(m.overview.Mode)
		return tuiAction{
			Label: "切换代理模式",
			Run: func(c *apiClient) (string, error) {
				if err := c.do(http.MethodPost, "/api/mode", map[string]string{"mode": next}, nil); err != nil {
					return "", err
				}
				return "代理模式 → " + tuiModeLabel(next), nil
			},
		}, true
	case "s":
		enable := !m.overview.SystemProxy
		return tuiAction{
			Label: "系统代理开关",
			Run: func(c *apiClient) (string, error) {
				if err := c.do(http.MethodPost, "/api/system-proxy", map[string]bool{"enabled": enable}, nil); err != nil {
					return "", err
				}
				return "系统代理" + tuiActionOnOff(enable), nil
			},
		}, true
	case "u":
		enable := !m.overview.TUN.Enabled
		return tuiAction{
			Label: "TUN 开关",
			Run: func(c *apiClient) (string, error) {
				if err := c.do(http.MethodPost, "/api/tun", map[string]bool{"enabled": enable}, nil); err != nil {
					return "", err
				}
				return "TUN" + tuiActionOnOff(enable), nil
			},
		}, true
	case "R":
		return tuiAction{
			Label: "全局刷新",
			Run: func(c *apiClient) (string, error) {
				if err := c.do(http.MethodPost, "/api/refresh", nil, nil); err != nil {
					return "", err
				}
				return "后台刷新订阅与测速进行中", nil
			},
		}, true
	case "t":
		if m.overview.Testing {
			return tuiAction{
				Label: "全局测速",
				Run: func(_ *apiClient) (string, error) {
					return "", fmt.Errorf("测速进行中，请稍候")
				},
			}, true
		}
		return tuiAction{
			Label: "全局测速",
			Run: func(c *apiClient) (string, error) {
				if err := c.do(http.MethodPost, "/api/test", nil, nil); err != nil {
					return "", err
				}
				return "后台测速进行中", nil
			},
		}, true
	default:
		return tuiAction{}, false
	}
}

// buildRowAction 按当前页与光标行构造行级动作。
//
// 参数说明：
//   - key: string，当前按键的规范化文本（enter/space/e/x/X/d/a 按页分派）。
//
// 返回值说明：tuiAction 与 bool，键在当前页无绑定或光标行为空时 ok 为 false。
//
// 错误情况：无；目标解析只读内存快照，不会为解析目标发出请求。
func (m tuiModel) buildRowAction(key string) (tuiAction, bool) {
	if m.overview == nil {
		return tuiAction{}, false
	}
	switch m.page {
	case tuiPageOverview:
		return m.buildModuleAction(key)
	case tuiPageNodes:
		return m.buildNodeAction(key)
	case tuiPageSubscriptions:
		return m.buildSubscriptionAction(key)
	case tuiPageConnections:
		return m.buildConnectionAction(key)
	case tuiPageRemote:
		return m.buildForwardAction(key)
	case tuiPageGateway:
		return m.buildGatewayAction(key)
	case tuiPageDesktop:
		return m.buildSessionAction(key)
	default:
		return tuiAction{}, false
	}
}

// buildModuleAction 构造概览页模块表的重试/启停动作。
//
// 参数说明：
//   - key: string，enter 重试选中模块，space 启停选中模块。
//
// 返回值说明：tuiAction 与 bool，模块列表为空或键未绑定时 ok 为 false。
//
// 错误情况：无；停用模块属于危险操作，返回的动作带确认文案。
func (m tuiModel) buildModuleAction(key string) (tuiAction, bool) {
	if m.cursor >= len(m.modules) {
		return tuiAction{}, false
	}
	module := m.modules[m.cursor]
	switch key {
	case "enter":
		return tuiAction{
			Label: "重试模块 " + module.Name,
			Run: func(c *apiClient) (string, error) {
				if err := c.do(http.MethodPost, "/api/modules/"+url.PathEscape(module.ID)+"/retry", nil, nil); err != nil {
					return "", err
				}
				return "已触发模块「" + module.Name + "」重试", nil
			},
		}, true
	case "space":
		enable := !module.Enabled
		action := tuiAction{
			Label: "启停模块 " + module.Name,
			Run: func(c *apiClient) (string, error) {
				if err := c.do(http.MethodPost, "/api/modules/"+url.PathEscape(module.ID), map[string]bool{"enabled": enable}, nil); err != nil {
					return "", err
				}
				return "模块「" + module.Name + "」" + tuiActionOnOff(enable), nil
			},
		}
		if !enable {
			action.Confirm = fmt.Sprintf("确认停用模块「%s」？停用后其功能立即下线。", module.Name)
		}
		return action, true
	default:
		return tuiAction{}, false
	}
}

// buildNodeAction 构造节点页的主出口、自动选优与删除手动节点动作。
//
// 参数说明：
//   - key: string，enter 设主出口（光标须在节点表），a 自动选优开关，d 删手动节点（光标须在手动表）。
//
// 返回值说明：tuiAction 与 bool，键未绑定或光标不在对应表时 ok 为 false。
//
// 错误情况：无；删除手动节点带确认文案；主出口动作在一次 Run 内顺序调用
// main-node 再关闭 main-auto，与 Web 控制台两步操作保持一致。
func (m tuiModel) buildNodeAction(key string) (tuiAction, bool) {
	nodes := sortedTUINodes(m.overview.Nodes)
	nodeCount := len(nodes)
	switch key {
	case "enter":
		if m.cursor >= nodeCount {
			return tuiAction{}, false
		}
		node := nodes[m.cursor]
		return tuiAction{
			Label: "设置主出口",
			Run: func(c *apiClient) (string, error) {
				if err := c.do(http.MethodPost, "/api/main-node", map[string]string{"node": node.Key}, nil); err != nil {
					return "", err
				}
				if err := c.do(http.MethodPost, "/api/main-auto", map[string]bool{"enabled": false}, nil); err != nil {
					return "", err
				}
				return "主出口 → " + node.Name, nil
			},
		}, true
	case "a":
		enable := !m.overview.MainAuto
		return tuiAction{
			Label: "自动选优开关",
			Run: func(c *apiClient) (string, error) {
				if err := c.do(http.MethodPost, "/api/main-auto", map[string]bool{"enabled": enable}, nil); err != nil {
					return "", err
				}
				return "自动选优" + tuiActionOnOff(enable), nil
			},
		}, true
	case "d":
		manualIndex := m.cursor - nodeCount
		if manualIndex < 0 || manualIndex >= len(m.overview.ManualNodes) {
			return tuiAction{}, false
		}
		manual := m.overview.ManualNodes[manualIndex]
		name := manual.Name
		if name == "" {
			name = fmt.Sprintf("#%d", manual.Index)
		}
		return tuiAction{
			Label:   "删除手动节点 " + name,
			Confirm: fmt.Sprintf("确认删除手动节点「%s」？该操作会写回配置文件。", name),
			Run: func(c *apiClient) (string, error) {
				if err := c.do(http.MethodDelete, fmt.Sprintf("/api/manual-nodes/%d", manual.Index), nil, nil); err != nil {
					return "", err
				}
				return "手动节点「" + name + "」已删除", nil
			},
		}, true
	default:
		return tuiAction{}, false
	}
}

// buildSubscriptionAction 构造订阅页的单订阅刷新与启停动作。
//
// 参数说明：
//   - key: string，enter 同步刷新该订阅，e/space 启停该订阅。
//
// 返回值说明：tuiAction 与 bool，订阅列表为空或键未绑定时 ok 为 false。
//
// 错误情况：无；启停 PUT 整体提交（Name/URL/Type/Enabled/PortMapping 取自
// SubEntry，仅翻转 Enabled），原始 URL 只在内存往返，不在界面展示。
func (m tuiModel) buildSubscriptionAction(key string) (tuiAction, bool) {
	if m.cursor >= len(m.overview.Subs) {
		return tuiAction{}, false
	}
	sub := m.overview.Subs[m.cursor]
	path := "/api/subscriptions/" + url.PathEscape(sub.Name)
	switch key {
	case "enter":
		return tuiAction{
			Label: "刷新订阅 " + sub.Name,
			Run: func(c *apiClient) (string, error) {
				if err := c.doTimeout(http.MethodPost, path+"/refresh", nil, nil, tuiSubRefreshTimeout); err != nil {
					return "", err
				}
				return "订阅「" + sub.Name + "」刷新完成", nil
			},
		}, true
	case "e", "space":
		enable := !sub.Enabled
		return tuiAction{
			Label: "启停订阅 " + sub.Name,
			Run: func(c *apiClient) (string, error) {
				body := map[string]any{
					"name":         sub.Name,
					"url":          sub.URL,
					"type":         sub.Type,
					"enabled":      enable,
					"port_mapping": sub.PortMapping,
				}
				if err := c.do(http.MethodPut, path, body, nil); err != nil {
					return "", err
				}
				return "订阅「" + sub.Name + "」" + tuiActionOnOff(enable), nil
			},
		}, true
	default:
		return tuiAction{}, false
	}
}

// buildConnectionAction 构造连接页的关闭单条/全部连接动作。
//
// 参数说明：
//   - key: string，x 关闭选中连接，X 关闭全部连接。
//
// 返回值说明：tuiAction 与 bool，连接列表为空或键未绑定时 ok 为 false。
//
// 错误情况：无；关闭单条使用完整 connection.ID（非界面上截断的 shortID），
// 关闭全部带确认文案。
func (m tuiModel) buildConnectionAction(key string) (tuiAction, bool) {
	connections := sortedTUIConnections(m.connections.Connections)
	switch key {
	case "x":
		if m.cursor >= len(connections) {
			return tuiAction{}, false
		}
		connection := connections[m.cursor]
		return tuiAction{
			Label: "关闭连接",
			Run: func(c *apiClient) (string, error) {
				if err := c.do(http.MethodDelete, "/api/connections/"+url.PathEscape(connection.ID), nil, nil); err != nil {
					return "", err
				}
				return "连接 " + shortID(connection.ID) + " 已关闭", nil
			},
		}, true
	case "X":
		if len(connections) == 0 {
			return tuiAction{}, false
		}
		return tuiAction{
			Label:   "关闭全部连接",
			Confirm: fmt.Sprintf("确认关闭全部 %d 条活动连接？", len(connections)),
			Run: func(c *apiClient) (string, error) {
				if err := c.do(http.MethodDelete, "/api/connections", nil, nil); err != nil {
					return "", err
				}
				return "全部连接已关闭", nil
			},
		}, true
	default:
		return tuiAction{}, false
	}
}

// buildForwardAction 构造远程页的本地转发启停动作。
//
// 参数说明：
//   - key: string，space 启停选中转发。
//
// 返回值说明：tuiAction 与 bool，转发表为空或键未绑定时 ok 为 false。
//
// 错误情况：无；翻转的是配置意图 forward.Enabled（不是运行态 Running）。
func (m tuiModel) buildForwardAction(key string) (tuiAction, bool) {
	if key != "space" || m.cursor >= len(m.remote.Forwards) {
		return tuiAction{}, false
	}
	forward := m.remote.Forwards[m.cursor]
	enable := !forward.Enabled
	return tuiAction{
		Label: "启停转发 " + forward.Name,
		Run: func(c *apiClient) (string, error) {
			path := "/api/remote/forwards/" + url.PathEscape(forward.Name)
			if err := c.do(http.MethodPut, path, map[string]bool{"enabled": enable}, nil); err != nil {
				return "", err
			}
			return "转发「" + forward.Name + "」" + tuiActionOnOff(enable), nil
		},
	}, true
}

// buildGatewayAction 构造网关页的模块开关动作。
//
// 参数说明：
//   - key: string，e 切换网关模块启停。
//
// 返回值说明：tuiAction 与 bool，网关状态未加载或键未绑定时 ok 为 false。
//
// 错误情况：无；停用网关属于危险操作，返回的动作带确认文案。
func (m tuiModel) buildGatewayAction(key string) (tuiAction, bool) {
	if key != "e" || m.gateway == nil {
		return tuiAction{}, false
	}
	enable := !m.gateway.Enabled
	action := tuiAction{
		Label: "网关开关",
		Run: func(c *apiClient) (string, error) {
			if err := c.do(http.MethodPost, "/api/gateway", map[string]bool{"enabled": enable}, nil); err != nil {
				return "", err
			}
			return "LAN 网关" + tuiActionOnOff(enable), nil
		},
	}
	if !enable {
		action.Confirm = "确认停用 LAN 网关？登记设备将立即失去分流出口。"
	}
	return action, true
}

// buildSessionAction 构造桌面页的关闭会话动作。
//
// 参数说明：
//   - key: string，x 关闭选中会话。
//
// 返回值说明：tuiAction 与 bool，会话表为空或键未绑定时 ok 为 false。
//
// 错误情况：无；关闭会话会断开其中的桌面连接，返回的动作带确认文案。
func (m tuiModel) buildSessionAction(key string) (tuiAction, bool) {
	if key != "x" || m.desktop == nil || m.cursor >= len(m.desktop.Sessions) {
		return tuiAction{}, false
	}
	session := m.desktop.Sessions[m.cursor]
	return tuiAction{
		Label:   "关闭会话",
		Confirm: fmt.Sprintf("确认关闭会话「%s」（%s）？其中的桌面连接会被断开。", session.ConnectionName, session.Protocol),
		Run: func(c *apiClient) (string, error) {
			if err := c.do(http.MethodDelete, "/api/desktop/sessions/"+url.PathEscape(session.ID), nil, nil); err != nil {
				return "", err
			}
			return "会话「" + session.ConnectionName + "」已关闭", nil
		},
	}, true
}

// tuiNextMode 返回代理模式的循环下一档。
//
// 参数说明：
//   - current: string，当前模式（rule/global/direct）。
//
// 返回值说明：string，按 rule→global→direct→rule 循环；未知值回到 rule。
//
// 错误情况：无。
func tuiNextMode(current string) string {
	switch current {
	case "rule":
		return "global"
	case "global":
		return "direct"
	default:
		return "rule"
	}
}

// tuiActionOnOff 生成动作反馈中的开关结果文本。
//
// 参数说明：
//   - enable: bool，动作目标状态。
//
// 返回值说明：string，"已开启" 或 "已关闭"。
//
// 错误情况：无。
func tuiActionOnOff(enable bool) string {
	if enable {
		return "已开启"
	}
	return "已关闭"
}
