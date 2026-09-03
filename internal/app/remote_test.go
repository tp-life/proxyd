package app

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/tailcat"

	"proxyd/internal/config"
)

// newRemoteTestToken 本地生成合法的 tailcat 连接 token（不触网）。
func newRemoteTestToken(t *testing.T) string {
	t.Helper()
	priv := tailcat.NewPrivateKey()
	ci := priv.Public
	ci.RegionID = 1
	return string(ci.ConnBlob())
}

// remoteForwardPort 取出规范化 listen 地址（127.0.0.1:<port>）中的端口号。
func remoteForwardPort(t *testing.T, listen string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		t.Fatalf("listen %q 非法: %v", listen, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("listen %q 端口非法: %v", listen, err)
	}
	return p
}

// TestAddRemoteForwardAutoAssign 验证 listen 为空/"auto" 时的本地端口自动分配：
// 两次分配得到不同端口；候选段中被占用的端口会被跳过；落盘配置保存具体地址。
func TestAddRemoteForwardAutoAssign(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	a, err := New(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopRemote()

	token := newRemoteTestToken(t)
	if err := a.AddRemotePeer(config.RemotePeer{Name: "nas", Token: token}); err != nil {
		t.Fatalf("add peer: %v", err)
	}

	// 先占住候选段起始端口，验证分配会跳过它。
	held, err := net.Listen("tcp", "127.0.0.1:10022")
	if err == nil {
		defer held.Close()
	}

	f1, err := a.AddRemoteForward(config.RemoteForward{Name: "f1", Listen: "auto", Remote: "nas", RemotePort: 22})
	if err != nil {
		t.Fatalf("assign f1: %v", err)
	}
	f2, err := a.AddRemoteForward(config.RemoteForward{Name: "f2", Listen: "", Remote: "nas", RemotePort: 22})
	if err != nil {
		t.Fatalf("assign f2: %v", err)
	}

	for _, f := range []config.RemoteForward{f1, f2} {
		if !strings.HasPrefix(f.Listen, "127.0.0.1:") {
			t.Fatalf("自动分配的 listen 应为 127.0.0.1:<port>，got %q", f.Listen)
		}
		if p := remoteForwardPort(t, f.Listen); p < 10022 || p > 10121 {
			t.Fatalf("自动分配端口 %d 超出候选段 10022-10121", p)
		}
	}
	if f1.Listen == f2.Listen {
		t.Fatalf("两次自动分配得到相同端口 %q", f1.Listen)
	}
	if held != nil && (f1.Listen == "127.0.0.1:10022" || f2.Listen == "127.0.0.1:10022") {
		t.Fatalf("被占用的 10022 不应被分配: %q, %q", f1.Listen, f2.Listen)
	}

	// 返回值必须与落盘配置一致（具体 listen，而非 "auto"）。
	got := a.Config().Remote.Forwards
	if len(got) != 2 || got[0].Listen != f1.Listen || got[1].Listen != f2.Listen {
		t.Fatalf("落盘配置与返回不一致: %+v vs %q/%q", got, f1.Listen, f2.Listen)
	}
	for _, f := range got {
		if f.Listen == "" || f.Listen == "auto" {
			t.Fatalf("配置不应保留自动分配占位符: %q", f.Listen)
		}
	}
}

// TestAddRemoteForwardPrivilegedPort 验证非 Windows 平台创建 <1024 监听端口的
// 转发时在入口即被拒绝（而不是运行时才 bind 失败）。
func TestAddRemoteForwardPrivilegedPort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 无特权端口限制")
	}
	cfg := &config.Config{StateDir: t.TempDir()}
	a, err := New(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopRemote()

	token := newRemoteTestToken(t)
	if err := a.AddRemotePeer(config.RemotePeer{Name: "nas", Token: token}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	if _, err := a.AddRemoteForward(config.RemoteForward{Name: "low", Listen: "127.0.0.1:222", Remote: "nas", RemotePort: 22}); err == nil {
		t.Fatal("特权端口应在创建时被拒绝")
	}
	if _, err := a.AddRemoteForward(config.RemoteForward{Name: "ok", Listen: "127.0.0.1:2222", Remote: "nas", RemotePort: 22}); err != nil {
		t.Fatalf("≥1024 端口应可创建: %v", err)
	}
}

// TestResetRemoteTempKey 验证临时身份重置：生成密钥对、公钥写入配置 temp-key、
// 可读出完整密钥对，再次重置时公钥变化且不影响手动白名单或持久客户端身份。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；临时身份独立轮换且三类持久状态均保持正确时测试通过。
//
// 错误情况：临时密钥未生成/轮换、手动 allow 丢失、客户端 nodekey 改变或磁盘
// 重载结果不一致时测试失败。
func TestResetRemoteTempKey(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	cfg, err := config.Quick([]string{"https://example.invalid/tempkey-reset-test"}, "")
	if err != nil {
		t.Fatalf("构造完整测试配置失败: %v", err)
	}
	cfg.StateDir = directory
	a, err := New(cfg, configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopRemote()
	clientKeyBefore := a.RemoteStatus().ClientKey
	if clientKeyBefore == "" {
		t.Fatal("测试前应生成持久客户端 nodekey")
	}

	if _, _, err := a.RemoteTempKeyPair(); err == nil {
		t.Fatal("未生成时读取应报错")
	}
	manual := tailcat.NewPrivateKey().Private.Public().String()
	if err := a.SetRemoteAllow([]config.RemoteAllowEntry{{Key: manual}}); err != nil {
		t.Fatal(err)
	}

	pub, err := a.ResetRemoteTempKey()
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := a.Config().Remote.TempKey; got != pub {
		t.Fatalf("temp-key 未落盘: %q", got)
	}
	pub2, priv, err := a.RemoteTempKeyPair()
	if err != nil || pub2 != pub || priv == "" {
		t.Fatalf("读取密钥对失败: pub=%q err=%v", pub2, err)
	}
	// 把当前临时公钥也显式加入手动列表，覆盖最容易混淆的场景：下一次 reset
	// 只能替换 remote.temp-key，不能顺手删除 allow 中数值相同的旧 nodekey。
	if err := a.SetRemoteAllow([]config.RemoteAllowEntry{{Key: manual}, {Name: "保留旧临时身份", Key: pub}}); err != nil {
		t.Fatalf("把旧临时公钥加入手动白名单失败: %v", err)
	}
	pub3, err := a.ResetRemoteTempKey()
	if err != nil {
		t.Fatal(err)
	}
	if pub3 == pub {
		t.Fatal("重置应生成全新身份")
	}
	// 手动白名单、本机持久客户端身份与磁盘配置都必须保持不变；这三处分别覆盖
	// 用户可见列表、作为客户端连出时使用的 nodekey，以及断电重启后的恢复结果。
	if got := a.Config().Remote.Allow; len(got) != 2 || got[0].Key != manual || got[1].Key != pub {
		t.Fatalf("手动白名单被重置影响: %+v", got)
	}
	if got := a.RemoteStatus().ClientKey; got != clientKeyBefore {
		t.Fatalf("持久客户端 nodekey 被重置: before=%q after=%q", clientKeyBefore, got)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("重载持久化配置失败: %v", err)
	}
	if got := loaded.Remote.Allow; len(got) != 2 || got[0].Key != manual || got[1].Key != pub {
		t.Fatalf("磁盘中的手动白名单被重置影响: %+v", got)
	}
}

// TestSetRemoteEnabledLinksBuiltinSSH 验证 remote 总开关始终同步内嵌 SSH 开关。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；所有开关组合都符合联动规则时测试通过。
//
// 错误情况：开启未同时开启 builtin-ssh，或关闭未同时关闭时测试失败。
func TestSetRemoteEnabledLinksBuiltinSSH(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		remoteConfig := config.RemoteConfig{Enabled: !enabled, BuiltinSSH: !enabled}
		setRemoteServiceEnabled(&remoteConfig, enabled)
		if remoteConfig.Enabled != enabled || remoteConfig.BuiltinSSH != enabled {
			t.Fatalf("enabled=%v 时总开关联动结果错误: %+v", enabled, remoteConfig)
		}
	}
}

// TestSetRemoteWebTerminalSafety 验证应用层持久化开关并强制非回环暴露确认。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无。
//
// 错误情况：非回环地址未经确认仍能开启、确认后未落盘，或关闭动作被安全门阻止时测试失败。
func TestSetRemoteWebTerminalSafety(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	cfg, err := config.Quick([]string{"https://example.invalid/web-terminal-test"}, "")
	if err != nil {
		t.Fatalf("构造完整测试配置失败: %v", err)
	}
	cfg.StateDir = directory
	cfg.APIListen = "0.0.0.0:19091"
	application, err := New(cfg, configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer application.stopRemote()
	if err := application.SetRemoteWebTerminal(true, false); err == nil || !strings.Contains(err.Error(), "必须显式确认") {
		t.Fatalf("非回环未确认时应被拒绝，got %v", err)
	}
	if application.RemoteStatus().WebTerminal {
		t.Fatal("安全门拒绝后不得改变内存配置")
	}
	if err := application.SetRemoteWebTerminal(true, true); err != nil {
		t.Fatalf("显式确认后开启失败: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("读取持久化配置失败: %v", err)
	}
	if !loaded.Remote.WebTerminal {
		t.Fatal("Web 终端开关未持久化")
	}
	if err := application.SetRemoteWebTerminal(false, false); err != nil {
		t.Fatalf("关闭动作不应要求确认: %v", err)
	}
}

// TestSetRemoteAllow 验证客户端白名单的整体替换：非法公钥被拒绝，
// 合法列表落盘进配置，清空恢复放行所有。
func TestSetRemoteAllow(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	a, err := New(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopRemote()

	if err := a.SetRemoteAllow([]config.RemoteAllowEntry{{Key: "nodekey:bad"}}); err == nil {
		t.Fatal("非法公钥应被拒绝")
	}
	pub := tailcat.NewPrivateKey().Private.Public().String()
	if err := a.SetRemoteAllow([]config.RemoteAllowEntry{{Name: "家里", Key: pub}}); err != nil {
		t.Fatalf("合法公钥应被接受: %v", err)
	}
	if got := a.Config().Remote.Allow; len(got) != 1 || got[0].Key != pub || got[0].Name != "家里" {
		t.Fatalf("白名单未落盘: %+v", got)
	}
	if !a.Config().Remote.AllowRestricted {
		t.Fatal("设置非空白名单后应进入受限模式")
	}
	if err := a.SetRemoteAllow(nil); err != nil {
		t.Fatalf("清空白名单应成功: %v", err)
	}
	if got := a.Config().Remote.Allow; len(got) != 0 {
		t.Fatalf("白名单应已清空: %+v", got)
	}
	if a.Config().Remote.AllowRestricted {
		t.Fatal("用户显式清空白名单后应恢复开放模式")
	}
}

// TestPruneExpiredRemoteAllow 验证分钟清扫只删除过期授权、保留有效授权，且最后
// 一个条目过期后仍维持受限模式，避免空白名单被 tailcat 解释为全开放。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：测试使用内存配置，不依赖网络；失败代表 TTL 清扫事务或安全语义回归。
func TestPruneExpiredRemoteAllow(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	expiredAt := now.Add(-time.Second)
	validUntil := now.Add(time.Hour)
	expiredKey := tailcat.NewPrivateKey().Private.Public().String()
	validKey := tailcat.NewPrivateKey().Private.Public().String()
	cfg := &config.Config{
		StateDir: t.TempDir(),
		Remote: config.RemoteConfig{
			AllowRestricted: true,
			Allow: []config.RemoteAllowEntry{
				{Name: "已过期", Key: expiredKey, ExpiresAt: &expiredAt, Ports: []int{22}},
				{Name: "仍有效", Key: validKey, ExpiresAt: &validUntil, Ports: []int{443}},
			},
		},
	}
	a, err := New(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopRemote()

	removed, err := a.pruneExpiredRemoteAllow(now)
	if err != nil {
		t.Fatalf("清扫过期授权失败: %v", err)
	}
	if removed != 1 {
		t.Fatalf("清扫数量 = %d, want 1", removed)
	}
	current := a.Config().Remote
	if len(current.Allow) != 1 || current.Allow[0].Key != validKey || !current.AllowRestricted {
		t.Fatalf("清扫后配置错误: %+v", current)
	}

	removed, err = a.pruneExpiredRemoteAllow(validUntil)
	if err != nil || removed != 1 {
		t.Fatalf("清扫最后一条授权失败: removed=%d err=%v", removed, err)
	}
	current = a.Config().Remote
	if len(current.Allow) != 0 || !current.AllowRestricted || current.ClientWhitelistEnabled() != true {
		t.Fatalf("最后一条过期后必须保持拒绝模式: %+v", current)
	}
	if removed, err = a.pruneExpiredRemoteAllow(validUntil.Add(time.Minute)); err != nil || removed != 0 {
		t.Fatalf("无过期项时应幂等: removed=%d err=%v", removed, err)
	}
}

// TestRemoteServerKeyImportExport 验证密钥导出格式、合法导入、0600 权限与非法输入零修改。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：仅使用临时目录且 remote 服务保持关闭；磁盘操作失败会直接终止用例。
func TestRemoteServerKeyImportExport(t *testing.T) {
	stateDir := t.TempDir()
	a, err := New(&config.Config{StateDir: stateDir}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopRemote()

	original, err := a.ExportRemoteServerKey()
	if err != nil {
		t.Fatalf("首次导出托管密钥失败: %v", err)
	}
	var originalKey tailcat.PrivateKey
	if err := json.Unmarshal(original, &originalKey); err != nil || originalKey.Private.IsZero() {
		t.Fatalf("导出内容不是 tailcat 私钥: %v", err)
	}
	if err := a.ImportRemoteServerKey([]byte(`{"Private":"invalid"}`)); err == nil {
		t.Fatal("非法密钥导入应被拒绝")
	}
	afterInvalid, err := a.ExportRemoteServerKey()
	if err != nil || string(afterInvalid) != string(original) {
		t.Fatalf("非法导入不得修改原密钥: err=%v", err)
	}

	importedKey := tailcat.NewPrivateKey()
	imported, err := json.MarshalIndent(importedKey, "", "\t")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ImportRemoteServerKey(imported); err != nil {
		t.Fatalf("合法密钥导入失败: %v", err)
	}
	exported, err := a.ExportRemoteServerKey()
	if err != nil {
		t.Fatalf("导入后导出失败: %v", err)
	}
	var actual tailcat.PrivateKey
	if err := json.Unmarshal(exported, &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Private.Public() != importedKey.Private.Public() {
		t.Fatal("导入后的服务端身份与源密钥不一致")
	}
	managedPath := filepath.Join(stateDir, "remote", "server.private.json")
	info, err := os.Stat(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	if permission := info.Mode().Perm(); permission != 0o600 {
		t.Fatalf("导入密钥权限 = %o, want 600", permission)
	}
	if a.Config().Remote.KeyFile != "" {
		t.Fatal("导入后应切换到内置托管密钥")
	}
}

// TestRemoteServerKeyImportRollback 验证配置持久化失败时恢复原密钥内容和配置选择。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：通过把 cfgPath 的父路径占成普通文件稳定制造落盘失败；导入必须返回错误。
func TestRemoteServerKeyImportRollback(t *testing.T) {
	stateDir := t.TempDir()
	blockedParent := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blockedParent, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(&config.Config{StateDir: stateDir}, filepath.Join(blockedParent, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopRemote()
	original, err := a.ExportRemoteServerKey()
	if err != nil {
		t.Fatal(err)
	}

	imported, err := json.Marshal(tailcat.NewPrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ImportRemoteServerKey(imported); err == nil {
		t.Fatal("配置持久化失败时导入应返回错误")
	}
	restored, err := a.ExportRemoteServerKey()
	if err != nil || string(restored) != string(original) {
		t.Fatalf("失败导入后原密钥未恢复: err=%v", err)
	}
}

// TestAssignRemoteForwardPortSkipUsed 验证自动分配跳过现有转发已占用的端口。
func TestAssignRemoteForwardPortSkipUsed(t *testing.T) {
	existing := []config.RemoteForward{{Name: "x", Listen: "10022"}} // 省略 host，规范化后为 127.0.0.1:10022
	listen, err := assignRemoteForwardPort(existing)
	if err != nil {
		t.Fatal(err)
	}
	if listen == "127.0.0.1:10022" {
		t.Fatalf("现有转发占用的端口不应再分配，got %q", listen)
	}
	if p := remoteForwardPort(t, listen); p < 10022 || p > 10121 {
		t.Fatalf("分配端口 %d 超出候选段", p)
	}
}
