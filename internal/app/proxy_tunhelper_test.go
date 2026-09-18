package app

// tun-helper 编排的单元测试：参数复算、fd 注入与生命周期（申请/复用/释放）。

import (
	"errors"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"proxyd/internal/config"
	"proxyd/internal/proxy/tunhelper"
)

// testTUNCfg 构造一份开启 TUN 的最小配置。
func testTUNCfg() *config.Config {
	cfg := &config.Config{}
	cfg.TUN = config.DefaultTUNConfig()
	cfg.TUN.Enable = true
	return cfg
}

func TestTunCreateParamsDefaults(t *testing.T) {
	params := tunCreateParams(testTUNCfg())
	if params.MTU != 9000 {
		t.Fatalf("默认 MTU 应为 9000，实际 %d", params.MTU)
	}
	if params.Inet4Address != "198.18.0.1/30" {
		t.Fatalf("默认 inet4 地址应为 198.18.0.1/30，实际 %s", params.Inet4Address)
	}
	if params.Inet6Address != "" {
		t.Fatalf("proxyd 不生成顶层 ipv6，inet6 应为空，实际 %s", params.Inet6Address)
	}
	if !params.AutoRoute {
		t.Fatal("默认 AutoRoute 应为 true")
	}
	want := []string{"1.0.0.0/8", "2.0.0.0/7", "4.0.0.0/6", "8.0.0.0/5", "16.0.0.0/4", "32.0.0.0/3", "64.0.0.0/2", "128.0.0.0/1"}
	if !slices.Equal(params.AutoRouteRanges, want) {
		t.Fatalf("默认路由子段不符:\n got %v\nwant %v", params.AutoRouteRanges, want)
	}
	if len(params.ExcludeRanges) != 0 {
		t.Fatalf("exclude 已在 app 层做差集，不应再传给 helper，实际 %v", params.ExcludeRanges)
	}
}

func TestTunCreateParamsCustom(t *testing.T) {
	cfg := testTUNCfg()
	cfg.DNSPreset = config.DNSPresetFakeIP // fake-ip-range 198.18.0.1/16 → 地址位 + /30
	params := tunCreateParams(cfg)
	if params.Inet4Address != "198.18.0.1/30" {
		t.Fatalf("fake-ip 预设下 inet4 应为 198.18.0.1/30，实际 %s", params.Inet4Address)
	}

	cfg = testTUNCfg()
	cfg.DNS = map[string]any{"fake-ip-range": "28.0.0.1/16"}
	params = tunCreateParams(cfg)
	if params.Inet4Address != "28.0.0.1/30" {
		t.Fatalf("手写 fake-ip-range 应决定 inet4 地址，实际 %s", params.Inet4Address)
	}

	cfg = testTUNCfg()
	cfg.TUN.Extra = map[string]any{"mtu": 1500}
	if params = tunCreateParams(cfg); params.MTU != 1500 {
		t.Fatalf("自定义 mtu 应生效，实际 %d", params.MTU)
	}

	// auto-route 显式关闭 → 不产生路由参数。
	autoRoute := false
	cfg.TUN.AutoRoute = &autoRoute
	if params = tunCreateParams(cfg); params.AutoRoute || len(params.AutoRouteRanges) != 0 {
		t.Fatalf("auto-route=false 不应产生路由，实际 %+v", params)
	}
}

func TestTunAutoRouteRangesCustomAndExclude(t *testing.T) {
	cfg := testTUNCfg()
	cfg.TUN.Extra = map[string]any{
		// 自定义 route-address 时追加掩码后的 tun 段（sing-tun darwin 行为）。
		"route-address":         []any{"10.0.0.0/8"},
		"route-exclude-address": "10.1.0.0/16",
	}
	params := tunCreateParams(cfg)
	if slices.Contains(params.AutoRouteRanges, "10.0.0.0/8") {
		t.Fatalf("exclude 应做集合差而非整段保留: %v", params.AutoRouteRanges)
	}
	if !slices.Contains(params.AutoRouteRanges, "198.18.0.0/30") {
		t.Fatalf("自定义 route-address 时应追加掩码后的 tun 段: %v", params.AutoRouteRanges)
	}
	// 差集结果应覆盖 10.0.0.0/8 去掉 10.1.0.0/16 后的空间：含低位段与高位段。
	joined := strings.Join(params.AutoRouteRanges, ",")
	if !strings.Contains(joined, "10.0.0.0/16") || !strings.Contains(joined, "10.2.0.0/15") {
		t.Fatalf("差集结果不符合预期: %v", params.AutoRouteRanges)
	}

	// 默认子段场景：exclude 精确命中一条子段时该段被移除。
	cfg = testTUNCfg()
	cfg.TUN.Extra = map[string]any{"inet4-route-exclude-address": "8.0.0.0/5"}
	params = tunCreateParams(cfg)
	if slices.Contains(params.AutoRouteRanges, "8.0.0.0/5") {
		t.Fatalf("被 exclude 完整覆盖的子段应移除: %v", params.AutoRouteRanges)
	}
	if len(params.AutoRouteRanges) != 7 {
		t.Fatalf("默认八条子段移除一条应为七条: %v", params.AutoRouteRanges)
	}
}

func TestInjectTUNFD(t *testing.T) {
	cfg := testTUNCfg()
	if got := injectTUNFD(cfg, 0); got != cfg {
		t.Fatal("fd=0 应原样返回配置指针")
	}
	out := injectTUNFD(cfg, 42)
	if out == cfg {
		t.Fatal("注入 fd 应返回副本，不修改原配置")
	}
	if out.TUN.Extra["file-descriptor"] != 42 {
		t.Fatalf("file-descriptor 注入失败: %v", out.TUN.Extra)
	}
	if _, ok := cfg.TUN.Extra["file-descriptor"]; ok {
		t.Fatal("原配置的 Extra 不应被写入 file-descriptor")
	}

	// TUN 未开启时不注入。
	disabled := &config.Config{}
	disabled.TUN = config.DefaultTUNConfig()
	if got := injectTUNFD(disabled, 42); got != disabled {
		t.Fatal("TUN 未开启应原样返回配置指针")
	}
}

func TestEnsureAndReleaseTUNFD(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("fd 申请路径仅 darwin 非 root 生效")
	}
	if isRoot() {
		t.Skip("root 下 ensureTUNFDLocked 不经过 helper")
	}

	originalRequest, originalClose := requestTUNFD, closeTUNFD
	t.Cleanup(func() { requestTUNFD, closeTUNFD = originalRequest, originalClose })

	a := &App{}
	cfg := testTUNCfg()

	// 申请失败：错误带 helper 安装指引，不持有 fd。
	requestTUNFD = func(tunhelper.CreateTUNParams) (int, string, error) {
		return -1, "", errors.New("dial failed")
	}
	if _, _, err := a.ensureTUNFDLocked(cfg); err == nil || !strings.Contains(err.Error(), "helper install") {
		t.Fatalf("申请失败应返回带安装指引的错误: %v", err)
	}
	if a.tunFD != 0 {
		t.Fatalf("失败后不应持有 fd，实际 %d", a.tunFD)
	}

	// 申请成功并复用；release 关闭语义。
	calls := 0
	requestTUNFD = func(tunhelper.CreateTUNParams) (int, string, error) {
		calls++
		return 42, "utun9", nil
	}
	fd, acquired, err := a.ensureTUNFDLocked(cfg)
	if err != nil || fd != 42 || !acquired {
		t.Fatalf("首次申请: fd=%d acquired=%t err=%v", fd, acquired, err)
	}
	fd, acquired, err = a.ensureTUNFDLocked(cfg)
	if err != nil || fd != 42 || acquired {
		t.Fatalf("已持有应复用不重复申请: fd=%d acquired=%t err=%v", fd, acquired, err)
	}
	if calls != 1 {
		t.Fatalf("helper 应只被调用一次，实际 %d", calls)
	}

	closed := 0
	closeTUNFD = func(fd int) { closed = fd }
	a.releaseTUNFDLocked(true)
	if closed != 42 || a.tunFD != 0 {
		t.Fatalf("release(closeIt=true) 应关闭并清零: closed=%d tunFD=%d", closed, a.tunFD)
	}

	// TUN 未开启时不申请。
	a.tunFD = 0
	if fd, acquired, err := a.ensureTUNFDLocked(&config.Config{}); err != nil || fd != 0 || acquired {
		t.Fatalf("TUN 关闭不应申请 fd: fd=%d acquired=%t err=%v", fd, acquired, err)
	}
}

// isRoot 报告测试进程是否 root（darwin 本机以普通用户运行，CI 可能 root）。
func isRoot() bool {
	return runtime.GOOS != "windows" && os.Geteuid() == 0
}
