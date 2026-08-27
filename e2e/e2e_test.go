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
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"proxyd/internal/api"
	"proxyd/internal/app"
	"proxyd/internal/config"
	"proxyd/internal/node"
	"proxyd/internal/pool"
	"proxyd/internal/sysproxy"
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
	apiSrv := api.New("127.0.0.1:0", a)
	if err := apiSrv.Start(); err != nil {
		t.Fatal(err)
	}
	defer apiSrv.Shutdown(context.Background())
	base := "http://" + apiSrv.Addr()

	// GET / 返回内嵌控制台页面
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "proxyd") || !strings.Contains(string(body), "/api/overview") {
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

	// ---- 按订阅查看节点：第二订阅内容与第一订阅完全相同（节点全部重叠），
	// 去重后节点归属名字序最小的订阅（"127.0.0.1" < "test"）----
	secondName := saved.Subscriptions[1].Name
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
	for newLo >= lo && newLo <= lo+4 || newLo == cfg.MixedPort || newLo+1 == cfg.MixedPort {
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

	// ---- 系统代理（macOS 真实执行 networksetup，测试后恢复原状）----
	if runtime.GOOS == "darwin" {
		snap, err := sysproxy.Snapshot()
		if err != nil {
			t.Logf("sysproxy snapshot 失败，跳过: %v", err)
		} else {
			defer func() { _ = sysproxy.Restore(snap) }()
			resp, err = http.Post(base+"/api/system-proxy", "application/json", strings.NewReader(`{"enabled":true}`))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("system-proxy on: status=%d", resp.StatusCode)
			}
			if on, _ := sysproxy.Status("127.0.0.1", cfg.MixedPort); !on {
				t.Error("系统代理未生效")
			}
			saved, _ = config.Load(cfgPath)
			if !saved.SystemProxy {
				t.Error("system-proxy 未持久化")
			}
			resp, _ = http.Post(base+"/api/system-proxy", "application/json", strings.NewReader(`{"enabled":false}`))
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("system-proxy off: status=%d", resp.StatusCode)
			}
			if on, _ := sysproxy.Status("127.0.0.1", cfg.MixedPort); on {
				t.Error("系统代理未关闭")
			}
		}
	}

	// 清理 mihomo 全局状态，避免污染其他测试
	a.Shutdown()
	if _, err := os.Stat(filepath.Join(stateDir, "mapping.json")); err != nil {
		t.Errorf("mapping.json not persisted: %v", err)
	}
}
