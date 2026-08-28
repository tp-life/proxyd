// Package app orchestrates proxyd: subscription refresh, health checks,
// port allocation and hot-reloading the embedded mihomo core.
package app

import (
	"context"
	"fmt"
	"log"
	urlpkg "net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/tunnel"

	"proxyd/internal/autostart"
	"proxyd/internal/config"
	"proxyd/internal/core"
	"proxyd/internal/node"
	"proxyd/internal/pool"
	"proxyd/internal/ruleurl"
	"proxyd/internal/subscribe"
	"proxyd/internal/sysproxy"
)

// RuleURLStat 是规则源的最近一次拉取状态（供 API 展示）。
type RuleURLStat struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Count int    `json:"count"`
	Error string `json:"error,omitempty"` // 拉取与缓存都失败
	Warn  string `json:"warn,omitempty"`  // 拉取失败但降级用了缓存
}

// App 是 proxyd 的运行时主体。
type App struct {
	cfg     *config.Config
	cfgPath string // 配置文件路径，配置变更时持久化；为空则不落盘
	runner  *core.Runner

	mu        sync.RWMutex
	nodes     []*node.Node           // 最近一次订阅合并结果
	assigns   []pool.Assignment      // 最近一次端口分配结果
	imported  map[string][]string    // rule-url 名 -> 导入的规则行
	ruleStats map[string]RuleURLStat // rule-url 名 -> 最近拉取状态

	excludeRe  *regexp.Regexp
	refreshing sync.Mutex // 保证刷新流水线串行执行

	// mainListenerOn 记录最近一次成功应用的配置里主端口是否为固定 listener 形态
	// （main-auto/main-node 生效）；用于 regenerateWithLocked 判断是否需要
	// 先释放主端口（mixed-port → 同端口 listener 直接热更新会 bind 冲突）。
	mainListenerOn bool
}

// New 创建 App。cfgPath 为配置变更时持久化用的配置文件路径（可为空）。
func New(cfg *config.Config, cfgPath string) (*App, error) {
	a := &App{
		cfg:       cfg,
		cfgPath:   cfgPath,
		runner:    core.NewRunner(cfg.StateDir),
		imported:  map[string][]string{},
		ruleStats: map[string]RuleURLStat{},
	}
	if cfg.Exclude != "" {
		re, err := regexp.Compile(cfg.Exclude)
		if err != nil {
			return nil, fmt.Errorf("compile exclude regexp: %w", err)
		}
		a.excludeRe = re
	}
	return a, nil
}

// Assignments 返回当前端口映射的只读快照（供 API 使用）。
func (a *App) Assignments() []pool.Assignment {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]pool.Assignment, len(a.assigns))
	copy(out, a.assigns)
	return out
}

// Nodes 返回当前节点列表快照（含健康状态）。
func (a *App) Nodes() []*node.Node {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*node.Node, len(a.nodes))
	copy(out, a.nodes)
	return out
}

// Subscriptions 返回配置中的订阅列表（供 API 展示）。
func (a *App) Subscriptions() []config.Subscription {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]config.Subscription, len(a.cfg.Subscriptions))
	copy(out, a.cfg.Subscriptions)
	return out
}

// Config 返回只读用的运行配置。
func (a *App) Config() *config.Config { return a.cfg }

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

// SetAutoPort 设置自动选优端口（0=关闭），持久化并热更新。
func (a *App) SetAutoPort(port int) error {
	a.mu.Lock()
	if err := a.cfg.CheckAutoPort(port); err != nil {
		a.mu.Unlock()
		return err
	}
	old := a.cfg.AutoPort
	a.cfg.AutoPort = port
	a.mu.Unlock()
	if err := a.Regenerate(); err != nil {
		a.mu.Lock()
		a.cfg.AutoPort = old
		a.mu.Unlock()
		_ = a.Regenerate()
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// SetMainAuto 开关「主端口使用最优节点」：开启后主端口跳过规则匹配、
// 固定走 AUTO url-test 组；关闭恢复规则模式。持久化并热更新。
// 主端口 mixed-port ↔ listener 形态转换的两阶段释放由 regenerateWithLocked 统一处理。
func (a *App) SetMainAuto(enabled bool) error {
	a.mu.Lock()
	old := a.cfg.MainAuto
	a.cfg.MainAuto = enabled
	a.mu.Unlock()
	if err := a.Regenerate(); err != nil {
		a.mu.Lock()
		a.cfg.MainAuto = old
		a.mu.Unlock()
		_ = a.Regenerate()
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// SetMainNode 设置主端口固定节点（node Key；空串 = 恢复规则模式），持久化并热更新。
// main-auto 开启时该设置被忽略（auto 优先），仍可保存。
// main-auto ↔ main-node（listener 同名 L<port>、仅 proxy 目标变化）
// 与 listener → mixed-port 方向由 mihomo 按 关闭→监听 顺序处理；
// mixed-port → listener 方向的过渡释放由 regenerateWithLocked 统一处理。
func (a *App) SetMainNode(key string) error {
	a.mu.Lock()
	old := a.cfg.MainNode
	a.cfg.MainNode = key
	a.mu.Unlock()
	if err := a.Regenerate(); err != nil {
		a.mu.Lock()
		a.cfg.MainNode = old
		a.mu.Unlock()
		_ = a.Regenerate()
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// SetMainPort 修改主端口（mixed-port），校验冲突后持久化并热更新；
// 系统代理当前已开启时自动重新绑定到新端口。
func (a *App) SetMainPort(port int) error {
	a.mu.Lock()
	if err := a.cfg.CheckMixedPort(port); err != nil {
		a.mu.Unlock()
		return err
	}
	old := a.cfg.MixedPort
	a.cfg.MixedPort = port
	sysOn := a.cfg.SystemProxy
	a.mu.Unlock()
	if err := a.Regenerate(); err != nil {
		a.mu.Lock()
		a.cfg.MixedPort = old
		a.mu.Unlock()
		_ = a.Regenerate()
		return err
	}
	if sysOn {
		if err := sysproxy.On("127.0.0.1", port); err != nil {
			log.Printf("[sysproxy] 主端口已改为 %d，但系统代理重绑失败: %v（可 proxyd sysproxy off 后重开）", port, err)
		} else {
			log.Printf("[sysproxy] 系统代理已跟随主端口重新指向 127.0.0.1:%d", port)
		}
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// SetSystemProxy 开关系统代理（指向主端口），立即应用并持久化配置。
func (a *App) SetSystemProxy(enabled bool) error {
	var err error
	if enabled {
		err = sysproxy.On("127.0.0.1", a.cfg.MixedPort)
	} else {
		err = sysproxy.Off()
	}
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg.SystemProxy = enabled
	err = a.persistLocked()
	a.mu.Unlock()
	return err
}

// SetAutostart 注册/移除开机自启项（OS 级状态，不写入配置文件）。
func (a *App) SetAutostart(enabled bool) error {
	if !enabled {
		return autostart.Off()
	}
	opt, err := a.autostartOptions()
	if err != nil {
		return err
	}
	return autostart.On(opt)
}

// AutostartStatus 报告自启项是否存在（查询失败按 false 处理）。
func (a *App) AutostartStatus() bool {
	on, err := autostart.Status()
	return err == nil && on
}

// autostartOptions 构建注册自启项的参数：二进制与配置文件均取绝对路径。
func (a *App) autostartOptions() (autostart.Options, error) {
	if a.cfgPath == "" {
		return autostart.Options{}, fmt.Errorf("无配置文件路径，无法注册自启（请先以配置文件方式运行）")
	}
	exe, err := os.Executable()
	if err != nil {
		return autostart.Options{}, err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return autostart.Options{}, err
	}
	cfgPath, err := filepath.Abs(a.cfgPath)
	if err != nil {
		return autostart.Options{}, err
	}
	return autostart.Options{Exe: exe, ConfigPath: cfgPath, StateDir: a.cfg.StateDir}, nil
}

// SetPortRange 修改节点映射端口区间并持久化，随后用当前节点
// （沿用最近一次测速结果，不重新拉订阅/测速）重新分配端口并热更新。
func (a *App) SetPortRange(lo, hi int) error {
	if lo <= 0 || hi > 65535 || lo > hi {
		return fmt.Errorf("invalid port range [%d, %d]", lo, hi)
	}
	a.mu.Lock()
	if a.cfg.MixedPort >= lo && a.cfg.MixedPort <= hi {
		a.mu.Unlock()
		return fmt.Errorf("新区间 [%d, %d] 与主端口 %d 冲突", lo, hi, a.cfg.MixedPort)
	}
	if a.cfg.AutoPort != 0 && a.cfg.AutoPort >= lo && a.cfg.AutoPort <= hi {
		a.mu.Unlock()
		return fmt.Errorf("新区间 [%d, %d] 与 auto-port %d 冲突", lo, hi, a.cfg.AutoPort)
	}
	for _, g := range a.cfg.Groups {
		if g.Port >= lo && g.Port <= hi {
			a.mu.Unlock()
			return fmt.Errorf("新区间 [%d, %d] 与分组 %q 端口 %d 冲突", lo, hi, g.Name, g.Port)
		}
	}
	old := a.cfg.PortRange
	a.cfg.PortRange = [2]int{lo, hi}
	a.mu.Unlock()
	if err := a.reallocate(); err != nil {
		a.mu.Lock()
		a.cfg.PortRange = old
		a.mu.Unlock()
		_ = a.reallocate()
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// reallocate 用当前节点列表重新分配端口、保存快照并热更新核心（无网络操作）。
func (a *App) reallocate() error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	var alive []*node.Node
	for _, n := range a.Nodes() {
		if n.Alive {
			alive = append(alive, n)
		}
	}
	prev, err := pool.LoadSnapshot(a.snapshotPath())
	if err != nil {
		log.Printf("[alloc] load snapshot: %v (ignored)", err)
	}
	a.mu.RLock()
	lo, hi := a.cfg.PortRange[0], a.cfg.PortRange[1]
	a.mu.RUnlock()
	assigns := pool.Allocate(alive, lo, hi, prev)
	snap := &pool.Snapshot{Mapping: make(map[string]int, len(assigns))}
	for _, as := range assigns {
		snap.Mapping[as.Node.Key()] = as.Port
	}
	if err := pool.SaveSnapshot(a.snapshotPath(), snap); err != nil {
		log.Printf("[alloc] save snapshot: %v", err)
	}

	if err := a.regenerateLocked(assigns); err != nil {
		return err
	}

	a.mu.Lock()
	a.assigns = assigns
	a.mu.Unlock()
	log.Printf("[alloc] port range changed: %d ports mapped to %d-%d", len(assigns), lo, hi)
	return nil
}

// Regenerate 仅按当前 cfg + 当前 assignments 重新生成并热应用 mihomo 配置
// （不拉订阅、不测速）。用于 auto-port/rules/groups 等变更后的热更新。
func (a *App) Regenerate() error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	return a.regenerateCurrentLocked()
}

// regenerateCurrentLocked 用当前 assigns 重新生成；调用方须已持有 refreshing 锁。
func (a *App) regenerateCurrentLocked() error {
	a.mu.RLock()
	assigns := make([]pool.Assignment, len(a.assigns))
	copy(assigns, a.assigns)
	a.mu.RUnlock()
	return a.regenerateLocked(assigns)
}

// regenerateLocked 是重新生成热更新的核心；调用方须已持有 refreshing 锁。
func (a *App) regenerateLocked(assigns []pool.Assignment) error {
	a.mu.RLock()
	cfg := a.cfg
	imported := a.mergedImportedLocked()
	a.mu.RUnlock()
	return a.regenerateWithLocked(cfg, assigns, imported)
}

// regenerateWithLocked 用指定配置生成并热应用；调用方须已持有 refreshing 锁。
// cfg 为运行时副本（不与 a.cfg 同步修改），供两阶段热更新等场景使用。
//
// 主端口形态转换保护：mihomo 热更新先 PatchInboundListeners（监听新 listener）
// 后 ReCreateMixed（关闭旧 mixed-port），主端口从顶层 mixed-port 直接换成同端口
// listener 会 bind 冲突把端口打挂。因此当目标配置的主端口是 listener 形态
// （main-auto/main-node 生效）而上次应用的不是时，先应用一版"主端口入口完全关闭"
// 的配置释放端口，再应用目标配置。反向（listener → mixed-port）以及
// listener 同名仅换 proxy 目标（main-auto ↔ main-node）由 mihomo 安全处理。
func (a *App) regenerateWithLocked(cfg *config.Config, assigns []pool.Assignment, imported []string) error {
	willListener := core.MainInboundIsListener(cfg, assigns)
	a.mu.RLock()
	wasListener := a.mainListenerOn
	a.mu.RUnlock()
	if willListener && !wasListener {
		phase := *cfg // 浅拷贝：Generate 只读
		phase.MainAuto = false
		phase.MainNode = ""
		phase.MixedPort = 0 // 生成 mixed-port: 0（mihomo 视为关闭该入口）
		if err := a.applyConfigLocked(&phase, assigns, imported); err != nil {
			// 释放失败不致命：继续尝试直接应用目标配置
			log.Printf("[app] 主端口形态切换：释放旧入口失败（继续应用目标配置）: %v", err)
		}
	}
	if err := a.applyConfigLocked(cfg, assigns, imported); err != nil {
		return err
	}
	a.mu.Lock()
	a.mainListenerOn = willListener
	a.mu.Unlock()
	return nil
}

// applyConfigLocked 生成并热应用一版 mihomo 配置；调用方须已持有 refreshing 锁。
func (a *App) applyConfigLocked(cfg *config.Config, assigns []pool.Assignment, imported []string) error {
	cfgYAML, err := core.Generate(cfg, assigns, imported)
	if err != nil {
		return fmt.Errorf("generate mihomo config: %w", err)
	}
	if err := a.runner.Reload(cfgYAML); err != nil {
		return fmt.Errorf("apply mihomo config: %w", err)
	}
	return nil
}

// mergedImportedLocked 合并全部规则源导入的规则：按 rule-urls 配置顺序，去重，
// 超出 ruleurl.MaxImportedRules 截断。调用方须已持有 a.mu（读锁）。
func (a *App) mergedImportedLocked() []string {
	var out []string
	seen := map[string]bool{}
	for _, ru := range a.cfg.RuleURLs {
		for _, r := range a.imported[ru.Name] {
			if seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
	}
	if len(out) > ruleurl.MaxImportedRules {
		log.Printf("[rules] 导入规则 %d 条超过上限 %d，截断", len(out), ruleurl.MaxImportedRules)
		out = out[:ruleurl.MaxImportedRules]
	}
	return out
}

// CustomRules 返回自定义规则快照（供 API 展示）。
func (a *App) CustomRules() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, len(a.cfg.CustomRules))
	copy(out, a.cfg.CustomRules)
	return out
}

// AddRule 追加一条自定义规则（生成时前置到内置规则之前），校验通过后持久化并热更新；
// mihomo 自检不通过时回滚并返回明确错误。
func (a *App) AddRule(rule string) error {
	rule = strings.TrimSpace(rule)
	if err := config.ValidateCustomRule(rule); err != nil {
		return err
	}
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.Lock()
	a.cfg.CustomRules = append(a.cfg.CustomRules, rule)
	a.mu.Unlock()
	if err := a.regenerateCurrentLocked(); err != nil {
		a.mu.Lock()
		a.cfg.CustomRules = a.cfg.CustomRules[:len(a.cfg.CustomRules)-1]
		a.mu.Unlock()
		_ = a.regenerateCurrentLocked()
		return fmt.Errorf("规则未通过 mihomo 校验: %w", err)
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// RemoveRule 按下标删除自定义规则，持久化并热更新。
func (a *App) RemoveRule(index int) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.Lock()
	if index < 0 || index >= len(a.cfg.CustomRules) {
		a.mu.Unlock()
		return fmt.Errorf("规则下标 %d 不存在", index)
	}
	removed := a.cfg.CustomRules[index]
	a.cfg.CustomRules = append(a.cfg.CustomRules[:index], a.cfg.CustomRules[index+1:]...)
	a.mu.Unlock()
	if err := a.regenerateCurrentLocked(); err != nil {
		a.mu.Lock()
		cr := append(a.cfg.CustomRules, "")
		copy(cr[index+1:], cr[index:])
		cr[index] = removed
		a.cfg.CustomRules = cr
		a.mu.Unlock()
		_ = a.regenerateCurrentLocked()
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// RuleURLs 返回规则源列表及最近拉取状态（供 API 展示）。
func (a *App) RuleURLs() []RuleURLStat {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]RuleURLStat, 0, len(a.cfg.RuleURLs))
	for _, ru := range a.cfg.RuleURLs {
		st := RuleURLStat{Name: ru.Name, URL: ru.URL}
		if s, ok := a.ruleStats[ru.Name]; ok {
			st.Count, st.Error, st.Warn = s.Count, s.Error, s.Warn
		}
		out = append(out, st)
	}
	return out
}

// AddRuleURL 新增规则源：持久化后立即拉取该源并热更新（失败仅记录状态，不报错）。
func (a *App) AddRuleURL(ru config.RuleURL) error {
	ru.Name = strings.TrimSpace(ru.Name)
	ru.URL = strings.TrimSpace(ru.URL)
	a.mu.Lock()
	if err := a.cfg.CheckRuleURL(ru); err != nil {
		a.mu.Unlock()
		return err
	}
	a.cfg.RuleURLs = append(a.cfg.RuleURLs, ru)
	a.mu.Unlock()

	// 立即拉取这一个源；失败只记入状态，规则等下一轮刷新再试
	res := ruleurl.Fetch(context.Background(), ru, a.cfg.StateDir)
	a.applyRuleResults([]ruleurl.Result{res})

	if err := a.Regenerate(); err != nil {
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// RemoveRuleURL 按名字删除规则源，移除其导入规则，持久化并热更新。
func (a *App) RemoveRuleURL(name string) error {
	a.mu.Lock()
	idx := -1
	for i, ru := range a.cfg.RuleURLs {
		if ru.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		a.mu.Unlock()
		return fmt.Errorf("规则源 %q 不存在", name)
	}
	a.cfg.RuleURLs = append(a.cfg.RuleURLs[:idx], a.cfg.RuleURLs[idx+1:]...)
	delete(a.imported, name)
	delete(a.ruleStats, name)
	a.mu.Unlock()
	if err := a.Regenerate(); err != nil {
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// RuleURLContent 返回规则源的原始文本（未解析）：优先读本地缓存，
// 缓存不存在时现场拉取一次（成功则写缓存）。供 API/CLI 查看原始内容。
func (a *App) RuleURLContent(name string) ([]byte, error) {
	a.mu.RLock()
	var ru config.RuleURL
	found := false
	for _, r := range a.cfg.RuleURLs {
		if r.Name == name {
			ru = r
			found = true
			break
		}
	}
	stateDir := a.cfg.StateDir
	a.mu.RUnlock()
	if !found {
		return nil, fmt.Errorf("规则源 %q 不存在", name)
	}
	return ruleurl.Content(context.Background(), ru, stateDir)
}

// applyRuleResults 记录规则源拉取结果（导入规则 + 状态），并打日志。
func (a *App) applyRuleResults(results []ruleurl.Result) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, res := range results {
		st := RuleURLStat{Name: res.Name, Count: len(res.Rules)}
		if res.Err != nil {
			st.Error = res.Err.Error()
			log.Printf("[rules] %v", res.Err)
		}
		if res.Warn != nil {
			st.Warn = res.Warn.Error()
			log.Printf("[rules] %v", res.Warn)
		}
		a.ruleStats[res.Name] = st
		if res.Err == nil {
			a.imported[res.Name] = res.Rules
		}
	}
}

// Groups 返回节点分组快照（供 API 展示）。
func (a *App) Groups() []config.NodeGroup {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]config.NodeGroup, len(a.cfg.Groups))
	copy(out, a.cfg.Groups)
	return out
}

// AddGroup 新增节点分组（一组节点 → 指定端口，组内自动选优），持久化并热更新。
func (a *App) AddGroup(g config.NodeGroup) error {
	g.Name = strings.TrimSpace(g.Name)
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.Lock()
	if err := a.cfg.CheckGroup(g); err != nil {
		a.mu.Unlock()
		return err
	}
	for _, n := range a.nodes {
		if n.Name == g.Name {
			a.mu.Unlock()
			return fmt.Errorf("分组名 %q 与节点名冲突", g.Name)
		}
	}
	a.cfg.Groups = append(a.cfg.Groups, g)
	a.mu.Unlock()
	if err := a.regenerateCurrentLocked(); err != nil {
		a.mu.Lock()
		a.cfg.Groups = a.cfg.Groups[:len(a.cfg.Groups)-1]
		a.mu.Unlock()
		_ = a.regenerateCurrentLocked()
		return fmt.Errorf("分组配置未通过 mihomo 校验: %w", err)
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// RemoveGroup 按名字删除节点分组，持久化并热更新。
func (a *App) RemoveGroup(name string) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.Lock()
	idx := -1
	for i, g := range a.cfg.Groups {
		if g.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		a.mu.Unlock()
		return fmt.Errorf("分组 %q 不存在", name)
	}
	removed := a.cfg.Groups[idx]
	a.cfg.Groups = append(a.cfg.Groups[:idx], a.cfg.Groups[idx+1:]...)
	a.mu.Unlock()
	if err := a.regenerateCurrentLocked(); err != nil {
		a.mu.Lock()
		gs := append(a.cfg.Groups, config.NodeGroup{})
		copy(gs[idx+1:], gs[idx:])
		gs[idx] = removed
		a.cfg.Groups = gs
		a.mu.Unlock()
		_ = a.regenerateCurrentLocked()
		return err
	}
	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	return err
}

// AddSubscription 添加订阅并持久化到配置文件；URL 去重，name 为空时自动命名。
// 调用方负责随后触发 Refresh。
func (a *App) AddSubscription(name, url string) (config.Subscription, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return config.Subscription{}, fmt.Errorf("订阅地址必须是 http(s) URL")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.cfg.Subscriptions {
		if s.URL == url {
			return s, fmt.Errorf("订阅地址已存在（%s）", s.Name)
		}
	}
	if name == "" {
		name = autoSubName(url, a.cfg.Subscriptions)
	} else {
		for _, s := range a.cfg.Subscriptions {
			if s.Name == name {
				return config.Subscription{}, fmt.Errorf("订阅名 %q 已存在", name)
			}
		}
	}
	sub := config.Subscription{Name: name, URL: url, Type: "auto"}
	a.cfg.Subscriptions = append(a.cfg.Subscriptions, sub)
	if err := a.persistLocked(); err != nil {
		return config.Subscription{}, err
	}
	return sub, nil
}

// RemoveSubscription 按名字删除订阅并持久化；不允许删除最后一个订阅。
func (a *App) RemoveSubscription(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, s := range a.cfg.Subscriptions {
		if s.Name == name {
			if len(a.cfg.Subscriptions) == 1 && len(a.cfg.ManualNodes) == 0 {
				return fmt.Errorf("不能删除最后一个订阅")
			}
			a.cfg.Subscriptions = append(a.cfg.Subscriptions[:i], a.cfg.Subscriptions[i+1:]...)
			return a.persistLocked()
		}
	}
	return fmt.Errorf("订阅 %q 不存在", name)
}

// ManualNodeEntry 是手动节点列表的展示项（供 API 返回）。
type ManualNodeEntry struct {
	Index int    `json:"index"`
	URL   string `json:"url"`
	Name  string `json:"name"` // 解析出的节点名（fragment/兜底），解析失败为空
}

// ManualNodes 返回配置中的手动节点列表（供 API 展示）。
func (a *App) ManualNodes() []ManualNodeEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]ManualNodeEntry, 0, len(a.cfg.ManualNodes))
	for i, u := range a.cfg.ManualNodes {
		out = append(out, ManualNodeEntry{Index: i, URL: u, Name: subscribe.ManualNodeName(u)})
	}
	return out
}

// AddManualNode 添加手动节点并持久化；name 非空且 URL 无 fragment 时附加为节点名。
// 重复 URL 被拒绝。调用方负责随后触发 Refresh。
func (a *App) AddManualNode(rawURL, name string) (ManualNodeEntry, error) {
	rawURL = strings.TrimSpace(rawURL)
	name = strings.TrimSpace(name)
	if rawURL == "" {
		return ManualNodeEntry{}, fmt.Errorf("节点 URL 不能为空")
	}
	if _, err := subscribe.ParseManualNode(rawURL); err != nil {
		return ManualNodeEntry{}, fmt.Errorf("节点 URL 解析失败: %w", err)
	}
	if name != "" && !strings.Contains(rawURL, "#") {
		rawURL += "#" + urlpkg.PathEscape(name)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.cfg.ManualNodes {
		if e == rawURL {
			return ManualNodeEntry{}, fmt.Errorf("节点 %q 已存在", rawURL)
		}
	}
	a.cfg.ManualNodes = append(a.cfg.ManualNodes, rawURL)
	if err := a.persistLocked(); err != nil {
		return ManualNodeEntry{}, err
	}
	return ManualNodeEntry{Index: len(a.cfg.ManualNodes) - 1, URL: rawURL, Name: subscribe.ManualNodeName(rawURL)}, nil
}

// RemoveManualNode 按下标删除手动节点并持久化。调用方负责随后触发 Refresh。
func (a *App) RemoveManualNode(index int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if index < 0 || index >= len(a.cfg.ManualNodes) {
		return fmt.Errorf("手动节点下标 %d 不存在", index)
	}
	if len(a.cfg.ManualNodes) == 1 && len(a.cfg.Subscriptions) == 0 {
		return fmt.Errorf("不能删除最后一个节点来源（已无订阅）")
	}
	a.cfg.ManualNodes = append(a.cfg.ManualNodes[:index], a.cfg.ManualNodes[index+1:]...)
	return a.persistLocked()
}

// persistLocked 把当前配置写回文件；调用方须已持有 a.mu。
func (a *App) persistLocked() error {
	if a.cfgPath == "" {
		return nil
	}
	if err := a.cfg.Save(a.cfgPath); err != nil {
		return fmt.Errorf("保存配置文件失败: %w", err)
	}
	return nil
}

// autoSubName 根据 URL 主机名生成不冲突的订阅名。
func autoSubName(url string, existing []config.Subscription) string {
	base := "sub"
	if u, err := urlpkg.Parse(url); err == nil && u.Hostname() != "" {
		base = u.Hostname()
	}
	taken := map[string]bool{}
	for _, s := range existing {
		taken[s.Name] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		if name := fmt.Sprintf("%s-%d", base, i); !taken[name] {
			return name
		}
	}
}

// Refresh 执行一轮完整流水线：fetch=true 时先拉取订阅与规则源，否则复用上次结果。
// 步骤：拉取/合并 → 健康检测 → 端口分配（稳定映射）→ 生成配置 → 热更新核心。
func (a *App) Refresh(ctx context.Context, fetch bool) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	var nodes []*node.Node
	if fetch || len(a.Nodes()) == 0 {
		a.mu.RLock()
		subs := make([]config.Subscription, len(a.cfg.Subscriptions))
		copy(subs, a.cfg.Subscriptions)
		ruleURLs := make([]config.RuleURL, len(a.cfg.RuleURLs))
		copy(ruleURLs, a.cfg.RuleURLs)
		manualEntries := make([]string, len(a.cfg.ManualNodes))
		copy(manualEntries, a.cfg.ManualNodes)
		a.mu.RUnlock()

		// 订阅与规则源并发拉取
		ruleCh := make(chan []ruleurl.Result, 1)
		go func() { ruleCh <- ruleurl.FetchAll(ctx, ruleURLs, a.cfg.StateDir) }()

		manual, manualErrs := subscribe.ParseManualNodes(manualEntries)
		for _, err := range manualErrs {
			if err != nil {
				log.Printf("[manual] %v", err)
			}
		}

		var errs []error
		nodes, errs = subscribe.FetchAll(ctx, subs, a.cfg.StateDir, a.excludeRe,
			map[string][]*node.Node{subscribe.ManualSubscription: manual})
		for _, err := range errs {
			if err != nil {
				log.Printf("[subscribe] %v", err)
			}
		}
		a.applyRuleResults(<-ruleCh)
		if len(nodes) == 0 {
			return fmt.Errorf("no nodes available from any subscription")
		}
		a.mu.Lock()
		a.nodes = nodes
		a.mu.Unlock()
	} else {
		nodes = a.Nodes()
	}

	pool.Check(ctx, nodes, a.cfg.HealthURL, a.cfg.HealthTimeout.D(), 32)

	alive := make([]*node.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Alive {
			alive = append(alive, n)
		}
	}
	if len(alive) == 0 {
		return fmt.Errorf("all %d nodes failed health check", len(nodes))
	}
	if capacity := a.cfg.Capacity(); len(alive) > capacity {
		log.Printf("[alloc] %d alive nodes exceed port capacity %d, keeping the fastest", len(alive), capacity)
	}

	prev, err := pool.LoadSnapshot(a.snapshotPath())
	if err != nil {
		log.Printf("[alloc] load snapshot: %v (ignored)", err)
	}
	assigns := pool.Allocate(alive, a.cfg.PortRange[0], a.cfg.PortRange[1], prev)
	snap := &pool.Snapshot{Mapping: make(map[string]int, len(assigns))}
	for _, as := range assigns {
		snap.Mapping[as.Node.Key()] = as.Port
	}
	if err := pool.SaveSnapshot(a.snapshotPath(), snap); err != nil {
		log.Printf("[alloc] save snapshot: %v", err)
	}

	if err := a.regenerateLocked(assigns); err != nil {
		return err
	}

	a.mu.Lock()
	a.assigns = assigns
	a.mu.Unlock()
	if err := node.SaveSnapshot(a.nodesSnapshotPath(), nodes); err != nil {
		log.Printf("[snapshot] 保存节点快照失败: %v", err)
	}
	log.Printf("[refresh] done: %d nodes, %d alive, %d ports mapped", len(nodes), len(alive), len(assigns))
	return nil
}

// Run 启动调度器：先加载节点快照立即提供服务，再执行一轮完整刷新，
// 之后按配置周期刷新订阅与检测健康。阻塞直到 ctx 取消。
func (a *App) Run(ctx context.Context) error {
	a.restoreSnapshot()
	if err := a.Refresh(ctx, true); err != nil {
		log.Printf("[refresh] initial refresh failed: %v", err)
	}

	refreshTick := time.NewTicker(a.cfg.RefreshInterval.D())
	healthTick := time.NewTicker(a.cfg.HealthInterval.D())
	defer refreshTick.Stop()
	defer healthTick.Stop()

	for {
		select {
		case <-ctx.Done():
			a.runner.Shutdown()
			return nil
		case <-refreshTick.C:
			if err := a.Refresh(ctx, true); err != nil {
				log.Printf("[refresh] %v", err)
			}
		case <-healthTick.C:
			if err := a.Refresh(ctx, false); err != nil {
				log.Printf("[health] %v", err)
			}
		}
	}
}

// Shutdown 关闭内嵌的 mihomo 核心。
func (a *App) Shutdown() { a.runner.Shutdown() }

func (a *App) snapshotPath() string {
	return a.cfg.StateDir + "/mapping.json"
}

func (a *App) nodesSnapshotPath() string {
	return a.cfg.StateDir + "/nodes.json"
}

// restoreSnapshot 启动时加载 nodes.json 节点快照并立即生成 mihomo 配置提供服务，
// 不必等首次订阅刷新完成。快照缺失/损坏仅打日志丢弃，不致命。
// 之后 Run 里的首次 Refresh 成功会覆盖；失败则快照保持可用。
func (a *App) restoreSnapshot() {
	snap, err := node.LoadSnapshot(a.nodesSnapshotPath())
	if err != nil {
		log.Printf("[snapshot] %v", err)
		return
	}
	if snap == nil || len(snap.Nodes) == 0 {
		return
	}
	var alive []*node.Node
	for _, n := range snap.Nodes {
		if n.Alive {
			alive = append(alive, n)
		}
	}
	if len(alive) == 0 {
		log.Printf("[snapshot] 快照 %d 个节点均标记失效，等待首次刷新", len(snap.Nodes))
		return
	}

	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	prev, err := pool.LoadSnapshot(a.snapshotPath())
	if err != nil {
		log.Printf("[alloc] load snapshot: %v (ignored)", err)
	}
	assigns := pool.Allocate(alive, a.cfg.PortRange[0], a.cfg.PortRange[1], prev)
	if err := a.regenerateLocked(assigns); err != nil {
		log.Printf("[snapshot] 快照节点生成配置失败（等待首次刷新）: %v", err)
		return
	}
	a.mu.Lock()
	a.nodes = snap.Nodes
	a.assigns = assigns
	a.mu.Unlock()
	log.Printf("[snapshot] 已从快照恢复 %d 个节点（%d 个可用，%d 个端口，保存于 %s）",
		len(snap.Nodes), len(alive), len(assigns), snap.SavedAt.Format("2006-01-02 15:04:05"))
}
