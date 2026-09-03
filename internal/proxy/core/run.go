package core

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub"
	"github.com/metacubex/mihomo/hub/executor"
	"github.com/metacubex/mihomo/listener"
	"github.com/metacubex/mihomo/tunnel"
)

// Runner 以库方式内嵌运行 mihomo 核心，支持启动、热更新与关闭。
type Runner struct {
	mu       sync.Mutex
	stateDir string
	started  bool
}

// NewRunner 创建 Runner。stateDir 作为 mihomo 的 home 目录（存 cache.db / geo 文件），
// 为空则沿用 mihomo 默认路径。
// 注意：SetHomeDir 必须在任何配置解析（含 Generate 的自检 ParseWithBytes）之前完成，
// 否则 geo 文件路径为空，GEOSITE/GEOIP 规则会因 "open :" 报错。
func NewRunner(stateDir string) *Runner {
	if stateDir != "" {
		if err := os.MkdirAll(stateDir, 0o755); err == nil {
			C.SetHomeDir(stateDir)
			ensureStateDirWritable(stateDir)
		}
	}
	return &Runner{stateDir: stateDir}
}

// geoFileNames 是 mihomo 会在 home 目录读写的 geo 数据文件（见 mihomo constant/path.go）。
var geoFileNames = []string{"GeoSite.dat", "GeoIP.dat", "Country.mmdb", "geoip.metadb", "ASN.mmdb"}

// ensureStateDirWritable 在启动时探测 state-dir 可写性，并尽量自动修复 geo 文件权限。
//
// 背景：用 sudo 或其他用户跑过 proxyd（常见于 TUN 授权）后，state-dir 或其中的 geo
// 文件会变成 root 所有，普通用户再启动时 mihomo 下载 geo 数据会报 permission denied，
// GEO 规则只能降级运行。这里分两级处理：
//  1. 目录本身不可写：无法自动修复，输出带 chown 命令的明确指引；
//  2. 目录可写但 geo 文件不可读：直接删除该文件（删除只看目录权限），让 mihomo 重新下载。
func ensureStateDirWritable(stateDir string) {
	probe, err := os.CreateTemp(stateDir, ".proxyd-write-probe-*")
	if err != nil {
		log.Printf("[core] state-dir %s 不可写（%v）：geo 数据、订阅缓存与端口快照都将无法持久化。"+
			"通常是因为之前用 sudo 或其他用户运行过 proxyd；请执行 sudo chown -R $(id -un):$(id -gn) %s 后重启",
			stateDir, err, stateDir)
		return
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())

	for _, name := range geoFileNames {
		path := filepath.Join(stateDir, name)
		f, err := os.OpenFile(path, os.O_RDONLY, 0)
		if err == nil {
			_ = f.Close()
			continue
		}
		if !errors.Is(err, os.ErrPermission) {
			continue // 文件不存在等情况交给 mihomo 自行下载
		}
		if rmErr := os.Remove(path); rmErr != nil {
			log.Printf("[core] geo 文件 %s 无读取权限且删除失败（%v）；请执行 sudo chown -R $(id -un):$(id -gn) %s 后重启", path, rmErr, stateDir)
			continue
		}
		log.Printf("[core] geo 文件 %s 属主异常（曾以其他用户运行？），已删除，将由 mihomo 重新下载", path)
	}
}

// Start 首次启动 mihomo 核心：整体应用配置。
func (r *Runner) Start(cfgYAML []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := hub.Parse(cfgYAML); err != nil {
		return fmt.Errorf("应用 mihomo 配置: %w", err)
	}
	r.started = true
	return nil
}

// Reload 热更新配置：hub.Parse 内部已 ApplyConfig（重建 REST API 与所有 listener）。
// 未 Start 时等价于 Start。
func (r *Runner) Reload(cfgYAML []byte) error {
	return r.Start(cfgYAML)
}

// URLTest 对已加载到 mihomo 代理表中的指定出站执行端到端延迟测试。
//
// 参数：
//   - ctx: context.Context，调用方取消刷新时立即终止网络探测。
//   - name: string，mihomo proxy 名称；链式节点必须在完整配置加载后才能找到依赖。
//   - url: string，健康检测目标 URL。
//   - timeout: time.Duration，单次检测最大时长；小于等于 0 时只使用 ctx 的期限。
//
// 返回值：
//   - uint16，mihomo 测得的毫秒延迟。
//   - error，核心未加载、代理不存在、超时或完整链路请求失败时返回。
//
// 错误情况：方法在读取代理表和执行 URLTest 期间持有 Runner 锁，避免热更新替换全局
// proxy map 造成数据竞争；因此较慢的链路检测会串行阻塞同一 Runner 的 Reload。
func (r *Runner) URLTest(ctx context.Context, name, url string, timeout time.Duration) (uint16, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return 0, fmt.Errorf("mihomo core is not started")
	}
	proxy, exists := tunnel.Proxies()[name]
	if !exists {
		return 0, fmt.Errorf("mihomo proxy %q not found", name)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	delay, err := proxy.URLTest(ctx, url, nil)
	if err != nil {
		return 0, fmt.Errorf("proxy %q URLTest: %w", name, err)
	}
	return delay, nil
}

// TUNEnabled 返回 mihomo 当前实际生效的 TUN listener 状态。
//
// 参数：无。
//
// 返回值：
//   - bool：TUN listener 创建成功并处于启用状态时返回 true。
//
// 错误情况：无；mihomo 的 ReCreateTun 会吞掉设备创建错误并把 LastTunConf.Enable
// 置回 false，因此这里必须读取 listener 实际状态，而不能相信输入配置。
func (r *Runner) TUNEnabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return listener.GetTunConf().Enable
}

// Shutdown 关闭 mihomo 核心（清理 listener 等）。
func (r *Runner) Shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	executor.Shutdown()
	r.started = false
}
