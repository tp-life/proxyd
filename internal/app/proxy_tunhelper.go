package app

// 代理域：tun-helper（root 特权助手）编排 —— TUN 设备 fd 的申请、参数复算与运行时注入。
//
// 背景见 docs/privilege-model.md 方案 B：macOS 上 utun 创建、地址配置与路由写入需要
// root，由 LaunchDaemon 托管的 com.proxyd.tun-helper 代劳；主进程恒以普通用户运行，
// 经 SCM_RIGHTS 取得 fd 后注入 mihomo 的 tun.file-descriptor（上游原生支持，fd 模式下
// sing-tun 跳过设备创建/地址/路由）。fd 是纯运行时状态：不落盘、每次开启重新申请；
// helper 不持有 fd 副本，主进程退出时内核随最后 fd 关闭自动销毁 utun 与关联路由。

import (
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"runtime"

	"go4.org/netipx"

	"proxyd/internal/autostart"
	"proxyd/internal/config"
	"proxyd/internal/proxy/tunhelper"
)

// 可注入的 tun-helper 客户端入口，测试替换。
var (
	requestTUNFD             = tunhelper.RequestTUN
	closeTUNFD               = tunhelper.CloseFD
	precheckTUNHelper        = tunhelper.Precheck
	tunHelperInstalledStatus = tunhelper.InstalledStatus
)

// migrateRootAutostartDaemon 把旧的 root 模式 LaunchDaemon（tun-helper 落地前 TUN
// 需要 root 时的产物，plist 无 UserName）重写为普通账户降权运行。
//
// 参数：无。
//
// 返回值：无；整个迁移是 best-effort，任何失败只记日志。
//
// 错误情况：仅当当前进程为 root（即正由旧服务托管）且自启项仍为 root 模式时触发；
// 降权账户从 plist 注入的 HOME 环境推导并经系统账户库校验，推导不出则跳过，
// 留待用户重新执行 proxyd autostart off/on。
func (a *App) migrateRootAutostartDaemon() {
	if runtime.GOOS != "darwin" || os.Geteuid() != 0 {
		return
	}
	if !autostart.RootDaemonPlistInstalled() {
		return
	}
	home := os.Getenv("HOME")
	name := filepath.Base(home)
	if home == "" || name == "." || name == "root" {
		log.Printf("[autostart] 旧 root 模式自启项迁移跳过：无法从 HOME=%q 推导运行账户", home)
		return
	}
	if _, err := user.Lookup(name); err != nil {
		log.Printf("[autostart] 旧 root 模式自启项迁移跳过：账户 %q 校验失败: %v", name, err)
		return
	}
	opt, err := a.autostartOptions()
	if err != nil {
		log.Printf("[autostart] 旧 root 模式自启项迁移跳过: %v", err)
		return
	}
	opt.RunAsUser = name
	if err := autostart.On(opt); err != nil {
		log.Printf("[autostart] 旧 root 模式自启项迁移失败（可执行 proxyd autostart off && proxyd autostart on 手工重建）: %v", err)
		return
	}
	log.Printf("[autostart] 已将旧的 root 模式自启项迁移为账户 %s 降权运行（TUN 改由 tun-helper 代劳）", name)
}

// ensureTUNFDLocked 返回当前应注入 mihomo 的 TUN fd；调用方须持有 refreshing 锁。
//
// 参数：
//   - cfg: *config.Config，本次待应用的运行配置（只读）。
//
// 返回值：
//   - int：可用的 utun fd；0 表示无需注入（TUN 未开启、非 darwin、或进程为 root 时
//     由 mihomo 自行创建设备）。
//   - bool：本次调用是否新申请了 fd（失败路径据此决定是否关闭）。
//   - error：helper 不可达或创建失败时返回，文本附带安装指引。
//
// 错误情况：fd 已持有（a.tunFD>0）时直接复用；mihomo 的 ReCreateTun 对相同 fd 的配置
// 短路不重建，因此重复注入同一 fd 不会导致设备重建。
func (a *App) ensureTUNFDLocked(cfg *config.Config) (int, bool, error) {
	if !cfg.TUN.Enable {
		return 0, false, nil
	}
	if a.tunFD > 0 {
		return a.tunFD, false, nil
	}
	// linux/windows 的设备创建权限由 tunperm 在开启前校验；root 进程可自行创建。
	if runtime.GOOS != "darwin" || os.Geteuid() == 0 {
		return 0, false, nil
	}
	fd, ifName, err := requestTUNFD(tunCreateParams(cfg))
	if err != nil {
		return 0, false, fmt.Errorf("经 tun-helper 创建 TUN 设备失败（未安装可执行 proxyd tun helper install，一次性管理员授权）: %w", err)
	}
	log.Printf("[app] TUN 设备已由 tun-helper 创建（%s, fd=%d）", ifName, fd)
	a.tunFD = fd
	return fd, true, nil
}

// injectTUNFD 返回注入了 file-descriptor 的运行时配置副本；fd<=0 或 TUN 未开启时
// 原样返回。绝不修改 a.cfg，保证 persistLocked 落盘的配置不含运行时 fd。
func injectTUNFD(cfg *config.Config, fd int) *config.Config {
	if fd <= 0 || !cfg.TUN.Enable {
		return cfg
	}
	out := *cfg
	tun := cfg.TUN.Clone()
	if tun.Extra == nil {
		tun.Extra = make(map[string]any, 1)
	}
	tun.Extra["file-descriptor"] = fd
	out.TUN = tun
	return &out
}

// releaseTUNFDLocked 清除持有的 TUN fd；调用方须持有 refreshing 锁。
// closeIt 为 true 时同时关闭 fd——仅限 fd 尚未随配置成功应用（所有权未移交 mihomo）
// 的回滚路径；配置已应用后 fd 由 mihomo 随 listener 关闭，本函数只清状态。
func (a *App) releaseTUNFDLocked(closeIt bool) {
	fd := a.tunFD
	a.tunFD = 0
	if closeIt && fd > 0 {
		closeTUNFD(fd)
	}
}

// tunCreateParams 按 mihomo/sing-tun 的默认规则复算 helper 创建 utun 所需的全部参数，
// 保证 fd 模式下设备形态与 mihomo 自行创建时一致。
//
// 参数：
//   - cfg: *config.Config，运行配置（只读；TUN 默认值在本地副本上补齐）。
//
// 返回值：tunhelper.CreateTUNParams，MTU/地址/路由均为最终生效值。
//
// 错误情况：无；单个非法 Extra 值按 mihomo 缺省行为回退，不阻断生成。
// 复算依据（third_party/mihomo）：
//   - MTU：config/config.go RawTun 默认 0 → sing_tun/server.go 0 视为 9000。
//   - inet4 地址：parseTun 取 dns fake-ip-range（缺省 198.18.0.1/16）的地址位 + /30。
//   - inet6：proxyd 不生成顶层 ipv6:true，parseIPV6 会把 Inet6Address 置空，故不传。
//   - 路由：route-address/inet4-route-address 非空时用之并追加掩码后的 tun 段
//     （sing-tun tun_rules.go 的 darwin 行为），否则用 darwin 默认八条子段；
//     route-exclude-address/inet4-route-exclude-address 按集合差剔除（同 sing-tun）。
func tunCreateParams(cfg *config.Config) tunhelper.CreateTUNParams {
	tun := cfg.TUN.Clone()
	tun.ApplyDefaults()
	inet4 := tunInet4Address(cfg)
	params := tunhelper.CreateTUNParams{
		MTU:          tunMTU(tun.Extra),
		Inet4Address: inet4.String(),
		AutoRoute:    tun.AutoRoute == nil || *tun.AutoRoute,
	}
	if params.AutoRoute {
		params.AutoRouteRanges = tunAutoRouteRanges(tun.Extra, inet4)
	}
	return params
}

// tunMTU 取用户配置的 tun.mtu（Extra 透传字段）；缺失或非法时回退 sing-tun 默认 9000。
func tunMTU(extra map[string]any) int {
	switch v := extra["mtu"].(type) {
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	}
	return 9000
}

// tunInet4Address 复算 mihomo parseTun 的 TUN IPv4 地址：fake-ip-range（手写 dns 段
// 优先，否则 proxyd fake-ip 预设与 mihomo 缺省同为 198.18.0.1/16）的地址位 + /30。
func tunInet4Address(cfg *config.Config) netip.Prefix {
	fallback := netip.PrefixFrom(netip.AddrFrom4([4]byte{198, 18, 0, 1}), 30)
	raw, _ := cfg.DNS["fake-ip-range"].(string)
	if raw == "" && len(cfg.DNS) == 0 && cfg.DNSPreset == config.DNSPresetFakeIP {
		raw = "198.18.0.1/16"
	}
	fakeIP, err := netip.ParsePrefix(raw)
	if err != nil {
		return fallback
	}
	addr := fakeIP.Addr()
	if !addr.Is4() {
		return fallback
	}
	return netip.PrefixFrom(addr, 30)
}

// darwinDefaultAutoRouteRanges 是 sing-tun 在 darwin 上 auto-route 的默认子段
// （tun_rules.go BuildAutoRouteRanges，避开 0.0.0.0/8 与 255/8 的八条前缀）。
var darwinDefaultAutoRouteRanges = []netip.Prefix{
	netip.PrefixFrom(netip.AddrFrom4([4]byte{1}), 8),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{2}), 7),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{4}), 6),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{8}), 5),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{16}), 4),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{32}), 3),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{64}), 2),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{128}), 1),
}

// tunAutoRouteRanges 复算 sing-tun BuildAutoRouteRanges 的最终 v4 路由前缀：
// 自定义 route-address（含 inet4-route-address）非空时用之并追加掩码后的 tun 段，
// 否则用 darwin 默认八条子段；再按 route-exclude-address（含 inet4-route-exclude-address）
// 做集合差。非法条目忽略，与 mihomo 解析失败的宽松度保持一致。
func tunAutoRouteRanges(extra map[string]any, inet4 netip.Prefix) []string {
	custom := extraPrefixList(extra, "route-address", "inet4-route-address")
	var base []netip.Prefix
	if len(custom) > 0 {
		base = custom
		if inet4.Bits() < 32 {
			base = append(base, inet4.Masked())
		}
	} else {
		base = darwinDefaultAutoRouteRanges
	}
	excludes := extraPrefixList(extra, "route-exclude-address", "inet4-route-exclude-address")
	if len(excludes) > 0 {
		var builder netipx.IPSetBuilder
		for _, prefix := range base {
			builder.AddPrefix(prefix)
		}
		for _, prefix := range excludes {
			builder.RemovePrefix(prefix)
		}
		if set, err := builder.IPSet(); err == nil {
			base = set.Prefixes()
		}
	}
	out := make([]string, 0, len(base))
	for _, prefix := range base {
		out = append(out, prefix.String())
	}
	return out
}

// extraPrefixList 从 TUN Extra 读取一个或多个键的前缀列表（YAML 解出的 string 或
// []any），仅保留合法 IPv4 前缀。
func extraPrefixList(extra map[string]any, keys ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, key := range keys {
		for _, raw := range extraStringList(extra[key]) {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil || !prefix.Addr().Is4() {
				continue
			}
			out = append(out, prefix)
		}
	}
	return out
}

// extraStringList 把 YAML inline 解出的标量/列表统一为字符串切片。
func extraStringList(value any) []string {
	switch v := value.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
