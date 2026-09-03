package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tailscale/tailcat"

	"proxyd/internal/app"
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

// newRemoteTestServer 搭建带临时 state-dir 与配置文件的 API 服务。
// 测试不开启 remote.enabled，因此不会真正连 DERP 网络。
func newRemoteTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{StateDir: dir}
	a, err := app.New(cfg, filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	server := New("127.0.0.1:0", a)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Shutdown(t.Context()) })
	return server, server.ln.Addr().String()
}

// remoteAPIReq 发送 JSON 请求并返回状态码与解析后的响应体。
func remoteAPIReq(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// freeListenPort 返回一个当前空闲的回环端口号。
func freeListenPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestRemoteAPI(t *testing.T) {
	_, addr := newRemoteTestServer(t)
	base := "http://" + addr

	// 初始状态：关闭、无 token、空列表。
	code, st := remoteAPIReq(t, "GET", base+"/api/remote", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/remote: %d", code)
	}
	if st["enabled"] != false || st["running"] != false {
		t.Fatalf("initial status: %v", st)
	}

	// 新增远端（合法 token），列表中应打码。
	token := newRemoteTestToken(t)
	code, _ = remoteAPIReq(t, "POST", base+"/api/remote/remotes", map[string]string{"name": "nas", "token": token})
	if code != http.StatusCreated {
		t.Fatalf("add remote: %d", code)
	}
	code, list := remoteAPIReq(t, "GET", base+"/api/remote/remotes", nil)
	if code != http.StatusOK {
		t.Fatalf("list remotes: %d", code)
	}
	remotes, _ := list["remotes"].([]any)
	if len(remotes) != 1 {
		t.Fatalf("remotes: %v", list)
	}
	masked := remotes[0].(map[string]any)["token"].(string)
	if strings.Contains(masked, token[6:20]) || !strings.Contains(masked, "…") {
		t.Fatalf("token should be masked, got %q", masked)
	}

	// 非法 token 拒绝。
	code, _ = remoteAPIReq(t, "POST", base+"/api/remote/remotes", map[string]string{"name": "bad", "token": "tc!!!"})
	if code != http.StatusBadRequest {
		t.Fatalf("bad token accepted: %d", code)
	}

	// 重名拒绝。
	code, _ = remoteAPIReq(t, "POST", base+"/api/remote/remotes", map[string]string{"name": "nas", "token": token})
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate name accepted: %d", code)
	}

	// 新增转发（引用远端名称），应出现在状态里且运行中。
	port := freeListenPort(t)
	fwd := map[string]any{"name": "nas-ssh", "listen": fmt.Sprintf("127.0.0.1:%d", port), "remote": "nas", "remote_port": 22}
	code, _ = remoteAPIReq(t, "POST", base+"/api/remote/forwards", fwd)
	if code != http.StatusCreated {
		t.Fatalf("add forward: %d", code)
	}
	code, st = remoteAPIReq(t, "GET", base+"/api/remote", nil)
	forwards, _ := st["forwards"].([]any)
	if len(forwards) != 1 {
		t.Fatalf("forwards: %v", st)
	}
	f0 := forwards[0].(map[string]any)
	if f0["running"] != true || f0["enabled"] != true {
		t.Fatalf("forward should be running: %v", f0)
	}

	// 停用转发。
	code, st = remoteAPIReq(t, "PUT", base+"/api/remote/forwards/nas-ssh", map[string]bool{"enabled": false})
	if code != http.StatusOK {
		t.Fatalf("disable forward: %d", code)
	}
	f0 = st["forwards"].([]any)[0].(map[string]any)
	if f0["running"] != false || f0["enabled"] != false {
		t.Fatalf("forward should be stopped: %v", f0)
	}

	// 引用不存在的远端名称拒绝。
	code, _ = remoteAPIReq(t, "POST", base+"/api/remote/forwards", map[string]any{"name": "x", "listen": "127.0.0.1:29999", "remote": "ghost", "remote_port": 22})
	if code != http.StatusBadRequest {
		t.Fatalf("forward with unknown remote accepted: %d", code)
	}

	// 删除转发与远端。
	code, _ = remoteAPIReq(t, "DELETE", base+"/api/remote/forwards/nas-ssh", nil)
	if code != http.StatusNoContent {
		t.Fatalf("del forward: %d", code)
	}
	code, _ = remoteAPIReq(t, "DELETE", base+"/api/remote/remotes/nas", nil)
	if code != http.StatusNoContent {
		t.Fatalf("del remote: %d", code)
	}

	// serve 端口校验。
	code, _ = remoteAPIReq(t, "POST", base+"/api/remote/serve", map[string]any{"ports": []int{22, 22}})
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate serve ports accepted: %d", code)
	}
	code, _ = remoteAPIReq(t, "POST", base+"/api/remote/serve", map[string]any{"ports": []int{22, 8022}})
	if code != http.StatusOK {
		t.Fatalf("set serve: %d", code)
	}
}

func TestRemoteAPIPersistsConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{
		StateDir:      dir,
		PortRange:     [2]int{42000, 42100},
		Rules:         []string{"MATCH,PROXY"},
		Subscriptions: []config.Subscription{{Name: "a", URL: "https://example.com/sub"}},
	}
	a, err := app.New(cfg, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	server := New("127.0.0.1:0", a)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(t.Context())
	base := "http://" + server.ln.Addr().String()

	token := newRemoteTestToken(t)
	if code, _ := remoteAPIReq(t, "POST", base+"/api/remote/remotes", map[string]string{"name": "nas", "token": token}); code != http.StatusCreated {
		t.Fatalf("add remote: %d", code)
	}

	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if len(loaded.Remote.Remotes) != 1 || loaded.Remote.Remotes[0].Token != token {
		t.Fatalf("remote peer not persisted: %+v", loaded.Remote.Remotes)
	}
}

// TestRemoteAPIPeerToken 验证按名称取回已保存远端的完整 token；名称不存在时 404。
func TestRemoteAPIPeerToken(t *testing.T) {
	_, addr := newRemoteTestServer(t)
	base := "http://" + addr

	token := newRemoteTestToken(t)
	code, _ := remoteAPIReq(t, "POST", base+"/api/remote/remotes", map[string]string{"name": "nas", "token": token})
	if code != http.StatusCreated {
		t.Fatalf("add remote: %d", code)
	}

	code, out := remoteAPIReq(t, "GET", base+"/api/remote/remotes/nas/token", nil)
	if code != http.StatusOK {
		t.Fatalf("get peer token: %d", code)
	}
	if out["token"] != token {
		t.Fatalf("应返回完整 token，got %q", out["token"])
	}

	code, _ = remoteAPIReq(t, "GET", base+"/api/remote/remotes/ghost/token", nil)
	if code != http.StatusNotFound {
		t.Fatalf("未知远端应返回 404，got %d", code)
	}
}

// TestRemoteAPIForwardAutoAssign 验证 listen 为空串/"auto" 时自动分配空闲回环端口，
// 且创建响应携带落盘后的具体 listen 地址。
func TestRemoteAPIForwardAutoAssign(t *testing.T) {
	_, addr := newRemoteTestServer(t)
	base := "http://" + addr

	token := newRemoteTestToken(t)
	code, _ := remoteAPIReq(t, "POST", base+"/api/remote/remotes", map[string]string{"name": "nas", "token": token})
	if code != http.StatusCreated {
		t.Fatalf("add remote: %d", code)
	}

	listens := map[string]bool{}
	for i, listen := range []string{"auto", ""} {
		name := fmt.Sprintf("f%d", i)
		code, out := remoteAPIReq(t, "POST", base+"/api/remote/forwards",
			map[string]any{"name": name, "listen": listen, "remote": "nas", "remote_port": 22})
		if code != http.StatusCreated {
			t.Fatalf("add forward %q (listen=%q): %d, %v", name, listen, code, out)
		}
		got, _ := out["listen"].(string)
		if !strings.HasPrefix(got, "127.0.0.1:") || got == "127.0.0.1:" {
			t.Fatalf("响应应携带具体 listen 地址，got %q（完整响应 %v）", got, out)
		}
		if out["name"] != name {
			t.Fatalf("响应应携带转发对象，got %v", out)
		}
		if listens[got] {
			t.Fatalf("两次自动分配得到相同地址 %q", got)
		}
		listens[got] = true
	}
}
