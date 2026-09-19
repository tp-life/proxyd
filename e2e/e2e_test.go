// Package e2e 端到端验证 proxyd 主流程：
// 本地订阅源（Clash YAML）→ 合并 → 健康检测 → 端口分配 → 内嵌 mihomo 运行，
// 最后通过不同映射端口发 HTTP 请求，验证各自走到正确的出口节点。
package e2e

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"proxyd/internal/api"
	"proxyd/internal/app"
	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/pool"
)

// fakeSocks5 是一个最小 SOCKS5 服务器：无论客户端请求连接哪里，
// 都固定转发到 fixedTarget。用于区分"流量走了哪个节点"。
func fakeSocks5(t *testing.T, fixedTarget string) (addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSocks5(conn, fixedTarget)
		}
	}()
	return ln.Addr().String()
}

func handleSocks5(conn net.Conn, fixedTarget string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	// greeting
	if _, err := br.ReadByte(); err != nil { // ver
		return
	}
	nmethods, err := br.ReadByte()
	if err != nil {
		return
	}
	if _, err := br.Discard(int(nmethods)); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil { // no auth
		return
	}
	// request
	head := make([]byte, 4)
	if _, err := io.ReadFull(br, head); err != nil {
		return
	}
	atyp := head[3]
	var host string
	switch atyp {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 0x03:
		l, err := br.ReadByte()
		if err != nil {
			return
		}
		b := make([]byte, int(l))
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = string(b)
	case 0x04:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		return
	}
	portB := make([]byte, 2)
	if _, err := io.ReadFull(br, portB); err != nil {
		return
	}
	_ = host
	_ = binary.BigEndian.Uint16(portB)

	upstream, err := net.DialTimeout("tcp", fixedTarget, 5*time.Second)
	if err != nil {
		conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, br); done <- struct{}{} }()
	go func() { io.Copy(conn, upstream); done <- struct{}{} }()
	<-done
}

// authHTTPProxy 是一个带 Basic 认证的最小 HTTP 代理（上游）：
// Proxy-Authorization 不匹配返回 407；匹配则固定转发/隧道到 target（echo 服务器地址）。
// 同时支持 absolute-form GET 与 CONNECT（mihomo http 出站统一走 CONNECT）。
func authHTTPProxy(t *testing.T, user, pass, target string) (proxyAddr string) {
	t.Helper()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	targetAddr := strings.TrimPrefix(target, "http://")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != want {
			w.Header().Set("Proxy-Authenticate", `Basic realm="proxyd-e2e"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		if r.Method == http.MethodConnect {
			hj, ok := w.(http.Hijacker)
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			upstream, err := net.DialTimeout("tcp", targetAddr, 5*time.Second)
			if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			conn, buf, err := hj.Hijack()
			if err != nil {
				upstream.Close()
				return
			}
			_, _ = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = buf.Flush()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(upstream, conn); done <- struct{}{} }()
			go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
			go func() { <-done; conn.Close(); upstream.Close() }()
			return
		}
		req2, err := http.NewRequest(r.Method, target+r.URL.RequestURI(), r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		resp, err := http.DefaultClient.Do(req2)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// freePort 拿一个空闲端口（随即释放，测试内竞争可忽略）。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestEndToEnd(t *testing.T) {
	// 两个"出口节点"：各自固定转发到不同的 echo 服务器
	echoA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "via-A")
	}))
	defer echoA.Close()
	echoB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "via-B")
	}))
	defer echoB.Close()

	socksA := fakeSocks5(t, echoA.Listener.Addr().String())
	socksB := fakeSocks5(t, echoB.Listener.Addr().String())
	_, portAStr, _ := net.SplitHostPort(socksA)
	_, portBStr, _ := net.SplitHostPort(socksB)

	// 手动节点测试用的 echo/认证上游代理提前创建并持有端口，
	// 避免之后 freePort 选出的映射区间被它们抢占（bind 冲突）。
	echoC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "via-C")
	}))
	defer echoC.Close()
	authProxyAddr := authHTTPProxy(t, "e2e-user", "e2e-pass", echoC.URL)

	// 本地订阅源：Clash YAML，含两个 socks5 节点 + 一个死节点
	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `proxies:
  - name: node-a
    type: socks5
    server: 127.0.0.1
    port: %s
    udp: true
  - name: node-b
    type: socks5
    server: 127.0.0.1
    port: %s
    udp: true
  - name: node-dead
    type: socks5
    server: 127.0.0.1
    port: 1
`, portAStr, portBStr)
	}))
	defer sub.Close()

	stateDir := t.TempDir()
	lo := freePort(t)
	mixedPort := freePort(t)
	for mixedPort >= lo && mixedPort <= lo+4 { // 保证主端口在映射区间外
		mixedPort = freePort(t)
	}
	apiPort := freePort(t)
	for apiPort >= lo && apiPort <= lo+4 || apiPort == mixedPort {
		apiPort = freePort(t)
	}
	apiAddr := fmt.Sprintf("127.0.0.1:%d", apiPort)
	cfg := &config.Config{
		Subscriptions:   []config.Subscription{{Name: "test", URL: sub.URL, Type: "clash"}},
		Listen:          "127.0.0.1",
		PortRange:       [2]int{lo, lo + 4},
		MixedPort:       mixedPort,
		RefreshInterval: config.Duration(time.Hour),
		HealthInterval:  config.Duration(time.Hour),
		HealthURL:       "http://127.0.0.1:1/never", // 健康检测走节点出口，假节点转发到 echo，见下方修正
		HealthTimeout:   config.Duration(5 * time.Second),
		Mode:            "rule",
		Rules:           []string{"MATCH,PROXY"},
		LogLevel:        "warning",
		StateDir:        stateDir,
		APIListen:       apiAddr,
	}
	// 健康检测 URL 必须经由节点可达：假 socks5 无视目标固定转发到 echo，
	// 但 node-a/node-b 指向不同 echo，无法用同一个 URL 区分——统一用 echoA。
	cfg.HealthURL = echoA.URL + "/generate_204"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	a, err := app.New(cfg, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := a.Refresh(ctx, true); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	assigns := a.Assignments()
	if len(assigns) != 2 {
		t.Fatalf("expected 2 mapped ports (dead node filtered), got %d: %+v", len(assigns), assigns)
	}
	portOf := map[string]int{}
	for _, as := range assigns {
		portOf[as.Node.Name] = as.Port
	}
	if portOf["node-a"] == 0 || portOf["node-b"] == 0 {
		t.Fatalf("missing mapping: %+v", assigns)
	}

	// 通过两个映射端口请求任意 URL，应分别得到 echoA / echoB 的响应
	tryVia := func(port int, target string) (string, error) {
		proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
		client := &http.Client{
			Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
			Timeout:   10 * time.Second,
		}
		resp, err := client.Get(target)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body), nil
	}
	getVia := func(port int, target string) string {
		body, err := tryVia(port, target)
		if err != nil {
			t.Fatalf("GET via port %d: %v", port, err)
		}
		return body
	}
	if got := getVia(portOf["node-a"], "http://example.invalid/"); got != "via-A" {
		t.Errorf("port %d (node-a) got %q, want via-A", portOf["node-a"], got)
	}
	if got := getVia(portOf["node-b"], "http://example.invalid/"); got != "via-B" {
		t.Errorf("port %d (node-b) got %q, want via-B", portOf["node-b"], got)
	}

	// 主端口（MATCH,PROXY → PROXY select 组默认选第一个节点）必须也能通
	if got := getVia(cfg.MixedPort, "http://example.invalid/"); got != "via-A" && got != "via-B" {
		t.Errorf("main mixed port got %q, want via-A or via-B", got)
	}

	// 快照文件已写入，且映射稳定（再次分配端口不变）
	snap, err := pool.LoadSnapshot(filepath.Join(stateDir, "mapping.json"))
	if err != nil || len(snap.Mapping) != 2 {
		t.Fatalf("snapshot: %v %+v", err, snap)
	}
	alive := make([]*node.Node, 0)
	for _, n := range a.Nodes() {
		if n.Alive {
			alive = append(alive, n)
		}
	}
	again := pool.Allocate(alive, cfg.PortRange[0], cfg.PortRange[1], snap)
	for _, as := range again {
		if portOf[as.Node.Name] != as.Port {
			t.Errorf("unstable mapping: %s moved %d -> %d", as.Node.Name, portOf[as.Node.Name], as.Port)
		}
	}

	// ---- Web 控制台 / API ----
	apiSrv := api.New(cfg.APIListen, a)
	if err := apiSrv.Start(); err != nil {
		t.Fatal(err)
	}
	defer apiSrv.Shutdown(context.Background())
	base := "http://" + cfg.APIListen

	// GET / 返回内嵌控制台页面。
	// React/Vite 版入口 HTML 很薄，不能再用旧单文件 UI 的正文文案或页面体积
	// 作为契约；这里检查 React 挂载点、ES module 和构建资源引用，确保 embed 入口完整。
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `id="root"`) || !strings.Contains(string(body), `type="module"`) || !strings.Contains(string(body), "/assets/") {
		t.Errorf("UI page looks wrong (%d bytes)", len(body))
	}

	// GET /api/overview 聚合正确
	resp, err = http.Get(base + "/api/overview")
	if err != nil {
		t.Fatal(err)
	}
	var ov struct {
		Mode  string `json:"mode"`
		Ports []struct {
			Port int `json:"port"`
		} `json:"ports"`
		Nodes []struct {
			Name  string `json:"name"`
			Alive bool   `json:"alive"`
			Port  int    `json:"port"`
		} `json:"nodes"`
		Subs []struct {
			Name  string `json:"name"`
			Total int    `json:"total"`
			Alive int    `json:"alive"`
		} `json:"subscriptions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ov); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ov.Mode != "rule" || len(ov.Ports) != 2 || len(ov.Nodes) != 3 {
		t.Errorf("overview: mode=%q ports=%d nodes=%d", ov.Mode, len(ov.Ports), len(ov.Nodes))
	}
	if len(ov.Subs) != 1 || ov.Subs[0].Total != 3 || ov.Subs[0].Alive != 2 {
		t.Errorf("overview subs: %+v", ov.Subs)
	}

	// POST /api/mode 切换模式
	resp, err = http.Post(base+"/api/mode", "application/json", strings.NewReader(`{"mode":"global"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || a.Mode() != "global" {
		t.Errorf("set mode: status=%d mode=%q", resp.StatusCode, a.Mode())
	}
	// 非法模式被拒绝
	resp, err = http.Post(base+"/api/mode", "application/json", strings.NewReader(`{"mode":"wat"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("bad mode accepted: status=%d", resp.StatusCode)
	}

	// POST /api/refresh 返回 202
	resp, err = http.Post(base+"/api/refresh", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Errorf("refresh: status=%d", resp.StatusCode)
	}

	// POST /api/test（手动测速）返回 202
	resp, err = http.Post(base+"/api/test", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Errorf("test: status=%d", resp.StatusCode)
	}

	// ---- 订阅管理：添加 → 持久化到配置文件；删除 → 同步移除 ----
	resp, err = http.Post(base+"/api/subscriptions", "application/json",
		strings.NewReader(`{"url":"`+sub.URL+`/second"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("add sub: status=%d", resp.StatusCode)
	}
	// 配置已落盘
	saved, err := config.Load(cfgPath)
	if err != nil || len(saved.Subscriptions) != 2 {
		t.Fatalf("config not persisted: %v %+v", err, saved.Subscriptions)
	}
	// 重复 URL 被拒绝
	resp, _ = http.Post(base+"/api/subscriptions", "application/json",
		strings.NewReader(`{"url":"`+sub.URL+`/second"}`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("dup sub accepted: status=%d", resp.StatusCode)
	}

	// 新增只保存设置，必须先看到空节点状态；随后显式调用单订阅同步，避免测试
	// 把“新增后自动下载”重新固化为产品行为。
	secondName := saved.Subscriptions[1].Name
	var beforeManualSync struct {
		Subs []struct {
			Name  string `json:"name"`
			Total int    `json:"total"`
		} `json:"subscriptions"`
	}
	beforeResp, beforeErr := http.Get(base + "/api/overview")
	if beforeErr != nil {
		t.Fatalf("读取手动同步前概览失败: %v", beforeErr)
	}
	_ = json.NewDecoder(beforeResp.Body).Decode(&beforeManualSync)
	beforeResp.Body.Close()
	for _, subscription := range beforeManualSync.Subs {
		if subscription.Name == secondName && subscription.Total != 0 {
			t.Fatalf("新增订阅不应自动同步，手动同步前节点数=%d", subscription.Total)
		}
	}
	manualSyncResp, manualSyncErr := http.Post(
		base+"/api/subscriptions/"+url.PathEscape(secondName)+"/refresh",
		"application/json",
		nil,
	)
	if manualSyncErr != nil {
		t.Fatalf("手动同步第二订阅失败: %v", manualSyncErr)
	}
	manualSyncResp.Body.Close()
	if manualSyncResp.StatusCode != http.StatusOK {
		t.Fatalf("手动同步第二订阅状态码=%d", manualSyncResp.StatusCode)
	}

	// ---- 按订阅查看节点：第二订阅内容与第一订阅完全相同（节点全部重叠），
	// 去重后节点归属名字序最小的订阅（"127.0.0.1" < "test"）----
	{
		deadline := time.Now().Add(20 * time.Second)
		for {
			var ov struct {
				Subs []struct {
					Name  string `json:"name"`
					Total int    `json:"total"`
				} `json:"subscriptions"`
				Nodes []struct {
					Name         string `json:"name"`
					Subscription string `json:"subscription"`
				} `json:"nodes"`
			}
			r, err := http.Get(base + "/api/overview")
			if err == nil {
				_ = json.NewDecoder(r.Body).Decode(&ov)
				r.Body.Close()
			}
			if len(ov.Subs) == 2 && ov.Subs[1].Total == 3 {
				for _, n := range ov.Nodes {
					if n.Subscription != secondName {
						t.Errorf("节点 %s 归属 %q，应归属去重后首个订阅 %q", n.Name, n.Subscription, secondName)
					}
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("等待第二订阅刷新超时: %+v", ov.Subs)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	// 删除
	req, _ := http.NewRequest("DELETE", base+"/api/subscriptions/"+saved.Subscriptions[1].Name, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("del sub: status=%d", resp.StatusCode)
	}
	saved, _ = config.Load(cfgPath)
	if len(saved.Subscriptions) != 1 {
		t.Errorf("after delete: %d subs", len(saved.Subscriptions))
	}

	// 主端口模式切回 rule（此前测试切过 global），供后续规则断言使用
	resp, err = http.Post(base+"/api/mode", "application/json", strings.NewReader(`{"mode":"rule"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || a.Mode() != "rule" {
		t.Fatalf("set rule: status=%d mode=%q", resp.StatusCode, a.Mode())
	}
	// mode=auto 已被移除（改为独立 auto-port）
	resp, _ = http.Post(base+"/api/mode", "application/json", strings.NewReader(`{"mode":"auto"}`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("mode=auto 应被拒绝: status=%d", resp.StatusCode)
	}

	// ---- auto-port：独立的自动选优端口，不影响主端口规则模式 ----
	autoPort := freePort(t)
	for autoPort >= lo && autoPort <= lo+4 || autoPort == cfg.MixedPort {
		autoPort = freePort(t)
	}
	resp, err = http.Post(base+"/api/auto-port", "application/json",
		strings.NewReader(fmt.Sprintf(`{"port":%d}`, autoPort)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("set auto-port: status=%d", resp.StatusCode)
	}
	if got := getVia(autoPort, "http://example.invalid/"); got != "via-A" && got != "via-B" {
		t.Errorf("auto-port got %q, want via-A or via-B", got)
	}
	// 主端口仍是规则模式（MATCH,PROXY），不受 auto-port 影响
	if got := getVia(cfg.MixedPort, "http://example.invalid/"); got != "via-A" && got != "via-B" {
		t.Errorf("auto-port 开启后主端口 got %q", got)
	}
	// 与主端口冲突的 auto-port 被拒绝
	resp, _ = http.Post(base+"/api/auto-port", "application/json",
		strings.NewReader(fmt.Sprintf(`{"port":%d}`, cfg.MixedPort)))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("conflict auto-port accepted: status=%d", resp.StatusCode)
	}
	// 持久化
	saved, _ = config.Load(cfgPath)
	if saved.AutoPort != autoPort {
		t.Errorf("auto-port 未持久化: %d", saved.AutoPort)
	}
	// 关闭后端口不再监听
	resp, _ = http.Post(base+"/api/auto-port", "application/json", strings.NewReader(`{"port":0}`))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("close auto-port: status=%d", resp.StatusCode)
	}
	if _, err := tryVia(autoPort, "http://example.invalid/"); err == nil {
		t.Errorf("auto-port 关闭后端口 %d 仍在监听", autoPort)
	}

	// ---- 自定义规则：前置生效（指向 node-b 的规则让对应域名走 node-b 出口）----
	resp, err = http.Post(base+"/api/rules", "application/json",
		strings.NewReader(`{"rule":"DOMAIN-SUFFIX,example.invalid,node-b"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("add rule: status=%d", resp.StatusCode)
	}
	if got := getVia(cfg.MixedPort, "http://example.invalid/"); got != "via-B" {
		t.Errorf("自定义规则未生效: got %q, want via-B", got)
	}
	resp, err = http.Get(base + "/api/rules")
	if err != nil {
		t.Fatal(err)
	}
	var rules []string
	_ = json.NewDecoder(resp.Body).Decode(&rules)
	resp.Body.Close()
	if len(rules) != 1 || rules[0] != "DOMAIN-SUFFIX,example.invalid,node-b" {
		t.Errorf("GET /api/rules = %v", rules)
	}
	// 非法规则被拒绝
	resp, _ = http.Post(base+"/api/rules", "application/json", strings.NewReader(`{"rule":"bad-rule"}`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("bad rule accepted: status=%d", resp.StatusCode)
	}
	// 删除后恢复 MATCH,PROXY 行为
	req, _ = http.NewRequest("DELETE", base+"/api/rules/0", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("del rule: status=%d", resp.StatusCode)
	}
	if got := getVia(cfg.MixedPort, "http://example.invalid/"); got != "via-A" && got != "via-B" {
		t.Errorf("删规则后主端口 got %q", got)
	}
	saved, _ = config.Load(cfgPath)
	if len(saved.CustomRules) != 0 {
		t.Errorf("custom-rules 未持久化删除: %v", saved.CustomRules)
	}

	// ---- 节点分组：组端口固定走组内节点（url-test 自动选优）----
	grpPort := freePort(t)
	for grpPort >= lo && grpPort <= lo+4 || grpPort == cfg.MixedPort {
		grpPort = freePort(t)
	}
	resp, err = http.Post(base+"/api/groups", "application/json",
		strings.NewReader(fmt.Sprintf(`{"name":"g1","port":%d,"nodes":["node-b"]}`, grpPort)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("add group: status=%d", resp.StatusCode)
	}
	if got := getVia(grpPort, "http://example.invalid/"); got != "via-B" {
		t.Errorf("分组端口 %d got %q, want via-B", grpPort, got)
	}
	// 端口与节点映射区间冲突被拒绝
	resp, _ = http.Post(base+"/api/groups", "application/json",
		strings.NewReader(fmt.Sprintf(`{"name":"g2","port":%d,"nodes":["node-a"]}`, lo+1)))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("conflict group accepted: status=%d", resp.StatusCode)
	}
	saved, _ = config.Load(cfgPath)
	if len(saved.Groups) != 1 || saved.Groups[0].Port != grpPort {
		t.Errorf("groups 未持久化: %+v", saved.Groups)
	}
	req, _ = http.NewRequest("DELETE", base+"/api/groups/g1", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("del group: status=%d", resp.StatusCode)
	}

	// ---- 端口区间变更：节点端口迁移到新区间 ----
	newLo := freePort(t)
	for newLo >= lo && newLo <= lo+4 || newLo == cfg.MixedPort || newLo+1 == cfg.MixedPort || apiPort >= newLo && apiPort <= newLo+1 {
		newLo = freePort(t)
	}
	resp, err = http.Post(base+"/api/port-range", "application/json",
		strings.NewReader(fmt.Sprintf(`{"range":"%d-%d"}`, newLo, newLo+1)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("set port-range: status=%d", resp.StatusCode)
	}
	newAssigns := a.Assignments()
	if len(newAssigns) != 2 {
		t.Fatalf("port-range 变更后应有 2 个映射, got %d", len(newAssigns))
	}
	newPortOf := map[string]int{}
	for _, as := range newAssigns {
		if as.Port < newLo || as.Port > newLo+1 {
			t.Errorf("端口 %d 不在新区间 [%d, %d]", as.Port, newLo, newLo+1)
		}
		newPortOf[as.Node.Name] = as.Port
	}
	if got := getVia(newPortOf["node-a"], "http://example.invalid/"); got != "via-A" {
		t.Errorf("迁移后 node-a 端口 %d got %q, want via-A", newPortOf["node-a"], got)
	}
	saved, _ = config.Load(cfgPath)
	if saved.PortRange != [2]int{newLo, newLo + 1} {
		t.Errorf("port-range 未持久化: %v", saved.PortRange)
	}
	// 与主端口冲突的区间被拒绝
	resp, _ = http.Post(base+"/api/port-range", "application/json",
		strings.NewReader(fmt.Sprintf(`{"range":"%d-%d"}`, cfg.MixedPort, cfg.MixedPort+10)))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("conflict port-range accepted: status=%d", resp.StatusCode)
	}
	// 管理 API 不能落入节点映射区间。否则 mihomo listener 会绑定失败，
	// 节点代理请求则可能被已监听的 Web API 接收并返回控制台 HTML。
	resp, _ = http.Post(base+"/api/port-range", "application/json",
		strings.NewReader(fmt.Sprintf(`{"range":"%d-%d"}`, apiPort, apiPort)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("api-listen conflict port-range accepted: status=%d", resp.StatusCode)
	}

	// ---- 节点列表：全量节点带类型/失败原因/未分配状态 ----
	{
		r, err := http.Get(base + "/api/overview")
		if err != nil {
			t.Fatal(err)
		}
		var ov struct {
			Nodes []struct {
				Name       string `json:"name"`
				Type       string `json:"type"`
				Alive      bool   `json:"alive"`
				FailReason string `json:"fail_reason"`
				Port       int    `json:"port"`
			} `json:"nodes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&ov)
		r.Body.Close()
		var dead *struct {
			Name       string `json:"name"`
			Type       string `json:"type"`
			Alive      bool   `json:"alive"`
			FailReason string `json:"fail_reason"`
			Port       int    `json:"port"`
		}
		for i := range ov.Nodes {
			if ov.Nodes[i].Name == "node-dead" {
				dead = &ov.Nodes[i]
			}
			if ov.Nodes[i].Type != "socks5" {
				t.Errorf("节点 %s type = %q, want socks5", ov.Nodes[i].Name, ov.Nodes[i].Type)
			}
		}
		if dead == nil {
			t.Fatal("overview 缺少未通过测速的 node-dead（应展示全量节点）")
		}
		if dead.Alive || dead.FailReason == "" || dead.Port != 0 {
			t.Errorf("node-dead: alive=%v fail_reason=%q port=%d", dead.Alive, dead.FailReason, dead.Port)
		}
	}

	// ---- 规则 URL 导入：mihomo 规则文本 + gfwlist(base64) ----
	ruleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gfwlist.txt" {
			gfw := "[AutoProxy 0.2.9]\n! 注释\n||gfw-test.invalid\n@@||direct-test.invalid\n||wild*.card\n"
			fmt.Fprint(w, base64.StdEncoding.EncodeToString([]byte(gfw)))
			return
		}
		// /rules.txt：mihomo 规则文本
		fmt.Fprintln(w, "# comment\nDOMAIN-SUFFIX,imported.invalid,node-b\nINVALID-LINE")
	}))
	defer ruleSrv.Close()

	resp, err = http.Post(base+"/api/rule-urls", "application/json",
		strings.NewReader(`{"name":"txt","url":"`+ruleSrv.URL+`/rules.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("add rule-url txt: status=%d", resp.StatusCode)
	}
	resp, err = http.Post(base+"/api/rule-urls", "application/json",
		strings.NewReader(`{"name":"gfw","url":"`+ruleSrv.URL+`/gfwlist.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("add rule-url gfw: status=%d", resp.StatusCode)
	}
	// 拉取状态与条目数
	r, err := http.Get(base + "/api/rule-urls")
	if err != nil {
		t.Fatal(err)
	}
	var ruStats []struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(r.Body).Decode(&ruStats)
	r.Body.Close()
	countOf := map[string]int{}
	for _, s := range ruStats {
		if s.Error != "" {
			t.Errorf("规则源 %s 拉取失败: %s", s.Name, s.Error)
		}
		countOf[s.Name] = s.Count
	}
	if countOf["txt"] != 1 || countOf["gfw"] != 2 {
		t.Errorf("规则源条目数 = %v, want txt=1 gfw=2", countOf)
	}
	// 导入规则生效：imported.invalid 走 node-b（文本源）；gfw-test.invalid 走 PROXY（gfwlist 源）
	if got := getVia(cfg.MixedPort, "http://imported.invalid/"); got != "via-B" {
		t.Errorf("规则 URL 导入未生效: got %q, want via-B", got)
	}
	if got := getVia(cfg.MixedPort, "http://gfw-test.invalid/"); got != "via-A" && got != "via-B" {
		t.Errorf("gfwlist 导入未生效: got %q", got)
	}
	// 持久化的是 URL 而非规则内容
	saved, _ = config.Load(cfgPath)
	if len(saved.RuleURLs) != 2 {
		t.Fatalf("rule-urls 未持久化: %+v", saved.RuleURLs)
	}
	rawCfg, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(rawCfg), "imported.invalid") || strings.Contains(string(rawCfg), "gfw-test") {
		t.Error("导入的规则内容不应写入配置文件")
	}
	// 删除文本源后其规则失效
	req, _ = http.NewRequest("DELETE", base+"/api/rule-urls/txt", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("del rule-url: status=%d", resp.StatusCode)
	}
	if got := getVia(cfg.MixedPort, "http://imported.invalid/"); got == "via-B" {
		t.Errorf("删除规则源后 imported.invalid 仍走 node-b")
	}

	// ---- 内置 PROXY 组选择（默认出口）：规则保持生效，未命中流量走所选出口 ----
	// 差分验证：先加一条指向 REJECT 的自定义规则，规则模式下主端口访问该域名被 502 拦截
	//（mihomo REJECT 对 HTTP 代理请求返回 502 空响应，不断连）
	viaStatus := func(port int, target string) (int, string) {
		proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
		client := &http.Client{
			Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
			Timeout:   10 * time.Second,
		}
		resp, err := client.Get(target)
		if err != nil {
			return 0, ""
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	resp, err = http.Post(base+"/api/rules", "application/json",
		strings.NewReader(`{"rule":"DOMAIN-SUFFIX,exit-test.invalid,REJECT"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("add reject rule: status=%d", resp.StatusCode)
	}
	if st, body := viaStatus(cfg.MixedPort, "http://exit-test.invalid/"); st != http.StatusBadGateway || body != "" {
		t.Fatalf("规则模式下 REJECT 未生效: status=%d body=%q（want 502 空响应）", st, body)
	}
	// 选定 node-b 为默认出口：规则仍生效（REJECT 依旧 502），未命中流量固定走 node-b
	resp, err = http.Post(base+"/api/groups/PROXY/select", "application/json",
		strings.NewReader(`{"node":"node-b"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PROXY select node-b: status=%d", resp.StatusCode)
	}
	if st, _ := viaStatus(cfg.MixedPort, "http://exit-test.invalid/"); st != http.StatusBadGateway {
		t.Errorf("选定默认出口后 REJECT 规则未保持生效: status=%d（want 502）", st)
	}
	if got := getVia(cfg.MixedPort, "http://example.invalid/"); got != "via-B" {
		t.Errorf("选定 node-b 后未命中流量 got %q, want via-B", got)
	}
	// 非法成员被拒绝；main-node / main-auto 端点已移除
	resp, _ = http.Post(base+"/api/groups/PROXY/select", "application/json",
		strings.NewReader(`{"node":"不存在的节点"}`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("PROXY 选非法成员应被拒绝: status=%d", resp.StatusCode)
	}
	// 端点已移除：真实服务器里未匹配的非 GET 请求会落到 SPA 兜底并返回 405，
	// 纯 API mux 的单元测试则返回 404；两者都说明没有对应 handler。
	for _, removed := range []string{"/api/main-node", "/api/main-auto"} {
		resp, _ = http.Post(base+removed, "application/json", strings.NewReader(`{}`))
		resp.Body.Close()
		if resp.StatusCode != 404 && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s 端点应已移除: status=%d, want 404/405", removed, resp.StatusCode)
		}
	}
	// 节点映射端口不受影响（固定出口，不经规则与 PROXY 组）
	if got := getVia(newPortOf["node-b"], "http://exit-test.invalid/"); got != "via-B" {
		t.Errorf("PROXY 选择后映射端口 %d got %q, want via-B", newPortOf["node-b"], got)
	}
	// overview 暴露内置 PROXY 项及其选中
	{
		r, err := http.Get(base + "/api/overview")
		if err != nil {
			t.Fatal(err)
		}
		var ov struct {
			Groups []struct {
				Name     string `json:"name"`
				Selected string `json:"selected"`
				Builtin  bool   `json:"builtin"`
			} `json:"groups"`
		}
		_ = json.NewDecoder(r.Body).Decode(&ov)
		r.Body.Close()
		if len(ov.Groups) == 0 || ov.Groups[0].Name != "PROXY" || !ov.Groups[0].Builtin || ov.Groups[0].Selected != "node-b" {
			t.Errorf("overview 内置 PROXY 项异常: %+v", ov.Groups)
		}
	}
	// 选中持久化到 state-dir/group-selected.json（键 "PROXY"，值为节点名）
	{
		raw, err := os.ReadFile(filepath.Join(stateDir, "group-selected.json"))
		if err != nil {
			t.Fatalf("读取选中状态文件失败: %v", err)
		}
		// 状态文件外层包裹 selected 字段（见 internal/proxy/groupstate）。
		var state struct {
			Selected map[string]string `json:"selected"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			t.Fatalf("解析选中状态文件失败: %v", err)
		}
		if state.Selected["PROXY"] != "node-b" {
			t.Errorf("PROXY 选中未持久化: %v", state.Selected)
		}
	}
	// AUTO：未命中流量走任一最优节点，规则仍生效
	resp, _ = http.Post(base+"/api/groups/PROXY/select", "application/json", strings.NewReader(`{"node":"AUTO"}`))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PROXY select AUTO: status=%d", resp.StatusCode)
	}
	if got := getVia(cfg.MixedPort, "http://example.invalid/"); got != "via-A" && got != "via-B" {
		t.Errorf("PROXY=AUTO 后主端口 got %q", got)
	}
	if st, _ := viaStatus(cfg.MixedPort, "http://exit-test.invalid/"); st != http.StatusBadGateway {
		t.Errorf("PROXY=AUTO 后 REJECT 规则未保持生效: status=%d（want 502）", st)
	}
	// DIRECT 恒可选（只验接口与持久化，不发直连流量）
	resp, _ = http.Post(base+"/api/groups/PROXY/select", "application/json", strings.NewReader(`{"node":"DIRECT"}`))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PROXY select DIRECT: status=%d", resp.StatusCode)
	}
	// 与 auto-port 并存：PROXY=DIRECT 期间 auto-port（AUTO 组入口）不受影响
	autoPort2 := freePort(t)
	for autoPort2 >= newLo && autoPort2 <= newLo+1 || autoPort2 == cfg.MixedPort {
		autoPort2 = freePort(t)
	}
	resp, err = http.Post(base+"/api/auto-port", "application/json",
		strings.NewReader(fmt.Sprintf(`{"port":%d}`, autoPort2)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("开启 auto-port: status=%d", resp.StatusCode)
	}
	if got := getVia(autoPort2, "http://example.invalid/"); got != "via-A" && got != "via-B" {
		t.Errorf("并存时 auto-port %d got %q", autoPort2, got)
	}
	// 清理：恢复默认出口 AUTO、关 auto-port、删 REJECT 规则
	resp, _ = http.Post(base+"/api/groups/PROXY/select", "application/json", strings.NewReader(`{"node":"AUTO"}`))
	resp.Body.Close()
	resp, _ = http.Post(base+"/api/auto-port", "application/json", strings.NewReader(`{"port":0}`))
	resp.Body.Close()
	req, _ = http.NewRequest("DELETE", base+"/api/rules/0", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// ---- main-port：主端口在线修改（热更新 + 冲突校验 + 持久化）----
	oldMain := cfg.MixedPort
	newMain := freePort(t)
	for newMain >= newLo && newMain <= newLo+1 || newMain == oldMain {
		newMain = freePort(t)
	}
	resp, err = http.Post(base+"/api/main-port", "application/json",
		strings.NewReader(fmt.Sprintf(`{"port":%d}`, newMain)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("set main-port: status=%d", resp.StatusCode)
	}
	if cfg.MixedPort != newMain { // app 与测试共用同一 cfg 指针
		t.Fatalf("cfg.MixedPort = %d, want %d", cfg.MixedPort, newMain)
	}
	if got := getVia(newMain, "http://example.invalid/"); got != "via-A" && got != "via-B" {
		t.Errorf("新主端口 %d got %q", newMain, got)
	}
	if _, err := tryVia(oldMain, "http://example.invalid/"); err == nil {
		t.Errorf("旧主端口 %d 仍在监听", oldMain)
	}
	// 冲突校验：节点区间内 / api 端口
	resp, _ = http.Post(base+"/api/main-port", "application/json",
		strings.NewReader(fmt.Sprintf(`{"port":%d}`, newLo)))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("区间内 main-port 应被拒绝: status=%d", resp.StatusCode)
	}
	_, apiPortStr, _ := net.SplitHostPort(apiAddr)
	resp, _ = http.Post(base+"/api/main-port", "application/json",
		strings.NewReader(`{"port":`+apiPortStr+`}`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("与 api 端口冲突的 main-port 应被拒绝: status=%d", resp.StatusCode)
	}
	// 越界
	resp, _ = http.Post(base+"/api/main-port", "application/json", strings.NewReader(`{"port":70000}`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("越界 main-port 应被拒绝: status=%d", resp.StatusCode)
	}
	saved, _ = config.Load(cfgPath)
	if saved.MixedPort != newMain {
		t.Errorf("main-port 未持久化: %d", saved.MixedPort)
	}

	// ---- 手动节点：带认证的 http 上游代理，验证解析/落盘/测速/映射/认证透传 ----
	// 无认证直连上游应被 407 拒绝
	if resp, err := http.Get("http://" + authProxyAddr + "/"); err != nil || resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("上游代理无认证应返回 407: %v %+v", err, resp)
	} else {
		resp.Body.Close()
	}
	resp, err = http.Post(base+"/api/manual-nodes", "application/json",
		strings.NewReader(`{"url":"http://e2e-user:e2e-pass@`+authProxyAddr+`#mn-http"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("add manual node: status=%d", resp.StatusCode)
	}
	// 非法节点被拒绝
	resp, _ = http.Post(base+"/api/manual-nodes", "application/json", strings.NewReader(`{"url":"not-a-url"}`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("bad manual node accepted: status=%d", resp.StatusCode)
	}
	// 已持久化到配置文件
	saved, _ = config.Load(cfgPath)
	mnEntry := ""
	if len(saved.ManualNodes) == 1 {
		mnEntry, _ = saved.ManualNodes[0].(string)
	}
	if mnEntry == "" || !strings.Contains(mnEntry, "#mn-http") {
		t.Errorf("manual-nodes 未持久化: %+v", saved.ManualNodes)
	}
	// 等待异步刷新把节点纳入池（参与测速与端口分配）
	mnPort := 0
	{
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			r, err := http.Get(base + "/api/overview")
			if err == nil {
				var ov struct {
					Nodes []struct {
						Name         string `json:"name"`
						Subscription string `json:"subscription"`
						Alive        bool   `json:"alive"`
						Port         int    `json:"port"`
					} `json:"nodes"`
					ManualNodes []struct {
						Index int    `json:"index"`
						Name  string `json:"name"`
					} `json:"manual_nodes"`
				}
				_ = json.NewDecoder(r.Body).Decode(&ov)
				r.Body.Close()
				if len(ov.ManualNodes) != 1 || ov.ManualNodes[0].Name != "mn-http" {
					t.Errorf("overview manual_nodes: %+v", ov.ManualNodes)
				}
				for _, n := range ov.Nodes {
					if n.Name == "mn-http" && n.Alive && n.Port != 0 {
						if n.Subscription != "manual" {
							t.Errorf("mn-http 归属 %q，应为 manual", n.Subscription)
						}
						mnPort = n.Port
					}
				}
				if mnPort != 0 {
					break
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	if mnPort == 0 {
		t.Fatal("手动节点 mn-http 未通过测速/未分配端口")
	}
	// 认证透传：经映射端口访问，上游校验 Proxy-Authorization 后转发到 echoC
	if got := getVia(mnPort, "http://example.invalid/"); got != "via-C" {
		t.Errorf("手动 http 节点端口 %d got %q, want via-C（认证透传失败）", mnPort, got)
	}
	// 删除
	req, _ = http.NewRequest("DELETE", base+"/api/manual-nodes/0", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("del manual node: status=%d", resp.StatusCode)
	}
	saved, _ = config.Load(cfgPath)
	if len(saved.ManualNodes) != 0 {
		t.Errorf("manual-nodes 未持久化删除: %+v", saved.ManualNodes)
	}

	// ---- CLI 作为本地 API 客户端：编译真实二进制打运行中实例 ----
	// 与 Makefile/GoReleaser 发布保持一致：with_gvisor 覆盖 VPN/TUN 能力，
	// ts_omit_acme 裁剪未使用的证书入口并避免双 Tailscale 全局指标冲突。
	binPath := filepath.Join(t.TempDir(), "proxyd")
	if out, err := exec.Command("go", "build", "-tags", "with_gvisor ts_omit_acme", "-o", binPath, "../cmd/proxyd").CombinedOutput(); err != nil {
		t.Fatalf("go build proxyd: %v\n%s", err, out)
	}
	runCLI := func(args ...string) (string, error) {
		full := append([]string{args[0], "-c", cfgPath}, args[1:]...) // flag 须在位置参数之前
		cmd := exec.Command(binPath, full...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := runCLI("mode"); err != nil || !strings.Contains(out, "rule") {
		t.Errorf("cli mode: %v %q", err, out)
	}
	if out, err := runCLI("mode", "global"); err != nil || a.Mode() != "global" {
		t.Errorf("cli mode set: %v %q mode=%q", err, out, a.Mode())
	}
	if _, err := runCLI("mode", "rule"); err != nil {
		t.Errorf("cli mode back: %v", err)
	}
	// 非法模式：API 报错原样透传
	if out, err := runCLI("mode", "bogus"); err == nil || !strings.Contains(out, "invalid mode") {
		t.Errorf("cli 应透传 API 报错: %v %q", err, out)
	}
	if out, err := runCLI("nodes"); err != nil || !strings.Contains(out, "node-a") {
		t.Errorf("cli nodes: %v %q", err, out)
	}
	if out, err := runCLI("subs", "list"); err != nil || !strings.Contains(out, "test") {
		t.Errorf("cli subs list: %v %q", err, out)
	}
	if out, err := runCLI("rules", "add", "DOMAIN-SUFFIX,cli-e2e.invalid,node-b"); err != nil {
		t.Errorf("cli rules add: %v %q", err, out)
	}
	if out, err := runCLI("rules", "list"); err != nil || !strings.Contains(out, "cli-e2e.invalid") {
		t.Errorf("cli rules list: %v %q", err, out)
	}
	if _, err := runCLI("rules", "del", "0"); err != nil {
		t.Errorf("cli rules del: %v", err)
	}
	if out, err := runCLI("rules", "list"); err != nil || strings.Contains(out, "cli-e2e.invalid") {
		t.Errorf("cli rules 删除后仍残留: %v %q", err, out)
	}
	if out, err := runCLI("auto-port", "off"); err != nil {
		t.Errorf("cli auto-port off: %v %q", err, out)
	}
	if out, err := runCLI("rule-urls", "list"); err != nil || !strings.Contains(out, "gfw") {
		t.Errorf("cli rule-urls list: %v %q", err, out)
	}
	// groups 子命令：内置 PROXY 组（默认出口）的选择
	if out, err := runCLI("groups", "list"); err != nil || !strings.Contains(out, "PROXY") {
		t.Errorf("cli groups list 应含内置 PROXY 组: %v %q", err, out)
	}
	if _, err := runCLI("groups", "select", "PROXY", "node-b"); err != nil {
		t.Errorf("cli groups select PROXY node-b: %v", err)
	}
	if got := getVia(cfg.MixedPort, "http://example.invalid/"); got != "via-B" {
		t.Errorf("cli 选定 node-b 为默认出口后主端口 got %q, want via-B", got)
	}
	if _, err := runCLI("groups", "select", "PROXY", "AUTO"); err != nil {
		t.Errorf("cli groups select PROXY AUTO: %v", err)
	}
	if got := getVia(cfg.MixedPort, "http://example.invalid/"); got != "via-A" && got != "via-B" {
		t.Errorf("cli 选 AUTO 后主端口 got %q", got)
	}
	cliMain := freePort(t)
	for cliMain >= newLo && cliMain <= newLo+1 || cliMain == cfg.MixedPort {
		cliMain = freePort(t)
	}
	if out, err := runCLI("main-port", fmt.Sprintf("%d", cliMain)); err != nil {
		t.Errorf("cli main-port set: %v %q", err, out)
	}
	if got := getVia(cliMain, "http://example.invalid/"); got != "via-A" && got != "via-B" {
		t.Errorf("cli 改主端口后新端口 %d got %q", cliMain, got)
	}
	if out, err := runCLI("main-port"); err != nil || !strings.Contains(out, fmt.Sprintf("%d", cliMain)) {
		t.Errorf("cli main-port view: %v %q", err, out)
	}
	// main-node 子命令已移除：应报用法错误
	if out, err := runCLI("main-node"); err == nil {
		t.Errorf("cli main-node 应已移除: %q", out)
	}

	// 系统代理会修改宿主机全局状态，因此由平台测试文件决定是否执行并负责恢复。
	// 非 macOS 构建调用同签名空实现，避免运行时判断仍解析 Darwin 专属符号的问题。
	testSystemProxyLifecycle(t, base, cfgPath, cfg.MixedPort, newLo)

	// 清理 mihomo 全局状态，避免污染其他测试
	a.Shutdown()
	if _, err := os.Stat(filepath.Join(stateDir, "mapping.json")); err != nil {
		t.Errorf("mapping.json not persisted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "nodes.json")); err != nil {
		t.Errorf("nodes.json 节点快照未持久化: %v", err)
	}
}

// TestSnapshotRestore 验证节点快照持久化和“启动不自动同步”边界：首轮手动刷新写入
// nodes.json 后重启，即使订阅源仍在线且订阅缓存被清除，也只从快照恢复节点与端口。
//
// 参数：
//   - t: *testing.T，Go 端到端测试上下文，用于启动本地订阅、SOCKS5 与回显服务。
//
// 返回值：无。
//
// 错误情况：快照/端口未恢复、代理流量不可用、Run 启动期间再次请求订阅，或关闭
// 调度器超时时测试失败。
func TestSnapshotRestore(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "via-snap")
	}))
	defer echo.Close()
	socks := fakeSocks5(t, echo.Listener.Addr().String())
	_, socksPort, _ := net.SplitHostPort(socks)

	var subscriptionRequests atomic.Int32
	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		subscriptionRequests.Add(1)
		fmt.Fprintf(w, `proxies:
  - name: node-snap
    type: socks5
    server: 127.0.0.1
    port: %s
    udp: true
`, socksPort)
	}))
	defer sub.Close()

	stateDir := t.TempDir()
	lo := freePort(t)
	mixedPort := freePort(t)
	for mixedPort >= lo && mixedPort <= lo+2 {
		mixedPort = freePort(t)
	}
	newCfg := func() *config.Config {
		return &config.Config{
			Subscriptions:   []config.Subscription{{Name: "test", URL: sub.URL, Type: "clash"}},
			Listen:          "127.0.0.1",
			PortRange:       [2]int{lo, lo + 2},
			MixedPort:       mixedPort,
			RefreshInterval: config.Duration(time.Hour),
			HealthInterval:  config.Duration(time.Hour),
			HealthURL:       echo.URL + "/generate_204",
			HealthTimeout:   config.Duration(5 * time.Second),
			Mode:            "rule",
			Rules:           []string{"MATCH,PROXY"},
			LogLevel:        "warning",
			StateDir:        stateDir,
		}
	}

	// 第一轮：正常刷新，写快照
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a1, err := app.New(newCfg(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a1.Refresh(ctx, true); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if subscriptionRequests.Load() != 1 {
		t.Fatalf("首轮手动同步请求次数=%d，期望 1", subscriptionRequests.Load())
	}
	portOf1 := map[string]int{}
	for _, as := range a1.Assignments() {
		portOf1[as.Node.Name] = as.Port
	}
	if portOf1["node-snap"] == 0 {
		t.Fatalf("node-snap 未分配端口: %+v", a1.Assignments())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "nodes.json")); err != nil {
		t.Fatalf("nodes.json 未写入: %v", err)
	}
	a1.Shutdown()

	// 清掉订阅正文缓存但保持订阅源在线：第二轮如果偷偷自动同步，请求计数会暴露。
	if err := os.RemoveAll(filepath.Join(stateDir, "cache")); err != nil {
		t.Fatal(err)
	}

	// 第二轮：Run 启动只恢复快照，不访问仍然在线的订阅源。
	a2, err := app.New(newCfg(), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done := make(chan error, 1)
	go func() { done <- a2.Run(ctx2) }()

	deadline := time.Now().Add(15 * time.Second)
	var assigns []pool.Assignment
	for time.Now().Before(deadline) {
		if assigns = a2.Assignments(); len(assigns) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(assigns) == 0 {
		t.Fatal("快照未恢复任何端口映射")
	}
	if assigns[0].Node.Name != "node-snap" {
		t.Fatalf("快照恢复节点 = %q, want node-snap", assigns[0].Node.Name)
	}
	if assigns[0].Port != portOf1["node-snap"] {
		t.Errorf("端口映射不稳定: %d -> %d", portOf1["node-snap"], assigns[0].Port)
	}

	// 快照节点立即可用（无需等待或执行订阅同步）。
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", assigns[0].Port))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 10 * time.Second}
	resp, err := client.Get("http://example.invalid/")
	if err != nil {
		t.Fatalf("快照恢复后流量不通: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "via-snap" {
		t.Errorf("got %q, want via-snap", body)
	}

	// 等待一段时间确认启动路径没有排队异步同步，且快照节点继续保留。
	time.Sleep(2 * time.Second)
	if len(a2.Nodes()) == 0 || len(a2.Assignments()) == 0 {
		t.Error("启动后快照节点/映射被清空")
	}
	if subscriptionRequests.Load() != 1 {
		t.Errorf("Run 不应自动同步订阅，累计请求次数=%d，期望仍为 1", subscriptionRequests.Load())
	}

	cancel2()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Run 未在 ctx 取消后退出")
	}
	a2.Shutdown()
}
