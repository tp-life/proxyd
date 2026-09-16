package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"proxyd/internal/app"
	"proxyd/internal/config"
	"proxyd/internal/proxy/groupstate"
	"proxyd/internal/proxy/node"
	proxytailscale "proxyd/internal/proxy/tailscale"
)

// vpnGroupTestServer 构造带一个 select 分组和一个 fallback 分组的测试服务，
// 并关闭后台刷新（应用层刷新会触发真实健康检测，与本测试关注点无关）。
func vpnGroupTestServer(t *testing.T) *Server {
	t.Helper()
	a, err := app.New(&config.Config{
		ManualNodes: []any{"socks5://127.0.0.1:1080#self"},
		Listen:      "127.0.0.1",
		PortRange:   [2]int{42000, 42010},
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
		Groups: []config.NodeGroup{
			{Name: "vpn", Port: 43000, Type: config.GroupTypeSelect, Subscription: "manual"},
			{Name: "plain", Port: 43001, Nodes: []string{"self"}},
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)
	srv := New("127.0.0.1:0", a)
	srv.runRefresh = func(context.Context, bool) error { return nil }
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	return srv
}

// TestTerminateTailscaleSetupAPI 验证 DELETE 接口会调用一体化终止事务并返回清理
// 摘要；重复删除同一名称时返回 404，而不是静默报告成功。
//
// 参数说明：t 是 Go 测试上下文，用于隔离状态目录和报告 HTTP/配置断言失败。
//
// 返回值说明：无；成功与不存在分支分别通过状态码和应用配置快照断言。
//
// 错误情况：路由参数未传入应用层、终止后节点或组残留、响应无法解码，或重复删除
// 未返回 404 时测试失败。
func TestTerminateTailscaleSetupAPI(t *testing.T) {
	application, err := app.New(&config.Config{
		Listen:    "127.0.0.1",
		PortRange: [2]int{42000, 42010},
		MixedPort: 41999,
		Mode:      "rule",
		LogLevel:  "silent",
		StateDir:  t.TempDir(),
		Rules:     []string{"MATCH,PROXY"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(application.Shutdown)
	if _, err := application.SetupTailscale(context.Background(), proxytailscale.Setup{
		Name: "campone", ControlURL: "http://127.0.0.1:1", AuthMode: proxytailscale.AuthModeApproval, AccessMode: proxytailscale.AccessModeProxy,
	}); err != nil {
		t.Fatalf("准备 Tailscale 接入失败: %v", err)
	}

	server := New("127.0.0.1:0", application)
	t.Cleanup(func() { server.Shutdown(context.Background()) })
	remove := func() *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodDelete, "/api/tailscale/setups/campone", nil)
		request.SetPathValue("name", "campone")
		server.handleTerminateTailscaleSetup(recorder, request)
		return recorder
	}

	recorder := remove()
	if recorder.Code != http.StatusOK {
		t.Fatalf("终止接口 status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result app.TailscaleTerminationResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || result.Name != "campone" {
		t.Fatalf("终止响应异常: result=%+v err=%v", result, err)
	}
	if cfg := application.Config(); len(cfg.ManualNodes) != 0 || len(cfg.Groups) != 0 {
		t.Fatalf("API 终止后配置仍有残留: manual=%d groups=%d", len(cfg.ManualNodes), len(cfg.Groups))
	}
	if duplicate := remove(); duplicate.Code != http.StatusNotFound {
		t.Fatalf("重复终止 status=%d, want 404", duplicate.Code)
	}
}

// TestPreviewOpenVPNProfileAPI 验证 .ovpn 导入端点只返回远端与认证需求摘要，不会
// 回显 CA、客户端私钥或内联 auth-user-pass 凭据。
//
// 参数说明：t 是 Go 测试上下文，用于构造隔离应用与 HTTP recorder。
//
// 返回值说明：无；通过状态码、摘要字段和响应原文的敏感词检查表达成功。
//
// 错误情况：profile 无法解析、摘要字段缺失，或任何证书/用户名/密码内容进入响应
// 时测试失败；这保证导入预览符合管理接口的凭据最小暴露原则。
func TestPreviewOpenVPNProfileAPI(t *testing.T) {
	srv := vpnGroupTestServer(t)
	profile := `remote vpn.example.com 443 tcp
auth-user-pass
<ca>
-----BEGIN CERTIFICATE-----
SENSITIVE-CA
-----END CERTIFICATE-----
</ca>`
	body, err := json.Marshal(map[string]string{"profile": profile})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/openvpn/import", strings.NewReader(string(body)))
	srv.handlePreviewOpenVPNProfile(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("导入预览 status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var preview struct {
		Server               string `json:"server"`
		Port                 int    `json:"port"`
		Proto                string `json:"proto"`
		RequiresUserPassword bool   `json:"requires_user_password"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Server != "vpn.example.com" || preview.Port != 443 || preview.Proto != "tcp" || !preview.RequiresUserPassword {
		t.Fatalf("导入摘要异常: %+v", preview)
	}
	if strings.Contains(recorder.Body.String(), "SENSITIVE-CA") || strings.Contains(recorder.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatalf("导入预览泄露证书材料: %s", recorder.Body.String())
	}
}

// TestGroupSelectAPI 验证 POST /api/groups/{name}/select 的请求校验与领域约束
// 透传：请求体非法/缺 node 为 400；分组不存在、非 select 类型、节点不在可用
// 成员中同样为 400。成功路径（状态落盘 + 热更新回滚）由 app 层测试覆盖。
func TestGroupSelectAPI(t *testing.T) {
	srv := vpnGroupTestServer(t)

	post := func(group, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/groups/"+group+"/select", strings.NewReader(body))
		req.SetPathValue("name", group)
		srv.handleSelectGroupNode(rec, req)
		return rec
	}

	if rec := post("vpn", "{invalid"); rec.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON status=%d, want 400", rec.Code)
	}
	if rec := post("vpn", `{"node":"  "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("空 node status=%d, want 400", rec.Code)
	}
	if rec := post("nope", `{"node":"self"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("不存在的分组 status=%d, want 400", rec.Code)
	}
	if rec := post("plain", `{"node":"self"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("非 select 分组 status=%d, want 400", rec.Code)
	}
	// 节点存在但当前不可用（节点池为空）时同样拒绝
	if rec := post("vpn", `{"node":"self"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("节点不在可用成员中 status=%d, want 400", rec.Code)
	}
}

// TestListGroupsExposesTypeAndSelected 验证分组列表响应始终暴露归一化后的
// type（空值补 url-test），并对 select 分组附带持久化的当前选中项；
// overview 的 groups 字段与列表接口共用同一结构。
func TestListGroupsExposesTypeAndSelected(t *testing.T) {
	srv := vpnGroupTestServer(t)
	stateDir := srv.app.Config().StateDir
	if err := groupstate.Save(filepath.Join(stateDir, groupstate.FileName), map[string]string{"vpn": "self"}); err != nil {
		t.Fatalf("写入选中状态失败: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.handleListGroups(rec, httptest.NewRequest(http.MethodGet, "/api/groups", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var entries []GroupEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("分组数量 = %d, want 2", len(entries))
	}
	if entries[0].Type != config.GroupTypeSelect || entries[0].Selected != "self" {
		t.Errorf("select 分组条目异常: %+v", entries[0])
	}
	if entries[1].Type != config.GroupTypeURLTest || entries[1].Selected != "" {
		t.Errorf("普通分组条目异常: %+v", entries[1])
	}

	ov := httptest.NewRecorder()
	srv.handleOverview(ov, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if ov.Code != http.StatusOK {
		t.Fatalf("overview status=%d body=%s", ov.Code, ov.Body.String())
	}
	var payload struct {
		Groups []GroupEntry `json:"groups"`
	}
	if err := json.Unmarshal(ov.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Groups) != 2 || payload.Groups[0].Selected != "self" || payload.Groups[0].Type != config.GroupTypeSelect {
		t.Fatalf("overview 分组条目异常: %+v", payload.Groups)
	}
}

// TestAddManualNodeProxyAPI 验证 POST /api/manual-nodes 的 proxy 结构化入口：
// 隧道类映射校验通过后持久化，mihomo 原生 Tailscale 参数保持透传，列表响应中
// 凭据字段（auth-key 等）已打码；url 与 proxy 混用、非隧道类型返回 400；Tailscale
// 缺少 auth-key 时进入 mihomo/tsnet 管理员审批模式，因此结构化入口必须允许保存。
func TestAddManualNodeProxyAPI(t *testing.T) {
	srv := vpnGroupTestServer(t)

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/manual-nodes", strings.NewReader(body))
		srv.handleAddManualNode(rec, req)
		return rec
	}

	if rec := post(`{"url":"socks5://h:1080","proxy":{"name":"x","type":"tailscale","auth-key":"k"}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("url 与 proxy 混用 status=%d, want 400", rec.Code)
	}
	if rec := post(`{"proxy":{"name":"x","type":"ss","server":"h","port":8388}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("非隧道类型 status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec := post(`{"proxy":{"name":"approval","type":"tailscale","control-url":"https://hs.example.com"}}`); rec.Code != http.StatusCreated {
		t.Errorf("管理员审批节点 status=%d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	rec := post(`{"proxy":{"name":"ts-exit","type":"tailscale","auth-key":"tskey-auth-secret","exit-node":"auto:any","accept-routes":true,"udp":true,"ip-version":"ipv4-prefer"},"name":""}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("添加结构化节点 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var entry app.ManualNodeEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Name != "ts-exit" || entry.Type != "tailscale" {
		t.Fatalf("添加结果异常: %+v", entry)
	}

	if rec := post(`{"proxy":{"name":"ts-exit","type":"tailscale","auth-key":"tskey-auth-other"}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("重复节点名 status=%d, want 400", rec.Code)
	}

	list := httptest.NewRecorder()
	srv.handleListManualNodes(list, httptest.NewRequest(http.MethodGet, "/api/manual-nodes", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("列表 status=%d body=%s", list.Code, list.Body.String())
	}
	if strings.Contains(list.Body.String(), "tskey-auth-secret") {
		t.Fatalf("列表响应泄露 auth-key: %s", list.Body.String())
	}
	var entries []app.ManualNodeEntry
	if err := json.Unmarshal(list.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	// 测试配置自带一个字符串节点（下标 0）和审批节点（下标 1），带密钥节点在下标 2。
	if len(entries) != 3 || entries[2].Type != "tailscale" || entries[2].Proxy["auth-key"] != "***" {
		t.Fatalf("结构化手动节点列表条目异常: %+v", entries)
	}
	if entries[2].Proxy["exit-node"] != "auto:any" || entries[2].Proxy["accept-routes"] != true || entries[2].Proxy["udp"] != true || entries[2].Proxy["ip-version"] != "ipv4-prefer" {
		t.Fatalf("Tailscale mihomo 原生参数未完整透传: %+v", entries[2].Proxy)
	}
}

// TestNewNodeEntryTunnel 验证节点列表记录的隧道标识：隧道类（VPN）出站
// 标记 tunnel=true 且 Port 恒为 0（不参与端口映射属正常状态），普通节点不受影响。
func TestNewNodeEntryTunnel(t *testing.T) {
	tunnel := &node.Node{
		Name:    "ts-exit",
		Alive:   true,
		Mapping: map[string]any{"name": "ts-exit", "type": "tailscale", "auth-key": "k"},
	}
	entry := newNodeEntry(tunnel, 0)
	if !entry.Tunnel || entry.Type != "tailscale" || entry.Port != 0 {
		t.Errorf("隧道节点条目异常: %+v", entry)
	}

	plain := &node.Node{
		Name:         "hk 01",
		Subscription: "sub-a",
		Alive:        true,
		Mapping:      map[string]any{"name": "hk 01", "type": "ss", "server": "1.2.3.4", "port": 8388},
	}
	entry = newNodeEntry(plain, 42001)
	if entry.Tunnel || entry.Port != 42001 {
		t.Errorf("普通节点条目异常: %+v", entry)
	}
}
