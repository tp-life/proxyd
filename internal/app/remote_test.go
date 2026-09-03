package app

import (
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"

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
// 可读出完整密钥对，再次重置时公钥变化且不影响手动白名单。
func TestResetRemoteTempKey(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	a, err := New(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopRemote()

	if _, _, err := a.RemoteTempKeyPair(); err == nil {
		t.Fatal("未生成时读取应报错")
	}
	manual := tailcat.NewPrivateKey().Private.Public().String()
	if err := a.SetRemoteAllow([]string{manual}); err != nil {
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
	pub3, err := a.ResetRemoteTempKey()
	if err != nil {
		t.Fatal(err)
	}
	if pub3 == pub {
		t.Fatal("重置应生成全新身份")
	}
	// 手动白名单条目不受重置影响。
	if got := a.Config().Remote.Allow; len(got) != 1 || got[0] != manual {
		t.Fatalf("手动白名单被重置影响: %+v", got)
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

	if err := a.SetRemoteAllow([]string{"nodekey:bad"}); err == nil {
		t.Fatal("非法公钥应被拒绝")
	}
	pub := tailcat.NewPrivateKey().Private.Public().String()
	if err := a.SetRemoteAllow([]string{pub}); err != nil {
		t.Fatalf("合法公钥应被接受: %v", err)
	}
	if got := a.Config().Remote.Allow; len(got) != 1 || got[0] != pub {
		t.Fatalf("白名单未落盘: %+v", got)
	}
	if err := a.SetRemoteAllow(nil); err != nil {
		t.Fatalf("清空白名单应成功: %v", err)
	}
	if got := a.Config().Remote.Allow; len(got) != 0 {
		t.Fatalf("白名单应已清空: %+v", got)
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
