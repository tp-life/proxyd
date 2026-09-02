// Package app 编排 proxyd 的订阅刷新、健康检测、端口分配和 mihomo 核心热更新用例。
package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
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
	"golang.org/x/mod/semver"

	"proxyd/internal/autostart"
	"proxyd/internal/config"
	"proxyd/internal/core"
	"proxyd/internal/node"
	"proxyd/internal/pool"
	"proxyd/internal/ruleurl"
	"proxyd/internal/subscribe"
	"proxyd/internal/sysproxy"
	"proxyd/internal/tunperm"
)

// RuleURLStat 是规则源的最近一次拉取状态（供 API 展示）。
type RuleURLStat struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Count int    `json:"count"`
	Error string `json:"error,omitempty"` // 拉取与缓存都失败
	Warn  string `json:"warn,omitempty"`  // 拉取失败但降级用了缓存
}

// TUNStatus 是 TUN 运行状态与当前进程权限状态的应用层只读视图。
//
// API 和 CLI 只依赖该视图，不直接读取操作系统权限适配器，从而避免展示层承担
// TUN 权限规则或拼接平台相关指引。
type TUNStatus struct {
	Enabled    bool   `json:"enabled"`
	Active     bool   `json:"active"`
	Allowed    bool   `json:"allowed"`
	Platform   string `json:"platform"`
	Permission string `json:"permission,omitempty"`
}

// ConfigCountChange 描述配置导入前后某类对象数量的变化，不包含 URL、secret 等敏感值。
type ConfigCountChange struct {
	Before int `json:"before"`
	After  int `json:"after"`
}

// ConfigImportPreview 是配置导入预检结果。
//
// Digest 绑定用户确认时看到的原始字节；Counts 和 ChangedFields 只提供影响摘要，
// 不回显订阅 URL、节点凭据、controller secret 等敏感内容。
type ConfigImportPreview struct {
	Digest          string                       `json:"digest"`
	RestartRequired bool                         `json:"restart_required"`
	Counts          map[string]ConfigCountChange `json:"counts"`
	ChangedFields   []string                     `json:"changed_fields"`
	Warnings        []string                     `json:"warnings"`
}

// LatestRelease 是版本检查端口返回的最新稳定版本元数据。
//
// 该值属于应用层合约；基础设施适配器负责从 GitHub Releases 获取并转换，
// API 和 Web 不直接依赖 GitHub 响应结构。
type LatestRelease struct {
	Version     string
	URL         string
	PublishedAt time.Time
}

// ReleaseChecker 定义应用层查询最新稳定版本所需的最小端口。
//
// 基础设施实现必须尊重 context 取消与超时；网络失败只作为 error 返回，不能终止代理服务。
type ReleaseChecker interface {
	Latest(ctx context.Context) (LatestRelease, error)
}

const (
	// VersionCheckDisabled 表示用户已关闭启动版本检查。
	VersionCheckDisabled = "disabled"
	// VersionCheckPending 表示检查器已配置、后台任务尚未开始。
	VersionCheckPending = "pending"
	// VersionCheckChecking 表示后台 HTTP 请求正在进行。
	VersionCheckChecking = "checking"
	// VersionCheckCurrent 表示当前版本不低于最新稳定版本。
	VersionCheckCurrent = "current"
	// VersionCheckAvailable 表示存在可下载的新稳定版本。
	VersionCheckAvailable = "available"
	// VersionCheckUnsupported 表示当前构建版本不是可可靠比较的语义化版本。
	VersionCheckUnsupported = "unsupported"
	// VersionCheckFailed 表示本次检查失败，但不影响代理功能。
	VersionCheckFailed = "failed"
)

// VersionCheckStatus 是版本检查的应用层只读状态。
//
// 状态由 App 缓存，overview 轮询只读取内存，不会把前端的十秒轮询放大为 GitHub 请求。
type VersionCheckStatus struct {
	Enabled   bool   `json:"enabled"`
	State     string `json:"state"`
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	URL       string `json:"url,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
	Message   string `json:"message,omitempty"`
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
	subInfos  map[string]subscribe.UserInfo

	includeRe  *regexp.Regexp
	excludeRe  *regexp.Regexp
	refreshing sync.Mutex // 保证刷新流水线串行执行

	// mainListenerOn 记录最近一次成功应用的配置里主端口是否为固定 listener 形态
	// （main-auto/main-node 生效）；用于 regenerateWithLocked 判断是否需要
	// 先释放主端口（mixed-port → 同端口 listener 直接热更新会 bind 冲突）。
	mainListenerOn bool

	updateChecker          ReleaseChecker
	versionStatus          VersionCheckStatus
	versionCheckGeneration uint64
	versionCheckRunning    uint64
}

// New 创建 App，并在配置已开启 TUN 时提前校验当前进程权限。
//
// 参数：
//   - cfg: *config.Config，已经完成默认值和结构校验的运行配置。
//   - cfgPath: string，配置变更时持久化使用的文件路径；空值表示只在内存中运行。
//
// 返回值：
//   - *App：初始化完成的应用编排器。
//   - error：exclude 正则无效或配置要求启用 TUN 但进程权限不足时返回错误。
//
// 错误情况：权限错误包含 macOS sudo、Linux setcap 或 Windows 管理员操作指引，
// 让服务在创建 API 和修改系统路由之前失败，避免出现“控制台已启动但 TUN 不工作”的半运行状态。
func New(cfg *config.Config, cfgPath string) (*App, error) {
	// app.New 既接收 config.Load 的结果，也被 e2e/嵌入调用方直接使用。
	// 这里补一次幂等默认值，保证后续持久化和状态 API 都看到完整 TUN 配置。
	cfg.TUN.ApplyDefaults()
	if cfg.TUN.Enable {
		if err := tunperm.Require(); err != nil {
			return nil, fmt.Errorf("配置已开启 TUN，但当前进程无法创建 TUN 设备: %w", err)
		}
	}
	a := &App{
		cfg:       cfg,
		cfgPath:   cfgPath,
		runner:    core.NewRunner(cfg.StateDir),
		imported:  map[string][]string{},
		ruleStats: map[string]RuleURLStat{},
		subInfos:  map[string]subscribe.UserInfo{},
	}
	if cfg.Exclude != "" {
		re, err := regexp.Compile(cfg.Exclude)
		if err != nil {
			return nil, fmt.Errorf("compile exclude regexp: %w", err)
		}
		a.excludeRe = re
	}
	if cfg.Include != "" {
		re, err := regexp.Compile(cfg.Include)
		if err != nil {
			return nil, fmt.Errorf("compile include regexp: %w", err)
		}
		a.includeRe = re
	}
	return a, nil
}

// ConfigureUpdateCheck 注入构建版本和 GitHub Releases 基础设施适配器。
//
// 参数：
//   - current: string，由构建 ldflags 注入的当前版本；开发构建通常为 dev。
//   - checker: ReleaseChecker，查询最新稳定版本的基础设施实现；可为 nil 以禁用外部查询。
//
// 返回值：无；只初始化内存状态，真正的网络请求由 Run 异步启动。
//
// 错误情况：无；缺少 checker 或版本不可比较会在状态中降级，不影响服务启动。
func (a *App) ConfigureUpdateCheck(current string, checker ReleaseChecker) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updateChecker = checker
	a.versionCheckGeneration++
	a.versionCheckRunning = 0
	a.versionStatus = VersionCheckStatus{
		Enabled: a.cfg.UpdateCheckEnabled(),
		State:   VersionCheckPending,
		Current: current,
	}
	if !a.versionStatus.Enabled {
		a.versionStatus.State = VersionCheckDisabled
	}
}

// VersionStatus 返回版本检查状态快照。
//
// 参数：无。
//
// 返回值：
//   - VersionCheckStatus：当前检查开关、阶段和可选最新版本信息。
//
// 错误情况：无；未调用 ConfigureUpdateCheck 时返回按配置推导的 pending/disabled 状态。
func (a *App) VersionStatus() VersionCheckStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	status := a.versionStatus
	if status.State == "" {
		status.Enabled = a.cfg.UpdateCheckEnabled()
		if status.Enabled {
			status.State = VersionCheckPending
		} else {
			status.State = VersionCheckDisabled
		}
	}
	return status
}

// SetUpdateCheck 切换启动版本检查并持久化配置。
//
// 参数：
//   - enabled: bool，true 表示启用并立即异步检查一次，false 表示关闭并忽略在途结果。
//
// 返回值：
//   - error：配置持久化失败时返回错误并恢复原状态。
//
// 错误情况：启用后的网络失败不会从本方法返回，而会记录日志并更新为 failed 状态；
// 这样本地设置操作和代理主流程都不依赖 GitHub 可用性。
func (a *App) SetUpdateCheck(enabled bool) error {
	a.mu.Lock()
	oldSetting := a.cfg.CheckUpdates
	oldStatus := a.versionStatus
	oldGeneration := a.versionCheckGeneration
	a.cfg.CheckUpdates = new(bool)
	*a.cfg.CheckUpdates = enabled
	a.versionCheckGeneration++
	a.versionCheckRunning = 0
	a.versionStatus.Enabled = enabled
	a.versionStatus.Latest = ""
	a.versionStatus.URL = ""
	a.versionStatus.CheckedAt = ""
	a.versionStatus.Message = ""
	if enabled {
		a.versionStatus.State = VersionCheckPending
	} else {
		a.versionStatus.State = VersionCheckDisabled
	}
	if err := a.persistLocked(); err != nil {
		a.cfg.CheckUpdates = oldSetting
		a.versionStatus = oldStatus
		a.versionCheckGeneration = oldGeneration
		a.mu.Unlock()
		return err
	}
	a.mu.Unlock()
	if enabled {
		go a.startVersionCheck(context.Background())
	}
	return nil
}

// startVersionCheck 在独立任务中执行至多一次版本查询并更新缓存状态。
//
// 参数：
//   - ctx: context.Context，服务退出时取消启动检查；设置页触发时由内部十秒超时兜底。
//
// 返回值：无；查询结果写入 VersionCheckStatus。
//
// 错误情况：关闭检查、重复任务、未注入 checker 或不可比较版本会直接降级；HTTP、
// JSON、限流和超时错误只记录日志并写 failed，不向 Run 返回 fatal error。
func (a *App) startVersionCheck(ctx context.Context) {
	a.mu.Lock()
	if !a.cfg.UpdateCheckEnabled() || a.updateChecker == nil {
		a.mu.Unlock()
		return
	}
	generation := a.versionCheckGeneration
	if a.versionCheckRunning == generation {
		a.mu.Unlock()
		return
	}
	currentRaw := a.versionStatus.Current
	current := normalizeBuildVersion(currentRaw)
	if current == "" {
		a.versionStatus.State = VersionCheckUnsupported
		a.versionStatus.Message = "当前构建版本不可比较，已跳过更新检查"
		a.mu.Unlock()
		return
	}
	checker := a.updateChecker
	a.versionCheckRunning = generation
	a.versionStatus.State = VersionCheckChecking
	a.versionStatus.Message = ""
	a.mu.Unlock()

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	latest, err := checker.Latest(checkCtx)
	cancel()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.versionCheckRunning == generation {
		a.versionCheckRunning = 0
	}
	if generation != a.versionCheckGeneration || !a.cfg.UpdateCheckEnabled() {
		return
	}
	a.versionStatus.CheckedAt = time.Now().Format(time.RFC3339)
	if err != nil {
		a.versionStatus.State = VersionCheckFailed
		a.versionStatus.Message = "版本检查失败，不影响代理功能"
		log.Printf("[update-check] 检查失败: %v", err)
		return
	}
	latestVersion := normalizeBuildVersion(latest.Version)
	if latestVersion == "" {
		a.versionStatus.State = VersionCheckFailed
		a.versionStatus.Message = "远端版本格式无法识别"
		log.Printf("[update-check] 远端版本格式无法识别: %q", latest.Version)
		return
	}
	a.versionStatus.Latest = latest.Version
	a.versionStatus.URL = latest.URL
	if semver.Compare(latestVersion, current) > 0 {
		a.versionStatus.State = VersionCheckAvailable
		a.versionStatus.Message = "发现新版本"
		return
	}
	a.versionStatus.State = VersionCheckCurrent
	a.versionStatus.Message = "当前已是最新版本"
}

// normalizeBuildVersion 把 release tag 或 git describe 输出规范化为可比较的 Go semver。
//
// 参数：
//   - raw: string，可能为 v1.2.3、v1.2.3-4-gabcdef、1.2.3、dev 或提交哈希。
//
// 返回值：
//   - string：规范化的 v 前缀 semver；无法可靠判断时返回空字符串。
//
// 错误情况：裸提交哈希和 dev 不携带基准版本，必须返回空值而不是做字典序误判；
// git describe 的提交距离形式按最近 tag 比较，避免把同一 tag 后的提交误判为落后于该 tag。
func normalizeBuildVersion(raw string) string {
	candidate := strings.TrimSpace(raw)
	describe := regexp.MustCompile(`^(v?[0-9]+\.[0-9]+\.[0-9]+)-[0-9]+-g[0-9a-fA-F]+(?:-dirty)?$`)
	if match := describe.FindStringSubmatch(candidate); len(match) == 2 {
		candidate = match[1]
	} else {
		candidate = strings.TrimSuffix(candidate, "-dirty")
	}
	if candidate != "" && candidate[0] >= '0' && candidate[0] <= '9' {
		candidate = "v" + candidate
	}
	if !semver.IsValid(candidate) {
		return ""
	}
	return semver.Canonical(candidate)
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

// SubscriptionUserInfos 返回订阅流量/到期信息快照。
//
// 参数：无。
//
// 返回值：
//   - map[string]subscribe.UserInfo: 订阅名到用量信息的映射；调用方可自由修改返回 map。
//
// 错误情况：无；没有用量信息的订阅不会出现在返回 map 中。
func (a *App) SubscriptionUserInfos() map[string]subscribe.UserInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]subscribe.UserInfo, len(a.subInfos))
	for name, info := range a.subInfos {
		out[name] = info
	}
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

// TUNStatus 返回 TUN 配置状态以及当前进程的权限检测结果。
//
// 参数：无。
//
// 返回值：
//   - TUNStatus：Enabled 来自当前配置；Allowed/Platform/Permission 来自操作系统适配器。
//
// 错误情况：无；底层无法读取权限状态时按不允许处理，并通过 Permission 返回修复指引。
func (a *App) TUNStatus() TUNStatus {
	a.mu.RLock()
	enabled := a.cfg.TUN.Enable
	a.mu.RUnlock()
	permission := tunperm.Current()
	return TUNStatus{
		Enabled:    enabled,
		Active:     a.runner.TUNEnabled(),
		Allowed:    permission.Allowed,
		Platform:   permission.Platform,
		Permission: permission.Hint,
	}
}

// SetTUN 开关 TUN 模式，并按“权限检查 → 热更新 → 持久化”的顺序应用变更。
//
// 参数：
//   - enabled: bool，true 表示让 mihomo 创建 TUN 设备并接管系统流量，false 表示关闭。
//
// 返回值：
//   - error：权限不足、mihomo 配置生成/热更新失败或配置文件持久化失败时返回错误。
//
// 错误情况：开启前必须通过平台权限检测；热更新失败时会恢复旧 TUN 配置并尝试
// 再应用旧运行配置。回滚失败不会覆盖原始错误，runner 会同时记录底层失败日志。
func (a *App) SetTUN(enabled bool) error {
	if enabled {
		if err := tunperm.Require(); err != nil {
			return err
		}
	}

	a.mu.Lock()
	if a.cfg.TUN.Enable == enabled {
		a.mu.Unlock()
		return nil
	}
	old := a.cfg.TUN.Clone()
	a.cfg.TUN.ApplyDefaults()
	a.cfg.TUN.Enable = enabled
	a.mu.Unlock()

	if err := a.Regenerate(); err != nil {
		return a.rollbackTUN(old, err)
	}

	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	if err != nil {
		return a.rollbackTUN(old, err)
	}
	return nil
}

// rollbackTUN 恢复 TUN 配置并重新应用旧的 mihomo 运行态。
//
// 参数：
//   - old: config.TUNConfig，切换前的完整 TUN 配置副本。
//   - cause: error，触发回滚的生成、热更新或持久化错误。
//
// 返回值：
//   - error：始终保留原始 cause；运行态恢复也失败时通过 errors.Join 同时返回两者。
//
// 错误情况：mihomo 的 TUN 创建错误可能只写日志，因此回滚同样经过 Regenerate 的
// listener 实际状态校验；不能静默宣称已经恢复。
func (a *App) rollbackTUN(old config.TUNConfig, cause error) error {
	a.mu.Lock()
	a.cfg.TUN = old
	a.mu.Unlock()
	if rollbackErr := a.Regenerate(); rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("恢复旧 TUN 运行态失败: %w", rollbackErr))
	}
	return cause
}

// SetDNSPreset 切换 proxyd 生成的 DNS 预设，并热更新 mihomo 配置。
//
// 参数：
//   - preset: string，仅允许 off、fake-ip 或 redir-host；大小写和首尾空白会被规范化。
//
// 返回值：
//   - error：枚举无效、mihomo 生成/热更新失败或配置持久化失败时返回错误。
//
// 错误情况：手写 cfg.DNS 非空时仍保存预设但不改变当前生成结果，因为手写 DNS 的
// 优先级更高；热更新失败会恢复旧预设并尝试重新应用旧配置。
func (a *App) SetDNSPreset(preset string) error {
	preset = strings.ToLower(strings.TrimSpace(preset))
	switch preset {
	case config.DNSPresetOff, config.DNSPresetFakeIP, config.DNSPresetRedirHost:
	default:
		return fmt.Errorf("invalid dns-preset %q (off|fake-ip|redir-host)", preset)
	}

	a.mu.Lock()
	if a.cfg.DNSPreset == preset {
		a.mu.Unlock()
		return nil
	}
	old := a.cfg.DNSPreset
	a.cfg.DNSPreset = preset
	a.mu.Unlock()

	if err := a.Regenerate(); err != nil {
		return a.rollbackDNSPreset(old, err)
	}

	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	if err != nil {
		return a.rollbackDNSPreset(old, err)
	}
	return nil
}

// rollbackDNSPreset 恢复旧 DNS 预设并重新应用此前的 mihomo DNS 行为。
//
// 参数：
//   - old: string，切换前的 dns-preset。
//   - cause: error，触发回滚的热更新或持久化错误。
//
// 返回值：
//   - error：原始错误；回滚也失败时同时包含回滚错误。
//
// 错误情况：回滚生成失败会通过 errors.Join 暴露，避免 UI 收到失败后运行态仍悄悄
// 使用新 DNS 预设，尤其避免 TUN 场景出现难以定位的解析分裂。
func (a *App) rollbackDNSPreset(old string, cause error) error {
	a.mu.Lock()
	a.cfg.DNSPreset = old
	a.mu.Unlock()
	if rollbackErr := a.Regenerate(); rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("恢复旧 DNS 预设失败: %w", rollbackErr))
	}
	return cause
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

// SetPortMapping 开关健康节点的一对一本地端口 listener，并以“热更新成功后再持久化”
// 的事务顺序提交。端口分配快照和 assignments 始终保留，因此重新开启时仍能恢复稳定端口。
//
// 参数：
//   - enabled: bool，true 表示创建每个健康节点对应的 mixed listener，false 表示只停用这些入口。
//
// 返回值：
//   - error：mihomo 配置生成/热更新失败或配置文件持久化失败时返回；成功返回 nil。
//
// 错误情况：任何失败都会恢复旧内存配置并重新应用旧运行态；若磁盘提交阶段失败，
// 还会尝试把旧配置重新写回。回滚中的多个错误使用 errors.Join 合并，避免掩盖原始原因。
func (a *App) SetPortMapping(enabled bool) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	a.mu.Lock()
	if a.cfg.PortMappingEnabled() == enabled {
		a.mu.Unlock()
		return nil
	}
	old := a.cfg.PortMapping
	a.cfg.PortMapping = new(bool)
	*a.cfg.PortMapping = enabled
	a.mu.Unlock()

	if err := a.regenerateCurrentLocked(); err != nil {
		return a.rollbackPortMappingLocked(old, err, false)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return a.rollbackPortMappingLocked(old, persistErr, true)
	}
	return nil
}

// rollbackPortMappingLocked 恢复端口映射开关对应的配置、运行态和可选磁盘状态。
// 调用方必须持有 refreshing 锁，确保回滚期间不会被订阅刷新或其它热更新穿插。
//
// 参数：
//   - old: *bool，变更前的原始指针；nil 表示沿用向后兼容的默认开启语义。
//   - cause: error，触发回滚的原始热更新或持久化错误。
//   - restoreDisk: bool，true 表示新配置已经尝试落盘，需要额外重写旧配置以消除不确定状态。
//
// 返回值：
//   - error：始终包含 cause；运行态或磁盘恢复失败时通过 errors.Join 一并返回。
//
// 错误情况：回滚失败不会被吞掉，API/CLI 可以明确告知用户人工检查运行态和配置文件。
func (a *App) rollbackPortMappingLocked(old *bool, cause error, restoreDisk bool) error {
	a.mu.Lock()
	a.cfg.PortMapping = old
	a.mu.Unlock()

	joined := cause
	if rollbackErr := a.regenerateCurrentLocked(); rollbackErr != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复旧端口映射运行态失败: %w", rollbackErr))
	}
	if restoreDisk {
		a.mu.Lock()
		rollbackErr := a.persistLocked()
		a.mu.Unlock()
		if rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复旧端口映射配置文件失败: %w", rollbackErr))
		}
	}
	return joined
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

// applyConfigLocked 生成并热应用一版 mihomo 配置。
//
// 参数：
//   - cfg: *config.Config，本次需要应用的运行配置副本。
//   - assigns: []pool.Assignment，需要创建本地 listener 的节点端口映射。
//   - imported: []string，已经合并清洗的远程规则。
//
// 返回值：error，生成、自检、mihomo 热加载或 TUN 实际状态不一致时返回。
//
// 错误情况：调用方必须持有 refreshing 锁，确保读取节点快照和 Reload 期间没有另一轮
// 刷新并发修改。生成时同时传入完整健康节点集，使 dialer-proxy 依赖和策略组成员即使
// 没有独立本地端口，也会作为 proxy-only 出站注册到 mihomo。
func (a *App) applyConfigLocked(cfg *config.Config, assigns []pool.Assignment, imported []string) error {
	cfgYAML, err := core.GenerateWithNodes(cfg, assigns, a.Nodes(), imported)
	if err != nil {
		return fmt.Errorf("generate mihomo config: %w", err)
	}
	if err := a.runner.Reload(cfgYAML); err != nil {
		return fmt.Errorf("apply mihomo config: %w", err)
	}
	// mihomo 的 ReCreateTun 在创建虚拟网卡失败时只写日志，不把错误返回给 hub.Parse。
	// 因此必须在 Reload 返回后读取 listener 实际状态，否则可能把 enable:true 持久化，
	// 但运行时 TUN 已关闭。状态不一致作为应用失败返回，上层会恢复旧配置。
	if active := a.runner.TUNEnabled(); active != cfg.TUN.Enable {
		return fmt.Errorf("mihomo TUN 实际状态与请求不一致（期望 enable=%t，实际 active=%t）；请检查 TUN 日志、stack 与系统权限", cfg.TUN.Enable, active)
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

// AddRule 追加一条自定义规则（生成时前置到远程规则与内置规则之前）。
//
// 参数：
//   - rule: string，Clash 规则文本；首尾空白会被移除。
//
// 返回值：error，规则非法、mihomo 热更新、持久化或回滚失败时返回。
//
// 错误情况：通过统一事务助手提交，任何失败都会恢复旧规则与旧运行态。
func (a *App) AddRule(rule string) error {
	rule = strings.TrimSpace(rule)
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.RLock()
	next := append([]string(nil), a.cfg.CustomRules...)
	a.mu.RUnlock()
	next = append(next, rule)
	return a.commitCustomRulesLocked(next)
}

// RemoveRule 按下标删除一条可编辑的 custom-rules 规则。
//
// 参数：
//   - index: int，自定义规则的零基下标；远程规则和内置规则不在该索引空间内。
//
// 返回值：error，下标不存在、热更新、持久化或回滚失败时返回。
//
// 错误情况：删除只作用于 custom-rules，不会修改 rule-url 内容或内置 rules。
func (a *App) RemoveRule(index int) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.RLock()
	if index < 0 || index >= len(a.cfg.CustomRules) {
		a.mu.RUnlock()
		return fmt.Errorf("规则下标 %d 不存在", index)
	}
	next := append([]string(nil), a.cfg.CustomRules...)
	a.mu.RUnlock()
	next = append(next[:index], next[index+1:]...)
	return a.commitCustomRulesLocked(next)
}

// UpdateRule 原位替换一条自定义规则，保留其优先级位置。
//
// 参数：
//   - index: int，自定义规则的零基下标。
//   - rule: string，新的 Clash 规则文本。
//
// 返回值：error，下标/规则非法、热更新、持久化或回滚失败时返回。
//
// 错误情况：新规则必须先通过静态校验和 mihomo 完整配置自检，失败不会覆盖旧规则。
func (a *App) UpdateRule(index int, rule string) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.RLock()
	if index < 0 || index >= len(a.cfg.CustomRules) {
		a.mu.RUnlock()
		return fmt.Errorf("规则下标 %d 不存在", index)
	}
	next := append([]string(nil), a.cfg.CustomRules...)
	a.mu.RUnlock()
	next[index] = strings.TrimSpace(rule)
	return a.commitCustomRulesLocked(next)
}

// MoveRule 调整一条自定义规则的优先级位置。
//
// 参数：
//   - from: int，规则当前零基下标。
//   - to: int，移动完成后的目标零基下标。
//
// 返回值：error，下标非法、热更新、持久化或回滚失败时返回。
//
// 错误情况：仅重排 custom-rules；远程与内置规则保持各自既定顺序和只读语义。
func (a *App) MoveRule(from, to int) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.RLock()
	if from < 0 || from >= len(a.cfg.CustomRules) || to < 0 || to >= len(a.cfg.CustomRules) {
		a.mu.RUnlock()
		return fmt.Errorf("规则移动下标无效：from=%d to=%d", from, to)
	}
	next := append([]string(nil), a.cfg.CustomRules...)
	a.mu.RUnlock()
	if from == to {
		return nil
	}
	moved := next[from]
	next = append(next[:from], next[from+1:]...)
	next = append(next, "")
	copy(next[to+1:], next[to:])
	next[to] = moved
	return a.commitCustomRulesLocked(next)
}

// commitCustomRulesLocked 校验并提交完整 custom-rules 候选列表。
// 调用方必须持有 refreshing 锁，以串行化规则、刷新和核心热更新。
//
// 参数：
//   - next: []string，目标自定义规则列表；函数会复制后再写入配置。
//
// 返回值：error，静态校验、mihomo 自检/热更新、持久化或回滚失败时返回。
//
// 错误情况：持久化失败时既恢复内存和运行态，也尝试重写旧磁盘配置；多重失败合并返回。
func (a *App) commitCustomRulesLocked(next []string) error {
	for index, rule := range next {
		if err := config.ValidateCustomRule(strings.TrimSpace(rule)); err != nil {
			return fmt.Errorf("custom-rules[%d]: %w", index, err)
		}
		next[index] = strings.TrimSpace(rule)
	}
	a.mu.Lock()
	old := append([]string(nil), a.cfg.CustomRules...)
	a.cfg.CustomRules = append([]string(nil), next...)
	a.mu.Unlock()
	if err := a.regenerateCurrentLocked(); err != nil {
		return a.rollbackCustomRulesLocked(old, fmt.Errorf("规则未通过 mihomo 校验: %w", err), false)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return a.rollbackCustomRulesLocked(old, persistErr, true)
	}
	return nil
}

// rollbackCustomRulesLocked 恢复规则事务开始前的配置、运行态和可选磁盘状态。
// 调用方必须持有 refreshing 锁。
//
// 参数：
//   - old: []string，事务前的 custom-rules 副本。
//   - cause: error，触发回滚的原始错误。
//   - restoreDisk: bool，是否额外重写旧配置文件。
//
// 返回值：error，始终包含 cause；回滚失败时合并返回全部错误。
//
// 错误情况：运行态或磁盘恢复失败不会被吞掉，避免 UI 错误提示与实际规则不一致。
func (a *App) rollbackCustomRulesLocked(old []string, cause error, restoreDisk bool) error {
	a.mu.Lock()
	a.cfg.CustomRules = old
	a.mu.Unlock()
	joined := cause
	if rollbackErr := a.regenerateCurrentLocked(); rollbackErr != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复旧自定义规则运行态失败: %w", rollbackErr))
	}
	if restoreDisk {
		a.mu.Lock()
		rollbackErr := a.persistLocked()
		a.mu.Unlock()
		if rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复旧自定义规则配置文件失败: %w", rollbackErr))
		}
	}
	return joined
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

// RuleURLContent 返回规则源适合直接阅读的内容。
//
// 参数：
//   - name: string，配置中的规则源唯一名称。
//
// 返回值：
//   - []byte：普通规则源保持原文，整体 Base64 gfwlist 返回解码后的 AutoProxy 文本。
//   - error：规则源不存在，或没有缓存且现场拉取失败时返回错误。
//
// 错误情况：
// 查找规则源时只持有读锁；真正的缓存读取和网络拉取在释放锁后执行，避免慢 I/O 阻塞
// 其他配置操作。解码仅影响展示返回值，不会改写配置或原始缓存。
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
	g.Type = strings.TrimSpace(g.Type)
	g.Subscription = strings.TrimSpace(g.Subscription)
	if g.Type == "" {
		g.Type = config.GroupTypeURLTest
	}
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

// UpdateGroup 原位修改一个策略分组的端口、策略和成员来源，并作为单个事务热更新与持久化。
//
// 参数：
//   - currentName: string，路径中现有分组名称，用于稳定定位待更新实体。
//   - next: config.NodeGroup，目标分组值；本用例暂不允许改名，避免破坏订阅节点中可能存在的 dialer-proxy 引用。
//
// 返回值：error，分组不存在、字段/端口冲突、mihomo 热更新、持久化或回滚失败时返回。
//
// 错误情况：任何提交步骤失败都会恢复事务前的完整 Groups 列表和运行态；持久化已经
// 尝试但失败时还会重写旧磁盘配置，回滚错误通过 errors.Join 与原始错误一并返回。
func (a *App) UpdateGroup(currentName string, next config.NodeGroup) error {
	currentName = strings.TrimSpace(currentName)
	next.Name = strings.TrimSpace(next.Name)
	next.Type = strings.TrimSpace(next.Type)
	next.Subscription = strings.TrimSpace(next.Subscription)
	if next.Type == "" {
		next.Type = config.GroupTypeURLTest
	}
	if next.Name != currentName {
		return fmt.Errorf("策略分组暂不支持改名：%q -> %q", currentName, next.Name)
	}
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	a.mu.Lock()
	index := -1
	for i, group := range a.cfg.Groups {
		if group.Name == currentName {
			index = i
			break
		}
	}
	if index < 0 {
		a.mu.Unlock()
		return fmt.Errorf("分组 %q 不存在", currentName)
	}
	oldGroups := cloneNodeGroups(a.cfg.Groups)
	// 临时移除当前实体后复用新增校验，既能允许端口保持不变，又能检查与其它分组冲突。
	a.cfg.Groups = append(cloneNodeGroups(oldGroups[:index]), oldGroups[index+1:]...)
	if err := a.cfg.CheckGroup(next); err != nil {
		a.cfg.Groups = oldGroups
		a.mu.Unlock()
		return err
	}
	for _, currentNode := range a.nodes {
		if currentNode.Name == next.Name {
			a.cfg.Groups = oldGroups
			a.mu.Unlock()
			return fmt.Errorf("分组名 %q 与节点名冲突", next.Name)
		}
	}
	candidate := cloneNodeGroups(oldGroups)
	candidate[index] = next
	a.cfg.Groups = candidate
	a.mu.Unlock()

	if err := a.regenerateCurrentLocked(); err != nil {
		return a.rollbackGroupsLocked(oldGroups, fmt.Errorf("分组配置未通过 mihomo 校验: %w", err), false)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return a.rollbackGroupsLocked(oldGroups, persistErr, true)
	}
	return nil
}

// rollbackGroupsLocked 恢复策略分组事务开始前的配置、运行态和可选磁盘状态。
// 调用方必须持有 refreshing 锁，避免回滚期间刷新任务写入新的运行配置。
//
// 参数：
//   - oldGroups: []config.NodeGroup，事务前的深拷贝分组列表。
//   - cause: error，触发回滚的原始错误。
//   - restoreDisk: bool，是否还需要把旧配置重新写回磁盘。
//
// 返回值：error，始终包含 cause；运行态或磁盘恢复失败时合并返回全部错误。
//
// 错误情况：回滚失败不会被吞掉，调用方可以据此提示管理员检查实际监听状态。
func (a *App) rollbackGroupsLocked(oldGroups []config.NodeGroup, cause error, restoreDisk bool) error {
	a.mu.Lock()
	a.cfg.Groups = cloneNodeGroups(oldGroups)
	a.mu.Unlock()
	joined := cause
	if rollbackErr := a.regenerateCurrentLocked(); rollbackErr != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复旧策略分组运行态失败: %w", rollbackErr))
	}
	if restoreDisk {
		a.mu.Lock()
		rollbackErr := a.persistLocked()
		a.mu.Unlock()
		if rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复旧策略分组配置文件失败: %w", rollbackErr))
		}
	}
	return joined
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

// AddSubscription 添加默认启用、自动识别格式的订阅，并持久化到配置文件。
// 该兼容入口供现有 CLI 使用；需要指定类型或初始禁用时应调用 AddSubscriptionEntry。
//
// 参数：
//   - name: string，订阅名；空值由 URL 主机名自动生成。
//   - url: string，HTTP(S) 订阅地址。
//
// 返回值：
//   - config.Subscription，规范化并已提交的订阅。
//   - error，字段非法、名称/URL 重复或配置持久化失败时返回。
//
// 错误情况：持久化失败会撤销内存追加，避免 API 报错后 overview 却出现未落盘订阅。
func (a *App) AddSubscription(name, url string) (config.Subscription, error) {
	enabled := true
	return a.AddSubscriptionEntry(config.Subscription{Name: name, URL: url, Type: "auto", Enabled: &enabled})
}

// AddSubscriptionEntry 添加带类型和启用状态的完整订阅值对象。
//
// 参数：
//   - sub: config.Subscription，包含可选名称、HTTP(S) URL、auto|clash|share 类型和启用状态。
//
// 返回值：
//   - config.Subscription，补齐名称、类型和 enabled 后的已提交值。
//   - error，字段校验、唯一性校验或持久化失败时返回。
//
// 错误情况：该方法只提交配置，不拉取订阅；启用后的首次拉取由 API 异步触发。
// 持久化失败会删除刚追加的内存项，保持运行态与磁盘一致。
func (a *App) AddSubscriptionEntry(sub config.Subscription) (config.Subscription, error) {
	sub.Name = strings.TrimSpace(sub.Name)
	sub.URL = strings.TrimSpace(sub.URL)
	sub.Type = strings.ToLower(strings.TrimSpace(sub.Type))
	if sub.Type == "" {
		sub.Type = "auto"
	}
	if sub.Enabled == nil {
		sub.Enabled = new(bool)
		*sub.Enabled = true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if sub.Name == "" {
		sub.Name = autoSubName(sub.URL, a.cfg.Subscriptions)
	}
	if err := validateSubscriptionFields(sub); err != nil {
		return config.Subscription{}, err
	}
	for _, s := range a.cfg.Subscriptions {
		if s.URL == sub.URL {
			return s, fmt.Errorf("订阅地址已存在（%s）", s.Name)
		}
	}
	for _, s := range a.cfg.Subscriptions {
		if s.Name == sub.Name {
			return config.Subscription{}, fmt.Errorf("订阅名 %q 已存在", sub.Name)
		}
	}
	a.cfg.Subscriptions = append(a.cfg.Subscriptions, sub)
	if err := a.persistLocked(); err != nil {
		a.cfg.Subscriptions = a.cfg.Subscriptions[:len(a.cfg.Subscriptions)-1]
		return config.Subscription{}, err
	}
	return sub, nil
}

// UpdateSubscription 编辑订阅名称、URL、类型和启用状态，并把策略组引用、节点运行态
// 与配置文件作为一次事务提交。启用订阅前必须成功拉取远端内容，或通过 FetchWarning
// 明确证明存在可解析缓存；否则旧的禁用状态保持不变。
//
// 参数：
//   - ctx: context.Context，控制订阅拉取和目标节点健康检测的取消/超时。
//   - currentName: string，要编辑的现有订阅名。
//   - next: config.Subscription，目标订阅值；Enabled 为 nil 时沿用旧状态。
//
// 返回值：
//   - config.Subscription，规范化并成功提交的目标值。
//   - error，订阅不存在、字段/唯一性非法、拉取无缓存、无可用出口、核心热更新、
//     配置持久化或回滚失败时返回。
//
// 错误情况：方法持有 refreshing 锁串行化整个事务。任何提交失败都会恢复旧订阅、
// 策略组引用、节点、端口 assignments、用量信息和 mihomo 配置；组合失败不会被吞掉。
func (a *App) UpdateSubscription(ctx context.Context, currentName string, next config.Subscription) (config.Subscription, error) {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	currentName = strings.TrimSpace(currentName)
	a.mu.RLock()
	index := -1
	var current config.Subscription
	for i, candidate := range a.cfg.Subscriptions {
		if candidate.Name == currentName {
			index = i
			current = candidate
			break
		}
	}
	stateDir := a.cfg.StateDir
	oldSubscriptions := append([]config.Subscription(nil), a.cfg.Subscriptions...)
	oldGroups := cloneNodeGroups(a.cfg.Groups)
	oldNodes := append([]*node.Node(nil), a.nodes...)
	oldAssignments := append([]pool.Assignment(nil), a.assigns...)
	oldInfos := cloneSubscriptionInfos(a.subInfos)
	a.mu.RUnlock()
	if index < 0 {
		return config.Subscription{}, fmt.Errorf("订阅 %q 不存在", currentName)
	}

	next.Name = strings.TrimSpace(next.Name)
	next.URL = strings.TrimSpace(next.URL)
	next.Type = strings.ToLower(strings.TrimSpace(next.Type))
	if next.Type == "" {
		next.Type = "auto"
	}
	if next.Enabled == nil {
		next.Enabled = current.Enabled
	}
	if err := validateSubscriptionFields(next); err != nil {
		return config.Subscription{}, err
	}
	for i, candidate := range oldSubscriptions {
		if i == index {
			continue
		}
		if candidate.Name == next.Name {
			return config.Subscription{}, fmt.Errorf("订阅名 %q 已存在", next.Name)
		}
		if candidate.URL == next.URL {
			return config.Subscription{}, fmt.Errorf("订阅地址已存在（%s）", candidate.Name)
		}
	}

	// 来源未变化（URL/类型未改且保持启用）时无需重新拉取：复用现有节点及其健康状态，
	// 只更新订阅归属标签。改名/原样保存因此立即完成，不再阻塞在订阅网络 I/O 上。
	// 旧配置可能缺省 type（等价 auto），比较前先归一化，避免误判为来源变化。
	currentType := current.Type
	if currentType == "" {
		currentType = "auto"
	}
	sourceChanged := current.URL != next.URL || currentType != next.Type
	needFetch := next.IsEnabled() && (!current.IsEnabled() || sourceChanged)

	var freshNodes []*node.Node
	var freshInfo subscribe.UserInfo
	if needFetch {
		var fetchErr error
		freshNodes, freshInfo, fetchErr = subscribe.FetchWithInfo(ctx, next, stateDir)
		if fetchErr != nil {
			var warning *subscribe.FetchWarning
			if !errors.As(fetchErr, &warning) {
				return config.Subscription{}, fmt.Errorf("启用订阅前拉取失败且没有可用缓存: %w", fetchErr)
			}
			log.Printf("[subscribe] %v", fetchErr)
		}
		if len(freshNodes) == 0 {
			return config.Subscription{}, fmt.Errorf("订阅 %q 没有可用的节点内容，未提交启用", next.Name)
		}
	} else if next.IsEnabled() {
		for _, existing := range oldNodes {
			if existing == nil || existing.Subscription != current.Name {
				continue
			}
			cloned := *existing
			cloned.Subscription = next.Name
			freshNodes = append(freshNodes, &cloned)
		}
		// 用量缓存跟随改名迁移，避免概览页的流量/到期信息在改名后丢失
		if info, ok := oldInfos[current.Name]; ok {
			freshInfo = info
		}
	}

	nextSubscriptions := append([]config.Subscription(nil), oldSubscriptions...)
	nextSubscriptions[index] = next
	nextGroups := cloneNodeGroups(oldGroups)
	if current.Name != next.Name {
		for i := range nextGroups {
			if nextGroups[i].Subscription == current.Name {
				nextGroups[i].Subscription = next.Name
			}
		}
	}
	enabledSources := map[string]bool{subscribe.ManualSubscription: true}
	for _, candidate := range nextSubscriptions {
		enabledSources[candidate.Name] = candidate.IsEnabled()
	}
	nodesBySource := make(map[string][]*node.Node, len(nextSubscriptions)+1)
	for _, existing := range oldNodes {
		if existing == nil || existing.Subscription == current.Name || !enabledSources[existing.Subscription] {
			continue
		}
		nodesBySource[existing.Subscription] = append(nodesBySource[existing.Subscription], existing)
	}
	if next.IsEnabled() {
		nodesBySource[next.Name] = freshNodes
	}
	nextNodes := subscribe.MergeFiltered(nodesBySource, a.includeRe, a.excludeRe)
	if needFetch {
		checkList := make([]*node.Node, 0, len(freshNodes))
		for _, candidate := range nextNodes {
			if candidate.Subscription == next.Name {
				checkList = append(checkList, candidate)
			}
		}
		pool.Check(ctx, checkList, a.cfg.HealthURL, a.cfg.HealthTimeout.D(), 32, groupNames(nextGroups)...)
	}

	alive := make([]*node.Node, 0, len(nextNodes))
	for _, candidate := range nextNodes {
		if candidate.Alive {
			alive = append(alive, candidate)
		}
	}
	// 健康节点保护只约束真正改变来源的提交（换 URL/重新启用）；纯改名不会降低可用性，
	// 不应因为节点此刻全部失效而被拒绝。
	if needFetch && len(nextNodes) > 0 && len(alive) == 0 {
		return config.Subscription{}, fmt.Errorf("订阅设置未提交：当前没有任何健康节点可维持代理运行")
	}
	previousSnapshot, snapshotErr := pool.LoadSnapshot(a.snapshotPath())
	if snapshotErr != nil {
		log.Printf("[alloc] load snapshot for subscription update: %v (ignored)", snapshotErr)
	}
	nextAssignments := pool.Allocate(alive, a.cfg.PortRange[0], a.cfg.PortRange[1], previousSnapshot)

	a.mu.Lock()
	a.cfg.Subscriptions = nextSubscriptions
	a.cfg.Groups = nextGroups
	a.nodes = nextNodes
	a.assigns = nextAssignments
	if next.IsEnabled() && !freshInfo.IsZero() {
		a.subInfos[next.Name] = freshInfo
	}
	if current.Name != next.Name {
		delete(a.subInfos, current.Name)
	}
	if !next.IsEnabled() {
		delete(a.subInfos, next.Name)
	}
	a.mu.Unlock()

	if err := a.regenerateLocked(nextAssignments); err != nil {
		return config.Subscription{}, a.rollbackSubscriptionLocked(
			oldSubscriptions, oldGroups, oldNodes, oldAssignments, oldInfos, err, false,
		)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return config.Subscription{}, a.rollbackSubscriptionLocked(
			oldSubscriptions, oldGroups, oldNodes, oldAssignments, oldInfos, persistErr, true,
		)
	}

	// 快照只在运行态与配置文件都提交成功后更新。这样失败回滚不会让下一次刷新
	// 误用尚未提交的端口分配；快照写失败只影响未来稳定性，不反向破坏已生效代理。
	snapshot := &pool.Snapshot{Mapping: make(map[string]int, len(nextAssignments))}
	for _, assignment := range nextAssignments {
		snapshot.Mapping[assignment.Node.Key()] = assignment.Port
	}
	if err := pool.SaveSnapshot(a.snapshotPath(), snapshot); err != nil {
		log.Printf("[alloc] save subscription update snapshot: %v", err)
	}
	if err := node.SaveSnapshot(a.nodesSnapshotPath(), nextNodes); err != nil {
		log.Printf("[snapshot] 保存订阅编辑后的节点快照失败: %v", err)
	}
	return next, nil
}

// rollbackSubscriptionLocked 恢复订阅编辑事务开始前的配置、运行态与可选磁盘状态。
// 调用方必须持有 refreshing 锁，确保回滚期间没有其它刷新穿插。
//
// 参数：
//   - subscriptions: []config.Subscription，事务前订阅快照。
//   - groups: []config.NodeGroup，事务前策略组快照。
//   - nodes: []*node.Node，事务前节点快照。
//   - assignments: []pool.Assignment，事务前稳定端口分配。
//   - infos: map[string]subscribe.UserInfo，事务前订阅用量缓存。
//   - cause: error，触发回滚的原始错误。
//   - restoreDisk: bool，是否额外把旧配置重写回磁盘。
//
// 返回值：error，始终包含 cause；运行态或磁盘恢复失败时合并返回全部错误。
//
// 错误情况：恢复失败不会静默降级，调用方会收到可诊断的组合错误。
func (a *App) rollbackSubscriptionLocked(
	subscriptions []config.Subscription,
	groups []config.NodeGroup,
	nodes []*node.Node,
	assignments []pool.Assignment,
	infos map[string]subscribe.UserInfo,
	cause error,
	restoreDisk bool,
) error {
	a.mu.Lock()
	a.cfg.Subscriptions = subscriptions
	a.cfg.Groups = groups
	a.nodes = nodes
	a.assigns = assignments
	a.subInfos = infos
	a.mu.Unlock()

	joined := cause
	if rollbackErr := a.regenerateLocked(assignments); rollbackErr != nil {
		joined = errors.Join(joined, fmt.Errorf("恢复订阅编辑前运行态失败: %w", rollbackErr))
	}
	if restoreDisk {
		a.mu.Lock()
		rollbackErr := a.persistLocked()
		a.mu.Unlock()
		if rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("恢复订阅编辑前配置文件失败: %w", rollbackErr))
		}
	}
	return joined
}

// validateSubscriptionFields 校验单个订阅值对象，不依赖 Config 的其它字段。
//
// 参数：
//   - sub: config.Subscription，已经完成空白与大小写规范化的候选值。
//
// 返回值：error，名称、URL 或类型非法时返回；合法时返回 nil。
//
// 错误情况：只允许 HTTP(S) URL 和 auto|clash|share 类型；名称不能为空。
func validateSubscriptionFields(sub config.Subscription) error {
	if sub.Name == "" {
		return fmt.Errorf("订阅名不能为空")
	}
	if !strings.HasPrefix(sub.URL, "http://") && !strings.HasPrefix(sub.URL, "https://") {
		return fmt.Errorf("订阅地址必须是 http(s) URL")
	}
	switch sub.Type {
	case "auto", "clash", "share":
		return nil
	default:
		return fmt.Errorf("订阅类型 %q 无效（auto|clash|share）", sub.Type)
	}
}

// cloneNodeGroups 深复制策略组切片，避免事务内修改 Nodes 或 Subscription 时污染旧快照。
//
// 参数：
//   - groups: []config.NodeGroup，源策略组列表。
//
// 返回值：[]config.NodeGroup，可独立修改的副本。
//
// 错误情况：无；nil 输入返回 nil。
func cloneNodeGroups(groups []config.NodeGroup) []config.NodeGroup {
	out := append([]config.NodeGroup(nil), groups...)
	for i := range out {
		out[i].Nodes = append([]string(nil), groups[i].Nodes...)
	}
	return out
}

// cloneSubscriptionInfos 复制订阅用量状态 map，供事务失败时完整恢复。
//
// 参数：
//   - infos: map[string]subscribe.UserInfo，当前应用层用量状态。
//
// 返回值：map[string]subscribe.UserInfo，可独立增删的副本。
//
// 错误情况：无；nil 输入返回空 map，保持后续写入安全。
func cloneSubscriptionInfos(infos map[string]subscribe.UserInfo) map[string]subscribe.UserInfo {
	out := make(map[string]subscribe.UserInfo, len(infos))
	for name, info := range infos {
		out[name] = info
	}
	return out
}

// groupNames 提取策略组名称，作为 dialer-proxy 健康检查允许引用的目标集合。
//
// 参数：
//   - groups: []config.NodeGroup，候选策略组列表。
//
// 返回值：[]string，保持配置顺序的组名。
//
// 错误情况：无；空名称由下游健康检查忽略。
func groupNames(groups []config.NodeGroup) []string {
	out := make([]string, 0, len(groups))
	for _, group := range groups {
		out = append(out, group.Name)
	}
	return out
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

// ExportConfig 导出当前运行配置的 YAML，可选择对凭据打码。
//
// 参数：
//   - maskTokens: bool，true 时隐藏 secret、URL 用户信息和敏感查询参数。
//
// 返回值：
//   - []byte：YAML 配置内容。
//   - error：YAML 序列化失败时返回错误。
//
// 错误情况：导出期间持有配置读锁以获得一致快照；不会读取订阅缓存或修改运行状态。
func (a *App) ExportConfig(maskTokens bool) ([]byte, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.ExportYAML(maskTokens)
}

// PreviewImport 解析并完整校验待导入配置，返回不含凭据的影响摘要和内容摘要，且绝不写盘。
//
// 参数：
//   - raw: []byte，用户选择的完整 YAML 原始字节；确认阶段必须提交完全相同的内容。
//
// 返回值：
//   - ConfigImportPreview，包含 SHA-256 digest、对象数量变化、关键字段变化与安全警告。
//   - error，YAML 或配置结构/业务约束校验失败时返回。
//
// 错误情况：预检只调用 config.Parse 和内存比较，不修改 a.cfg、运行态、缓存或配置文件；
// 因此任何错误都可以安全返回给 UI 修正后重试。
func (a *App) PreviewImport(raw []byte) (ConfigImportPreview, error) {
	next, err := config.Parse(raw)
	if err != nil {
		return ConfigImportPreview{}, fmt.Errorf("导入配置预检失败: %w", err)
	}
	a.mu.RLock()
	current := a.cfg
	preview := ConfigImportPreview{
		Digest:          configImportDigest(raw),
		RestartRequired: true,
		Counts: map[string]ConfigCountChange{
			"subscriptions": {Before: len(current.Subscriptions), After: len(next.Subscriptions)},
			"manual_nodes":  {Before: len(current.ManualNodes), After: len(next.ManualNodes)},
			"groups":        {Before: len(current.Groups), After: len(next.Groups)},
			"custom_rules":  {Before: len(current.CustomRules), After: len(next.CustomRules)},
			"rule_urls":     {Before: len(current.RuleURLs), After: len(next.RuleURLs)},
		},
	}
	if current.Listen != next.Listen {
		preview.ChangedFields = append(preview.ChangedFields, "节点监听地址")
	}
	if current.MixedPort != next.MixedPort {
		preview.ChangedFields = append(preview.ChangedFields, "主代理端口")
	}
	if current.PortRange != next.PortRange {
		preview.ChangedFields = append(preview.ChangedFields, "节点端口区间")
	}
	if current.APIListen != next.APIListen {
		preview.ChangedFields = append(preview.ChangedFields, "管理 API 地址")
	}
	if current.StateDir != next.StateDir {
		preview.ChangedFields = append(preview.ChangedFields, "状态目录")
	}
	if current.ExternalController != next.ExternalController {
		preview.ChangedFields = append(preview.ChangedFields, "mihomo 控制器地址")
	}
	if current.TUN.Enable != next.TUN.Enable {
		preview.ChangedFields = append(preview.ChangedFields, "TUN 开关")
	}
	if current.PortMappingEnabled() != next.PortMappingEnabled() {
		preview.ChangedFields = append(preview.ChangedFields, "节点端口映射开关")
	}
	a.mu.RUnlock()
	if next.Listen != "127.0.0.1" && next.Listen != "::1" && next.Listen != "localhost" {
		preview.Warnings = append(preview.Warnings, "节点监听地址不是回环地址，请确认局域网暴露风险")
	}
	if next.TUN.Enable {
		preview.Warnings = append(preview.Warnings, "导入后将启用 TUN，重启进程需要相应系统权限")
	}
	if len(preview.ChangedFields) == 0 {
		preview.ChangedFields = []string{}
	}
	if len(preview.Warnings) == 0 {
		preview.Warnings = []string{}
	}
	return preview, nil
}

// ImportConfigConfirmed 校验确认摘要并原子写入配置，确保用户确认的内容与最终提交字节一致。
//
// 参数：
//   - raw: []byte，本次要写入的 YAML 原始字节。
//   - expectedDigest: string，PreviewImport 返回的十六进制 SHA-256 摘要。
//
// 返回值：error，摘要缺失/不匹配、配置校验或写盘失败时返回；成功返回 nil。
//
// 错误情况：摘要使用常量时间比较，避免确认后文件内容被替换或浏览器状态过期时误写；
// 摘要通过后仍重新解析校验，不能把预检结果当作绕过最终边界校验的许可证。
func (a *App) ImportConfigConfirmed(raw []byte, expectedDigest string) error {
	expectedDigest = strings.ToLower(strings.TrimSpace(expectedDigest))
	actualDigest := configImportDigest(raw)
	if len(expectedDigest) != len(actualDigest) || subtle.ConstantTimeCompare([]byte(expectedDigest), []byte(actualDigest)) != 1 {
		return fmt.Errorf("配置内容已变化或尚未预检，请重新预览后确认导入")
	}
	return a.ImportConfig(raw)
}

// configImportDigest 计算配置确认协议使用的稳定 SHA-256 十六进制摘要。
//
// 参数：
//   - raw: []byte，未经规范化的 YAML 原始字节。
//
// 返回值：string，64 个小写十六进制字符；换行或空白变化也会产生不同摘要。
//
// 错误情况：无；sha256.Sum256 对任意字节输入都有确定结果。
func configImportDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ImportConfig 校验并原子写入一份完整配置，等待进程重启后生效。
//
// 参数：
//   - raw: []byte，用户上传的完整 YAML 配置，调用方应限制请求体大小。
//
// 返回值：
//   - error：YAML/配置校验失败、当前实例无配置路径或原子写入失败时返回错误。
//
// 错误情况：导入不会直接替换 a.cfg。配置可能改变 api-listen、state-dir 和系统监听
// 端口，在当前 HTTP 请求中热替换会让磁盘地址与运行地址分裂，因此只写磁盘并要求重启。
func (a *App) ImportConfig(raw []byte) error {
	next, err := config.Parse(raw)
	if err != nil {
		return fmt.Errorf("导入配置校验失败: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfgPath == "" {
		return fmt.Errorf("当前实例没有配置文件路径，无法导入；请先使用 proxyd serve -c <配置文件> 启动")
	}
	// 导入与设置页的其他持久化操作共用同一把配置锁，避免两个 Save 同时使用
	// `<config>.tmp`，也避免较早开始的普通设置在导入完成后覆盖整份备份。
	if err := next.Save(a.cfgPath); err != nil {
		return fmt.Errorf("写入导入配置失败: %w", err)
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
		var infos map[string]subscribe.UserInfo
		nodes, infos, errs = subscribe.FetchAllWithInfoAndFilters(ctx, subs, a.cfg.StateDir, a.includeRe, a.excludeRe,
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
		a.subInfos = infos
		a.mu.Unlock()
	} else {
		// 仅测速刷新也必须重新应用当前订阅开关；否则刚从配置恢复的禁用订阅节点
		// 可能因旧内存快照继续参与健康检测和监听生成。
		nodes = filterEnabledSubscriptionNodes(a.Nodes(), a.Subscriptions())
	}

	pool.Check(ctx, nodes, a.cfg.HealthURL, a.cfg.HealthTimeout.D(), 32, a.dialerTargets()...)
	return a.applyNodes(ctx, nodes)
}

// applyNodes 执行健康检测后的流水线尾部，并完成链式代理的二阶段验证。
//
// 参数：
//   - ctx: context.Context，控制完整链路 URLTest 的取消与超时传播。
//   - nodes: []*node.Node，本轮订阅合并后的节点集合；健康状态会原地更新并保存快照。
//
// 返回值：error，没有任何可用节点、端口分配后的配置无法加载，或完整链路全部失败时返回。
//
// 错误情况：调用方必须持有 a.refreshing 锁。普通节点已经由 pool.Check 直接测速；
// dialer-proxy 节点先以依赖候选身份加载，随后通过 Runner.URLTest 验证真实完整链路。
// 若候选失败，会重新分配端口并热加载一次，确保失败链路不会残留在最终监听入口。
func (a *App) applyNodes(ctx context.Context, nodes []*node.Node) error {
	// 先更新应用节点快照，让 GenerateWithNodes 能看到未分配端口的链路依赖。
	// 刷新失败时仍保留本轮状态和失败原因，便于 Web/CLI 解释问题，而不是展示旧假象。
	a.mu.Lock()
	a.nodes = nodes
	a.mu.Unlock()

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
	if err := a.regenerateLocked(assigns); err != nil {
		return err
	}

	// pool.Check 无法在首次配置加载前解析 dialer-proxy 的运行时依赖。此处使用刚刚
	// 生效的 mihomo 代理表执行真实 URLTest；只在可用性发生变化时重新生成配置，
	// 延迟数值变化本身不触发第二次热更新，避免无意义地重建 listener。
	if a.verifyDialerNodes(ctx, nodes) {
		alive = alive[:0]
		for _, n := range nodes {
			if n.Alive {
				alive = append(alive, n)
			}
		}
		if len(alive) == 0 {
			// 候选配置已经短暂加载，不能直接返回并把失败链路 listener 留在运行态。
			// 生成空 assignment 配置会释放节点端口，同时保留主端口的 DIRECT 回退；
			// 清理失败时把两层错误一并返回，避免调用方误以为运行态已经安全收敛。
			if cleanupErr := a.regenerateLocked(nil); cleanupErr != nil {
				return fmt.Errorf("all %d dialer-proxy candidates failed end-to-end health check; cleanup failed: %w", len(nodes), cleanupErr)
			}
			a.mu.Lock()
			a.assigns = nil
			a.mu.Unlock()
			return fmt.Errorf("all %d dialer-proxy candidates failed end-to-end health check", len(nodes))
		}
		assigns = pool.Allocate(alive, a.cfg.PortRange[0], a.cfg.PortRange[1], prev)
		if err := a.regenerateLocked(assigns); err != nil {
			return err
		}
	}

	snap := &pool.Snapshot{Mapping: make(map[string]int, len(assigns))}
	for _, as := range assigns {
		snap.Mapping[as.Node.Key()] = as.Port
	}
	if err := pool.SaveSnapshot(a.snapshotPath(), snap); err != nil {
		log.Printf("[alloc] save snapshot: %v", err)
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

// verifyDialerNodes 使用已加载的 mihomo 代理表验证所有链式节点的真实端到端可用性。
//
// 参数：
//   - ctx: context.Context，整轮刷新取消时终止后续测试。
//   - nodes: []*node.Node，包含普通节点和 dialer-proxy 节点的本轮节点集合。
//
// 返回值：bool，只要任一链式节点从候选可用变为不可用就返回 true，提示调用方重生成配置。
//
// 错误情况：代理不存在、上游组不可用、网络失败与超时均写入 FailReason。检测串行执行，
// 因为 Runner 为保护 mihomo 全局代理表会持锁；这样避免并发 URLTest 与热更新产生竞态。
func (a *App) verifyDialerNodes(ctx context.Context, nodes []*node.Node) bool {
	availabilityChanged := false
	for _, n := range nodes {
		if n == nil || n.DialerProxy() == "" || !n.Alive {
			continue
		}
		delay, err := a.runner.URLTest(ctx, n.Name, a.cfg.HealthURL, a.cfg.HealthTimeout.D())
		if err != nil {
			n.Alive = false
			n.Delay = 0
			n.FailReason = firstErrorLine(err.Error())
			availabilityChanged = true
			continue
		}
		n.Delay = delay
		n.FailReason = ""
	}
	return availabilityChanged
}

// firstErrorLine 把底层多行错误压缩为适合节点状态展示的一行文本。
//
// 参数：
//   - message: string，可能包含换行、堆栈或协议详情的错误内容。
//
// 返回值：string，第一个换行符之前的内容；没有换行时返回原内容。
//
// 错误情况：无；空字符串原样返回。
func firstErrorLine(message string) string {
	if index := strings.IndexByte(message, '\n'); index >= 0 {
		return message[:index]
	}
	return message
}

// dialerTargets 返回当前配置中可被节点 dialer-proxy 引用的策略组名称快照。
//
// 参数：无；方法在读锁内读取 Config.Groups。
//
// 返回值：[]string，保持配置顺序的组名列表；调用方可自由修改返回切片。
//
// 错误情况：无；分组结构已经由 config.Validate 校验，空名称仍会在 pool 层忽略。
func (a *App) dialerTargets() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	targets := make([]string, 0, len(a.cfg.Groups))
	for _, group := range a.cfg.Groups {
		targets = append(targets, group.Name)
	}
	return targets
}

// RefreshSubscription 只刷新单个订阅：重新拉取该订阅，与其它来源的现有节点
// 重新合并后只检测该订阅的节点，再执行端口分配与热更新。
func (a *App) RefreshSubscription(ctx context.Context, name string) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()

	a.mu.RLock()
	var target *config.Subscription
	for i := range a.cfg.Subscriptions {
		if a.cfg.Subscriptions[i].Name == name {
			sub := a.cfg.Subscriptions[i]
			target = &sub
			break
		}
	}
	a.mu.RUnlock()
	if target == nil {
		return fmt.Errorf("subscription %q not found", name)
	}
	if !target.IsEnabled() {
		return fmt.Errorf("订阅 %q 已禁用，请先启用后再刷新", name)
	}

	fresh, info, err := subscribe.FetchWithInfo(ctx, *target, a.cfg.StateDir)
	if err != nil {
		var w *subscribe.FetchWarning
		if !errors.As(err, &w) {
			return err
		}
		log.Printf("[subscribe] %v", err) // 拉取失败，降级使用缓存节点
	}

	// 其它来源沿用现有节点，与该订阅的新节点重新合并（Merge 按稳定身份去重、
	// 保证名称唯一；名称变化不影响端口稳定映射，后者按节点 Key 对齐快照）
	groups := map[string][]*node.Node{name: fresh}
	for _, n := range a.Nodes() {
		if n.Subscription != name {
			groups[n.Subscription] = append(groups[n.Subscription], n)
		}
	}
	nodes := subscribe.MergeFiltered(groups, a.includeRe, a.excludeRe)
	if len(nodes) == 0 {
		return fmt.Errorf("no nodes available from any subscription")
	}
	if !info.IsZero() {
		a.mu.Lock()
		a.subInfos[name] = info
		a.mu.Unlock()
	}

	// 只检测该订阅的节点，其它节点沿用上次检测结果
	var checkList []*node.Node
	for _, n := range nodes {
		if n.Subscription == name {
			checkList = append(checkList, n)
		}
	}
	pool.Check(ctx, checkList, a.cfg.HealthURL, a.cfg.HealthTimeout.D(), 32, a.dialerTargets()...)
	return a.applyNodes(ctx, nodes)
}

// TestSubscription 只对单个订阅的现有节点做健康检测/延迟测试，
// 不重新拉取订阅；完成后重新分配端口并热更新。
func (a *App) TestSubscription(ctx context.Context, name string) error {
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	a.mu.RLock()
	found := false
	enabled := false
	for _, subscription := range a.cfg.Subscriptions {
		if subscription.Name == name {
			found = true
			enabled = subscription.IsEnabled()
			break
		}
	}
	a.mu.RUnlock()
	if !found {
		return fmt.Errorf("订阅 %q 不存在", name)
	}
	if !enabled {
		return fmt.Errorf("订阅 %q 已禁用，请先启用后再测速", name)
	}

	nodes := a.Nodes()
	var checkList []*node.Node
	for _, n := range nodes {
		if n.Subscription == name {
			checkList = append(checkList, n)
		}
	}
	if len(checkList) == 0 {
		return fmt.Errorf("订阅 %s 当前没有节点", name)
	}
	pool.Check(ctx, checkList, a.cfg.HealthURL, a.cfg.HealthTimeout.D(), 32, a.dialerTargets()...)
	return a.applyNodes(ctx, nodes)
}

// filterEnabledSubscriptionNodes 按订阅启用状态过滤运行节点，同时始终保留手动节点。
//
// 参数：
//   - nodes: []*node.Node，当前内存节点快照。
//   - subscriptions: []config.Subscription，当前订阅配置快照。
//
// 返回值：[]*node.Node，只包含启用订阅和 manual 来源的节点；保持原顺序。
//
// 错误情况：无；来源不存在于配置中的陈旧节点会被过滤，避免删除订阅后继续监听。
func filterEnabledSubscriptionNodes(nodes []*node.Node, subscriptions []config.Subscription) []*node.Node {
	enabled := map[string]bool{subscribe.ManualSubscription: true}
	for _, subscription := range subscriptions {
		enabled[subscription.Name] = subscription.IsEnabled()
	}
	out := make([]*node.Node, 0, len(nodes))
	for _, candidate := range nodes {
		if candidate != nil && enabled[candidate.Subscription] {
			out = append(out, candidate)
		}
	}
	return out
}

// Run 启动调度器：异步检查版本、恢复节点快照、执行首次刷新，再进入周期任务。
//
// 参数：
//   - ctx: context.Context，收到 SIGINT/SIGTERM 后用于停止后台检查、定时器和 mihomo。
//
// 返回值：
//   - error：TUN 配置要求开启但首次应用失败时返回错误；正常取消返回 nil。
//
// 错误情况：版本检查失败只写状态和日志，不影响启动；普通首次订阅刷新失败会保留快照，
// 只有 TUN 实际未启用可能造成流量绕过时才停止服务。
func (a *App) Run(ctx context.Context) error {
	go a.startVersionCheck(ctx)
	a.restoreSnapshot()
	if err := a.Refresh(ctx, true); err != nil {
		// TUN 配置要求全局接管系统路由；如果首次应用后 listener 仍未生效，继续以
		// “看似开启、实际关闭”的状态运行会造成流量泄漏，因此把启动失败上抛给 CLI。
		if a.cfg.TUN.Enable && !a.runner.TUNEnabled() {
			return fmt.Errorf("TUN 未能启动，服务已停止以避免流量绕过代理: %w", err)
		}
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
