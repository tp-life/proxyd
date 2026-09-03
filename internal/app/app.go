// Package app 编排 proxyd 的订阅刷新、健康检测、端口分配和 mihomo 核心热更新用例。
package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"

	"proxyd/internal/autostart"
	"proxyd/internal/config"
	"proxyd/internal/proxy/core"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/pool"
	"proxyd/internal/proxy/subscribe"
	"proxyd/internal/proxy/tunperm"
	"proxyd/internal/remote"
)

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

	// remote 是「远程连接」周边模块（tailcat 隧道），与代理数据面独立；
	// 由 initRemote 创建，Run 启动时按配置应用，Shutdown 时关闭。
	remote *remote.Manager
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
	a.initRemote()
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

// Config 返回只读用的运行配置。
func (a *App) Config() *config.Config { return a.cfg }

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
	a.startRemote()
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
			a.stopRemote()
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

// Shutdown 关闭远程连接模块与内嵌的 mihomo 核心。
func (a *App) Shutdown() {
	a.stopRemote()
	a.runner.Shutdown()
}

func (a *App) snapshotPath() string {
	return a.cfg.StateDir + "/mapping.json"
}

func (a *App) nodesSnapshotPath() string {
	return a.cfg.StateDir + "/nodes.json"
}
