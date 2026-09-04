package remote

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"

	"proxyd/internal/config"
)

// newTestToken 本地生成一个合法的连接 token（不需要网络）。
func newTestToken(t *testing.T) string {
	t.Helper()
	priv := tailcat.NewPrivateKey()
	ci := priv.Public
	ci.RegionID = 1
	return string(ci.ConnBlob())
}

func TestParseRegion(t *testing.T) {
	id, hosts, err := parseRegion("")
	if err != nil || id != 0 || hosts != nil {
		t.Fatalf("empty: got id=%d hosts=%v err=%v", id, hosts, err)
	}
	id, _, err = parseRegion("302")
	if err != nil || id != 302 {
		t.Fatalf("numeric: got id=%d err=%v", id, err)
	}
	if _, _, err = parseRegion("0"); err == nil {
		t.Fatal("region 0 should be rejected")
	}
	_, hosts, err = parseRegion("derp.example.com")
	if err != nil || len(hosts) != 1 || hosts[0] != "derp.example.com" {
		t.Fatalf("host: got hosts=%v err=%v", hosts, err)
	}
	_, hosts, err = parseRegion("a.example.com, b.example.com")
	if err != nil || len(hosts) != 2 {
		t.Fatalf("multi host: got hosts=%v err=%v", hosts, err)
	}
	if _, _, err = parseRegion("nyc"); err == nil {
		t.Fatal("bare region name should be rejected")
	}
}

func TestResolveToken(t *testing.T) {
	token := newTestToken(t)
	remotes := []config.RemotePeer{{Name: "nas", Token: token}}

	got, err := ResolveToken(remotes, "nas")
	if err != nil || got != token {
		t.Fatalf("by name: got %q err=%v", got, err)
	}
	got, err = ResolveToken(remotes, token)
	if err != nil || got != token {
		t.Fatalf("passthrough: got %q err=%v", got, err)
	}
	if _, err = ResolveToken(remotes, "missing"); err == nil {
		t.Fatal("unknown name should error")
	}
	if _, err = ResolveToken(remotes, ""); err == nil {
		t.Fatal("empty should error")
	}
}

func TestValidateToken(t *testing.T) {
	if err := ValidateToken(newTestToken(t)); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if err := ValidateToken("tc!!!not-a-token"); err == nil {
		t.Fatal("invalid token accepted")
	}
	if err := ValidateToken(""); err == nil {
		t.Fatal("empty token accepted")
	}
}

func TestServerKeyPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remote", "server.private.json")

	k1, err := loadOrCreateNodeKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file perm = %o, want 600", perm)
	}

	k2, err := loadOrCreateNodeKey(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if k1.Public() != k2.Public() {
		t.Fatal("reloaded key differs from created key (token would change across restarts)")
	}
}

func TestClientKeyPersistence(t *testing.T) {
	dir := t.TempDir()

	k1, err := LoadOrCreateClientKey(dir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, clientKeyRelPath))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file perm = %o, want 600", perm)
	}

	k2, err := LoadOrCreateClientKey(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if k1.Public() != k2.Public() {
		t.Fatal("reloaded client key differs from created key (--allow identity would change across restarts)")
	}
}

func TestManagerStatusClientKey(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	defer m.Close()

	st := m.Status()
	if st.ClientKey == "" {
		t.Fatal("Status should expose the persistent client public key")
	}
	// 两次 Status 必须返回同一公钥（白名单身份跨调用稳定）。
	if again := m.Status(); again.ClientKey != st.ClientKey {
		t.Fatal("client key changed between Status calls")
	}
}

// startEchoServer 启动一个本地 echo TCP 服务，返回地址。
func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// dialDirect 返回把隧道拨号替换为直连 echo 地址的注入 dialer。
func dialDirect(addr string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

func TestForwardRunnerEcho(t *testing.T) {
	echoAddr := startEchoServer(t)

	r := newForwardRunner("t", "127.0.0.1:0", tailcat.ConnBlob("tcunused"), 22, nil, key.NodePrivate{}, dialDirect(echoAddr))
	if err := r.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer r.stop()

	conn, err := net.Dial("tcp", r.ln.Addr().String())
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	defer conn.Close()

	msg := []byte("ping-through-forward")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo mismatch: %q", buf)
	}
}

// TestForwardRunnerStopReleasesResources 验证删除或禁用常驻转发时，已经建立的连接
// 和尚未完成的远端拨号都会被主动取消，重复停止也不会 panic。
//
// 参数说明：
//   - t: *testing.T，提供回环监听、内存连接和超时断言。
//
// 返回值说明：无。
//
// 错误情况：stop 后任一连接仍可阻塞读取、拨号 context 未取消、active 未归零，
// 或连接追踪集合未清空时测试失败。
func TestForwardRunnerStopReleasesResources(t *testing.T) {
	t.Run("active connection", func(t *testing.T) {
		upstream, peer := net.Pipe()
		defer peer.Close()
		dialStarted := make(chan struct{})
		runner := newForwardRunner(
			"active",
			"127.0.0.1:0",
			tailcat.ConnBlob("tcunused"),
			22,
			nil,
			key.NodePrivate{},
			func(context.Context) (net.Conn, error) {
				close(dialStarted)
				return upstream, nil
			},
		)
		if err := runner.start(); err != nil {
			t.Fatalf("启动转发失败: %v", err)
		}
		client, err := net.Dial("tcp", runner.ln.Addr().String())
		if err != nil {
			t.Fatalf("连接本地转发失败: %v", err)
		}
		defer client.Close()
		select {
		case <-dialStarted:
		case <-time.After(time.Second):
			t.Fatal("远端拨号未开始")
		}

		runner.stop()
		runner.stop()
		_ = client.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := client.Read(make([]byte, 1)); err == nil {
			t.Fatal("stop 后本地活动连接仍未关闭")
		}
		_ = peer.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := peer.Read(make([]byte, 1)); err == nil {
			t.Fatal("stop 后远端活动连接仍未关闭")
		}

		deadline := time.Now().Add(time.Second)
		for runner.active.Load() != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if runner.active.Load() != 0 {
			t.Fatalf("stop 后仍有 %d 个活动 handler", runner.active.Load())
		}
		// 连接集合使用独立互斥锁，与 lastErr 锁分离；这使慢连接收尾
		// 不会阻塞运行错误读取，测试也必须遵守相同的同步边界。
		runner.connMu.Lock()
		remaining := len(runner.conns)
		runner.connMu.Unlock()
		if remaining != 0 {
			t.Fatalf("stop 后连接追踪集合仍有 %d 项", remaining)
		}
	})

	t.Run("in-flight dial", func(t *testing.T) {
		dialStarted := make(chan struct{})
		dialFinished := make(chan struct{})
		runner := newForwardRunner(
			"dialing",
			"127.0.0.1:0",
			tailcat.ConnBlob("tcunused"),
			22,
			nil,
			key.NodePrivate{},
			func(ctx context.Context) (net.Conn, error) {
				close(dialStarted)
				<-ctx.Done()
				close(dialFinished)
				return nil, ctx.Err()
			},
		)
		if err := runner.start(); err != nil {
			t.Fatalf("启动转发失败: %v", err)
		}
		client, err := net.Dial("tcp", runner.ln.Addr().String())
		if err != nil {
			t.Fatalf("连接本地转发失败: %v", err)
		}
		defer client.Close()
		select {
		case <-dialStarted:
		case <-time.After(time.Second):
			t.Fatal("远端拨号未开始")
		}

		runner.stop()
		select {
		case <-dialFinished:
		case <-time.After(time.Second):
			t.Fatal("stop 未取消在途拨号")
		}
	})
}

// applyForwardsOnly 用 Enabled=false 的配置走 Apply（不触网启动隧道服务端）。
func applyForwardsOnly(t *testing.T, m *Manager, forwards []config.RemoteForward) {
	t.Helper()
	if err := m.Apply(config.RemoteConfig{Enabled: false, Forwards: forwards}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// freeTCPAddr 申请一个当前空闲的回环 listen 地址（释放后供被测代码绑定）。
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestManagerForwardReconcile(t *testing.T) {
	echoAddr := startEchoServer(t)
	m := NewManager(t.TempDir(), nil)
	defer m.Close()

	// 注入 dialer：直接操作内部 map 以避开真实隧道（runner 由 reconcile 创建后替换 dial 不可行，
	// 因此这里改用 listen 地址指向 echo 的“伪转发”验证调和逻辑本身）。
	_ = echoAddr
	fwd := config.RemoteForward{Name: "a", Listen: freeTCPAddr(t), Remote: newTestToken(t), RemotePort: 22}
	applyForwardsOnly(t, m, []config.RemoteForward{fwd})

	m.mu.Lock()
	_, ok := m.forwards["a"]
	m.mu.Unlock()
	if !ok {
		t.Fatal("forward a should be running")
	}

	st := m.Status()
	if st.Running {
		t.Fatal("server should not be running when disabled")
	}
	if len(st.Forwards) != 1 || !st.Forwards[0].Running || st.Forwards[0].Name != "a" {
		t.Fatalf("status forwards: %+v", st.Forwards)
	}

	// 同名同规格重复 Apply：runner 应保留（不重建）。
	m.mu.Lock()
	first := m.forwards["a"]
	m.mu.Unlock()
	applyForwardsOnly(t, m, []config.RemoteForward{fwd})
	m.mu.Lock()
	if m.forwards["a"] != first {
		t.Fatal("unchanged forward should keep its runner")
	}
	m.mu.Unlock()

	// 禁用后 runner 移除，但 Status 仍列出条目（Enabled=false, Running=false）。
	disabled := false
	fwd.Enabled = &disabled
	applyForwardsOnly(t, m, []config.RemoteForward{fwd})
	m.mu.Lock()
	_, ok = m.forwards["a"]
	m.mu.Unlock()
	if ok {
		t.Fatal("disabled forward should be stopped")
	}
	st = m.Status()
	if len(st.Forwards) != 1 || st.Forwards[0].Enabled || st.Forwards[0].Running {
		t.Fatalf("disabled forward status: %+v", st.Forwards)
	}

	// 删除条目后 Status 不再列出。
	applyForwardsOnly(t, m, nil)
	if st := m.Status(); len(st.Forwards) != 0 {
		t.Fatalf("forwards should be empty: %+v", st.Forwards)
	}
}

func TestManagerForwardBadListen(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	defer m.Close()

	// 占用一个端口制造监听冲突。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	fwd := config.RemoteForward{Name: "bad", Listen: ln.Addr().String(), Remote: newTestToken(t), RemotePort: 22}
	applyForwardsOnly(t, m, []config.RemoteForward{fwd})

	deadline := time.Now().Add(2 * time.Second)
	for {
		st := m.Status()
		if len(st.Forwards) == 1 && !st.Forwards[0].Running && st.Forwards[0].LastError != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected failed forward with error, got %+v", st.Forwards)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServerConfigEqual(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	m.cfg = config.RemoteConfig{Region: "302", Serve: []int{22, 8022}, Allow: []config.RemoteAllowEntry{{Key: "nodekey:a"}}}

	if !m.serverConfigEqual(config.RemoteConfig{Region: "302", Serve: []int{8022, 22}, Allow: []config.RemoteAllowEntry{{Key: "nodekey:a"}}}) {
		t.Fatal("same ports/allow in different order should be equal")
	}
	if m.serverConfigEqual(config.RemoteConfig{Region: "302", Serve: []int{22}, Allow: []config.RemoteAllowEntry{{Key: "nodekey:a"}}}) {
		t.Fatal("port removed should not be equal")
	}
	if m.serverConfigEqual(config.RemoteConfig{Region: "", Serve: []int{22, 8022}, Allow: []config.RemoteAllowEntry{{Key: "nodekey:a"}}}) {
		t.Fatal("region changed should not be equal")
	}
	if m.serverConfigEqual(config.RemoteConfig{Region: "302", DERPMapURL: "https://x/derp.json", Serve: []int{22, 8022}, Allow: []config.RemoteAllowEntry{{Key: "nodekey:a"}}}) {
		t.Fatal("derpmap changed should not be equal")
	}
	if m.serverConfigEqual(config.RemoteConfig{Region: "302", Serve: []int{22, 8022}}) {
		t.Fatal("allow removed should not be equal")
	}
	if m.serverConfigEqual(config.RemoteConfig{Region: "302", Serve: []int{22, 8022}, Allow: []config.RemoteAllowEntry{{Key: "nodekey:b"}}}) {
		t.Fatal("allow key changed should not be equal")
	}
	if m.serverConfigEqual(config.RemoteConfig{Region: "302", Serve: []int{22, 8022}, Allow: []config.RemoteAllowEntry{{Key: "nodekey:a"}}, KeyFile: "/tmp/x.private.json"}) {
		t.Fatal("key-file changed should not be equal")
	}
	if m.serverConfigEqual(config.RemoteConfig{Region: "302", Serve: []int{22, 8022}, Allow: []config.RemoteAllowEntry{{Key: "nodekey:a"}}, BuiltinSSH: true}) {
		t.Fatal("builtin-ssh changed should not be equal")
	}
	if m.serverConfigEqual(config.RemoteConfig{Region: "302", Serve: []int{22, 8022}, Allow: []config.RemoteAllowEntry{{Key: "nodekey:a"}}, AllowRestricted: true}) {
		t.Fatal("allow-restricted changed should not be equal")
	}
	// TTL、别名和端口限制由每条连接上的领域授权动态读取，变化时不应重启
	// tailcat 服务端，否则会无谓中断现有连接。
	expiresAt := time.Now().Add(time.Hour)
	if !m.serverConfigEqual(config.RemoteConfig{Region: "302", Serve: []int{22, 8022}, Allow: []config.RemoteAllowEntry{{Name: "renamed", Key: "nodekey:a", ExpiresAt: &expiresAt, Ports: []int{22}}}}) {
		t.Fatal("allow metadata changes should not restart the server")
	}
}

func TestAutoRegionSticky(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	cached := &tailcfg.DERPRegion{RegionID: 302, RegionCode: "sfo"}
	m.autoRegion = cached
	m.autoRegionMapURL = ""

	// 缓存命中：直接返回缓存对象，不再触发网络探测（无网环境也能通过即证明）
	r, err := m.autoRegionLocked(config.RemoteConfig{})
	if err != nil {
		t.Fatalf("cached region rejected: %v", err)
	}
	if r != cached {
		t.Fatal("expected cached region to be reused (sticky), got a different object")
	}
}

func TestServerKeyPath(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	if got, want := m.serverKeyPath(config.RemoteConfig{}), filepath.Join(m.stateDir, serverKeyRelPath); got != want {
		t.Fatalf("default key path = %q, want %q", got, want)
	}
	if got := m.serverKeyPath(config.RemoteConfig{KeyFile: " /tmp/x.private.json "}); got != "/tmp/x.private.json" {
		t.Fatalf("custom key path = %q, want trimmed absolute path", got)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if got, want := m.serverKeyPath(config.RemoteConfig{KeyFile: "~/k.private.json"}), filepath.Join(home, "k.private.json"); got != want {
			t.Fatalf("~/ expansion = %q, want %q", got, want)
		}
	}
}

func TestValidateKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.private.json")
	if err := ValidateKeyFile(path); err != nil {
		t.Fatalf("missing file should pass (created on start): %v", err)
	}
	if _, err := loadOrCreateNodeKey(path); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ValidateKeyFile(path); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	bad := filepath.Join(dir, "bad.private.json")
	if err := os.WriteFile(bad, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateKeyFile(bad); err == nil {
		t.Fatal("key file without private key accepted")
	}
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateKeyFile(bad); err == nil {
		t.Fatal("garbage key file accepted")
	}
}

func TestValidateClientKey(t *testing.T) {
	priv := tailcat.NewPrivateKey()
	text := priv.Private.Public().String()

	pub, err := ValidateClientKey(text)
	if err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	if pub != priv.Private.Public() {
		t.Fatal("parsed public key differs from source key")
	}
	if _, err = ValidateClientKey("nodekey:bad"); err == nil {
		t.Fatal("garbage key should be rejected")
	}
	if _, err = ValidateClientKey(""); err == nil {
		t.Fatal("empty key should be rejected")
	}
}

func TestParseAndGenerateClientKey(t *testing.T) {
	privText, pubText := GenerateClientKey()

	priv, err := ParseClientKey(privText)
	if err != nil {
		t.Fatalf("generated private key rejected: %v", err)
	}
	pub, err := ValidateClientKey(pubText)
	if err != nil {
		t.Fatalf("generated public key rejected: %v", err)
	}
	if priv.Public() != pub {
		t.Fatal("generated keypair mismatch: private does not correspond to public")
	}
	if _, err = ParseClientKey("nodekey:bad"); err == nil {
		t.Fatal("garbage private key should be rejected")
	}
}

func TestTempKeyLifecycle(t *testing.T) {
	dir := t.TempDir()

	// 未生成时读取报错（不自动创建）。
	if _, _, err := LoadTempKey(dir); err == nil {
		t.Fatal("missing temp key should error")
	}

	// 重置生成：返回公钥，私钥落盘 0600，读回密钥对一致。
	pub, err := ResetTempKey(dir)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, tempKeyRelPath))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("temp key file perm = %o, want 600", perm)
	}
	privText, pubText, err := LoadTempKey(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if pubText != pub {
		t.Fatal("loaded public key differs from reset result")
	}
	priv, err := ParseClientKey(privText)
	if err != nil || priv.Public().String() != pub {
		t.Fatal("loaded private key does not match public key")
	}

	// 再次重置：公钥必须变化（旧私钥失效）。
	pub2, err := ResetTempKey(dir)
	if err != nil {
		t.Fatalf("second reset: %v", err)
	}
	if pub2 == pub {
		t.Fatal("reset should generate a fresh identity")
	}
}

func TestKnownClientAddrs(t *testing.T) {
	privText, pubText := GenerateClientKey()
	priv, _ := ParseClientKey(privText)

	known := knownClientAddrs([]string{pubText}, "")
	want := tcAddrForKey(priv.Public())
	var got string
	for addr, text := range known {
		if addr == want {
			got = text
		}
	}
	if got != pubText {
		t.Fatalf("allow key not mapped to its tunnel addr (got %q, want %q)", got, pubText)
	}
	if _, ok := known[want]; !ok {
		t.Fatal("derived addr missing from map")
	}
	// 非法公钥静默跳过，不影响其他条目。
	bad := knownClientAddrs([]string{"nodekey:bad", pubText}, "")
	if len(bad) != 1 {
		t.Fatalf("bad key should be skipped, got %d entries", len(bad))
	}
}

// Example 演示最小用法：配置一个转发条目交给 Manager 调和。
func ExampleManager_Apply() {
	m := NewManager(os.TempDir(), nil)
	defer m.Close()
	// Enabled=false 时不启动隧道服务端，仅调和转发；此处转发为空，幂等无操作。
	if err := m.Apply(config.RemoteConfig{Enabled: false}); err != nil {
		fmt.Println(err)
	}
	fmt.Println(m.Status().Running)
	// Output: false
}
