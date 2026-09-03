package app

import (
	"net"
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
