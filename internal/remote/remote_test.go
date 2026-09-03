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

	k1, err := loadOrCreateServerKey(path)
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

	k2, err := loadOrCreateServerKey(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if k1.Public() != k2.Public() {
		t.Fatal("reloaded key differs from created key (token would change across restarts)")
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

	r := newForwardRunner("t", "127.0.0.1:0", tailcat.ConnBlob("tcunused"), 22, nil, dialDirect(echoAddr))
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
	m.cfg = config.RemoteConfig{Region: "302", Serve: []int{22, 8022}}

	if !m.serverConfigEqual(config.RemoteConfig{Region: "302", Serve: []int{8022, 22}}) {
		t.Fatal("same ports in different order should be equal")
	}
	if m.serverConfigEqual(config.RemoteConfig{Region: "302", Serve: []int{22}}) {
		t.Fatal("port removed should not be equal")
	}
	if m.serverConfigEqual(config.RemoteConfig{Region: "", Serve: []int{22, 8022}}) {
		t.Fatal("region changed should not be equal")
	}
	if m.serverConfigEqual(config.RemoteConfig{Region: "302", DERPMapURL: "https://x/derp.json", Serve: []int{22, 8022}}) {
		t.Fatal("derpmap changed should not be equal")
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
