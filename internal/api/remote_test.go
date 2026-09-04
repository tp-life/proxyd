package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	cfg := &config.Config{StateDir: dir, APIListen: "127.0.0.1:19091"}
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

// TestRemoteAPI 验证 remote 基础 HTTP 用例及总开关对 builtin-ssh 的联动行为。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；状态、远端、转发、端口与开关联动接口均符合契约时测试通过。
//
// 错误情况：测试服务无法监听、HTTP 请求失败、响应码或状态字段不符合预期时测试失败。
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

	// Web 使用的 remote 总开关端点必须联动 builtin-ssh；先独立开启内嵌 SSH，
	// 再关闭总开关，验证响应与持久配置不会留下“remote 关、SSH 开”的残余状态。
	code, st = remoteAPIReq(t, http.MethodPost, base+"/api/remote/builtin-ssh", map[string]bool{"enabled": true})
	if code != http.StatusOK || st["builtin_ssh"] != true {
		t.Fatalf("开启 builtin-ssh 失败: code=%d status=%v", code, st)
	}
	code, st = remoteAPIReq(t, http.MethodPost, base+"/api/remote", map[string]bool{"enabled": false})
	if code != http.StatusOK || st["enabled"] != false || st["builtin_ssh"] != false {
		t.Fatalf("remote 总开关未联动关闭 builtin-ssh: code=%d status=%v", code, st)
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
	if code != http.StatusOK {
		t.Fatalf("get remote state after adding forward: %d", code)
	}
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

// TestRemoteWebTerminalSafetyAPI 验证默认 404 与非回环二次确认门。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：仅启动回环 HTTP 测试服务，不建立真实 shell；失败表示高权限 Web 终端
// 可能在未确认时暴露，或关闭状态仍泄露端点存在。开启后终端不再要求服务端运行或
// builtin-ssh：缺少 WebSocket 握手的普通 GET 应进入升级流程并以 426 拒绝。
func TestRemoteWebTerminalSafetyAPI(t *testing.T) {
	_, addr := newRemoteTestServer(t)
	base := "http://" + addr
	if code, _ := remoteAPIReq(t, http.MethodGet, base+"/api/remote/terminal", nil); code != http.StatusNotFound {
		t.Fatalf("默认关闭的终端端点应返回 404，got %d", code)
	}
	if code, status := remoteAPIReq(t, http.MethodPost, base+"/api/remote/web-terminal", map[string]bool{"enabled": true}); code != http.StatusOK || status["web_terminal"] != true {
		t.Fatalf("回环 API 应允许直接开启 Web 终端，code=%d status=%v", code, status)
	}
	if code, _ := remoteAPIReq(t, http.MethodGet, base+"/api/remote/terminal", nil); code != http.StatusUpgradeRequired {
		t.Fatalf("开启后缺少 WebSocket 握手的请求应进入升级流程（426），got %d", code)
	}
	if code, _ := remoteAPIReq(t, http.MethodPost, base+"/api/remote/web-terminal", map[string]bool{"enabled": false}); code != http.StatusOK {
		t.Fatalf("关闭 Web 终端失败: %d", code)
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	application, err := app.New(&config.Config{StateDir: dir, APIListen: "0.0.0.0:19091"}, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	server := New("127.0.0.1:0", application)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Shutdown(t.Context()) })
	nonLoopbackBase := "http://" + server.ln.Addr().String()
	if code, _ := remoteAPIReq(t, http.MethodPost, nonLoopbackBase+"/api/remote/web-terminal", map[string]bool{"enabled": true}); code != http.StatusBadRequest {
		t.Fatalf("非回环 API 未确认时必须拒绝开启，got %d", code)
	}
	if code, status := remoteAPIReq(t, http.MethodPost, nonLoopbackBase+"/api/remote/web-terminal", map[string]bool{
		"enabled":              true,
		"acknowledge_exposure": true,
	}); code != http.StatusOK || status["web_terminal"] != true || status["api_loopback"] != false {
		t.Fatalf("非回环 API 显式确认后应可开启，code=%d status=%v", code, status)
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

// TestRemoteSecurityMetadataAPI 验证 TTL/端口授权的 JSON 往返、空审计查询和参数边界。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：测试启动本机回环 HTTP 服务；监听或请求失败会直接终止用例。
func TestRemoteSecurityMetadataAPI(t *testing.T) {
	_, addr := newRemoteTestServer(t)
	base := "http://" + addr
	publicKey := tailcat.NewPrivateKey().Private.Public().String()
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	code, status := remoteAPIReq(t, "POST", base+"/api/remote/allow", map[string]any{
		"entries": []map[string]any{{
			"name":       "临时维护",
			"key":        publicKey,
			"expires_at": expiresAt.Format(time.RFC3339),
			"ports":      []int{22, 8080},
		}},
	})
	if code != http.StatusOK {
		t.Fatalf("设置带 TTL/端口的授权失败: code=%d response=%v", code, status)
	}
	entries, _ := status["allow"].([]any)
	if len(entries) != 1 {
		t.Fatalf("授权条目未返回: %v", status)
	}
	entry := entries[0].(map[string]any)
	ports, _ := entry["ports"].([]any)
	if entry["expires_at"] != expiresAt.Format(time.RFC3339) || len(ports) != 2 || status["allow_restricted"] != true {
		t.Fatalf("TTL/端口/受限模式响应错误: %v", status)
	}

	// CLI 与 Web 都调用该端点重置临时身份；响应中的手动 nodekey 及其最小授权
	// 元数据必须原样保留，避免前端用响应替换状态后看起来像“白名单被重置”。
	code, status = remoteAPIReq(t, http.MethodPost, base+"/api/remote/tempkey/reset", nil)
	if code != http.StatusOK || status["temp_key"] == "" {
		t.Fatalf("重置临时身份失败: code=%d response=%v", code, status)
	}
	entries, _ = status["allow"].([]any)
	if len(entries) != 1 {
		t.Fatalf("临时身份重置后手动 nodekey 丢失: %v", status)
	}
	entry = entries[0].(map[string]any)
	ports, _ = entry["ports"].([]any)
	if entry["key"] != publicKey || entry["expires_at"] != expiresAt.Format(time.RFC3339) || len(ports) != 2 {
		t.Fatalf("临时身份重置改写了手动授权: %v", entry)
	}

	code, audit := remoteAPIReq(t, "GET", base+"/api/remote/audit?tail=10", nil)
	if code != http.StatusOK {
		t.Fatalf("查询审计失败: code=%d response=%v", code, audit)
	}
	if entries, ok := audit["entries"].([]any); !ok || len(entries) != 0 {
		t.Fatalf("初始审计应为空数组: %v", audit)
	}
	if code, _ = remoteAPIReq(t, "GET", base+"/api/remote/audit?tail=501", nil); code != http.StatusBadRequest {
		t.Fatalf("越界 tail 应返回 400，got %d", code)
	}
	if code, _ = remoteAPIReq(t, "POST", base+"/api/remote/ping", map[string]string{"remote": "missing"}); code != http.StatusBadRequest {
		t.Fatalf("未知远端探测应返回 400，got %d", code)
	}
}

// TestRemoteKeyFileAPI 验证专用下载端点与原始 JSON 导入端点不会泄露到普通状态接口。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：测试启动回环 HTTP 服务；请求、读取或 JSON 解析错误会终止用例。
func TestRemoteKeyFileAPI(t *testing.T) {
	_, addr := newRemoteTestServer(t)
	base := "http://" + addr

	response, err := http.Get(base + "/api/remote/keyfile/export")
	if err != nil {
		t.Fatal(err)
	}
	exported, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("导出密钥失败: status=%d err=%v", response.StatusCode, readErr)
	}
	if !strings.Contains(response.Header.Get("Content-Disposition"), "attachment") || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("敏感下载响应头不完整: %v", response.Header)
	}
	var exportedKey tailcat.PrivateKey
	if err := json.Unmarshal(exported, &exportedKey); err != nil || exportedKey.Private.IsZero() {
		t.Fatalf("导出文件格式无效: %v", err)
	}

	invalidResponse, err := http.Post(base+"/api/remote/keyfile/import", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = invalidResponse.Body.Close()
	if invalidResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法导入应返回 400，got %d", invalidResponse.StatusCode)
	}

	importedKey := tailcat.NewPrivateKey()
	imported, err := json.Marshal(importedKey)
	if err != nil {
		t.Fatal(err)
	}
	importResponse, err := http.Post(base+"/api/remote/keyfile/import", "application/json", bytes.NewReader(imported))
	if err != nil {
		t.Fatal(err)
	}
	defer importResponse.Body.Close()
	if importResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(importResponse.Body)
		t.Fatalf("合法导入失败: status=%d body=%s", importResponse.StatusCode, body)
	}
}
