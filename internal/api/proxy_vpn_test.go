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
// 隧道类映射校验通过后持久化，列表响应中凭据字段（auth-key 等）已打码；
// url 与 proxy 混用、非隧道类型、缺必填凭据均返回 400。
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
	if rec := post(`{"proxy":{"name":"x","type":"tailscale"}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("缺 auth-key status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}

	rec := post(`{"proxy":{"name":"ts-exit","type":"tailscale","auth-key":"tskey-auth-secret"},"name":""}`)
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
	// 测试配置自带一个字符串手动节点（下标 0），结构化节点在下标 1。
	if len(entries) != 2 || entries[1].Type != "tailscale" || entries[1].Proxy["auth-key"] != "***" {
		t.Fatalf("结构化手动节点列表条目异常: %+v", entries)
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
