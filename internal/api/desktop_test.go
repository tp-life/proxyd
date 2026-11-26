package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"proxyd/internal/desktop"
)

// TestDesktopAPIConnectionCRUD 验证独立桌面路由的服务状态与连接档案 CRUD 合约。
//
// 参数说明：t 为 Go 测试上下文。
//
// 返回值说明：无；状态结构、默认端口、更新和删除响应均符合 Web 合约时通过。
//
// 错误情况：路由未注册、JSON 字段不稳定、配置事务失败或未知会话未返回 400 时失败。
func TestDesktopAPIConnectionCRUD(t *testing.T) {
	_, address := newRemoteTestServer(t)
	base := "http://" + address

	code, status := remoteAPIReq(t, http.MethodGet, base+"/api/desktop", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/desktop 状态码 = %d", code)
	}
	services, ok := status["services"].([]any)
	if !ok || len(services) != 2 {
		t.Fatalf("桌面服务响应错误: %v", status)
	}
	if status["remote_enabled"] != false || status["api_loopback"] != true {
		t.Fatalf("桌面前置状态错误: %v", status)
	}
	if status["session_idle_timeout"] != "30m0s" || status["session_max_lifetime"] != "24h0m0s" {
		t.Fatalf("桌面会话回收边界未暴露: %v", status)
	}

	connection := map[string]any{
		"name": "办公室 Windows", "remote": "office", "protocol": "rdp", "remote_port": 0, "username": `DOMAIN\user`,
	}
	code, status = remoteAPIReq(t, http.MethodPost, base+"/api/desktop/connections", connection)
	if code != http.StatusOK {
		t.Fatalf("新增桌面连接状态码 = %d，响应=%v", code, status)
	}
	connections, ok := status["connections"].([]any)
	if !ok || len(connections) != 1 || connections[0].(map[string]any)["remote_port"] != float64(3389) {
		t.Fatalf("新增桌面连接响应错误: %v", status)
	}

	connection["name"] = "办公室 VNC"
	connection["protocol"] = "vnc"
	connection["remote_port"] = 5901
	code, status = remoteAPIReq(t, http.MethodPut, base+"/api/desktop/connections/%E5%8A%9E%E5%85%AC%E5%AE%A4%20Windows", connection)
	if code != http.StatusOK {
		t.Fatalf("更新桌面连接状态码 = %d，响应=%v", code, status)
	}
	connections = status["connections"].([]any)
	if len(connections) != 1 || connections[0].(map[string]any)["name"] != "办公室 VNC" {
		t.Fatalf("更新桌面连接响应错误: %v", status)
	}

	code, _ = remoteAPIReq(t, http.MethodPost, base+"/api/desktop/sessions", map[string]string{"connection": "不存在"})
	if code != http.StatusBadRequest {
		t.Fatalf("未知连接启动会话状态码 = %d，期望 400", code)
	}
	code, status = remoteAPIReq(t, http.MethodDelete, base+"/api/desktop/connections/%E5%8A%9E%E5%85%AC%E5%AE%A4%20VNC", nil)
	if code != http.StatusOK || len(status["connections"].([]any)) != 0 {
		t.Fatalf("删除桌面连接失败: code=%d status=%v", code, status)
	}
}

// TestDesktopSessionResponseLaunchTargets 验证领域会话到浏览器启动目标的显式映射。
//
// 参数说明：t 为 Go 测试上下文。
//
// 返回值说明：无；RDP 使用同源下载端点、VNC 使用本地 URI 时通过。
//
// 错误情况：响应泄露领域内部对象、地址格式错误或协议启动类型映射颠倒时测试失败。
func TestDesktopSessionResponseLaunchTargets(t *testing.T) {
	base := desktop.Session{
		ID: "abc123", ConnectionName: "office", RemotePort: 3389,
		LocalAddress: "127.0.0.1:41000", StartedAt: time.Date(2026, 9, 4, 1, 2, 3, 0, time.UTC),
	}
	base.Protocol = desktop.ProtocolRDP
	rdp := desktopSessionToResponse(base)
	if rdp.LaunchKind != "download" || rdp.LaunchURL != "/api/desktop/sessions/abc123/rdp" {
		t.Fatalf("RDP 启动目标错误: %+v", rdp)
	}
	if !strings.HasPrefix(rdp.StartedAt, "2026-09-04T01:02:03") {
		t.Fatalf("RDP 会话时间格式错误: %q", rdp.StartedAt)
	}

	base.Protocol = desktop.ProtocolVNC
	base.RemotePort = 5900
	vnc := desktopSessionToResponse(base)
	if vnc.LaunchKind != "uri" || vnc.LaunchURL != "vnc://127.0.0.1:41000" {
		t.Fatalf("VNC 启动目标错误: %+v", vnc)
	}
}

// TestDesktopQualityAndAdhocSession 验证 RDP 画质预设持久化及 CLI 复用守护进程临时会话端点。
func TestDesktopQualityAndAdhocSession(t *testing.T) {
	_, address := newRemoteTestServer(t)
	base := "http://" + address

	token := newRemoteTestToken(t)
	code, _ := remoteAPIReq(t, http.MethodPost, base+"/api/remote/remotes", map[string]string{"name": "win-pc", "token": token})
	if code != http.StatusCreated {
		t.Fatalf("添加远端失败: %d", code)
	}
	code, status := remoteAPIReq(t, http.MethodPost, base+"/api/desktop/connections", map[string]any{
		"name": "办公机", "remote": "win-pc", "protocol": "rdp", "remote_port": 3389, "quality": "smooth",
	})
	if code != http.StatusOK {
		t.Fatalf("保存带画质预设的连接失败: code=%d status=%v", code, status)
	}
	conns := status["connections"].([]any)
	if conns[0].(map[string]any)["quality"] != "smooth" {
		t.Fatalf("画质预设未保存: %v", conns[0])
	}
	code, _ = remoteAPIReq(t, http.MethodPost, base+"/api/desktop/connections", map[string]any{
		"name": "非法画质", "remote": "win-pc", "protocol": "rdp", "remote_port": 3389, "quality": "ultra",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("非法画质预设应返回 400，got %d", code)
	}

	code, session := remoteAPIReq(t, http.MethodPost, base+"/api/desktop/sessions/adhoc", map[string]any{
		"remote": "win-pc", "protocol": "rdp", "remote_port": 3389,
	})
	if code != http.StatusOK || session["local_address"] == "" || session["udp_enabled"] != true {
		t.Fatalf("POST /api/desktop/sessions/adhoc 失败: code=%d session=%v", code, session)
	}
	id, _ := session["id"].(string)
	code, _ = remoteAPIReq(t, http.MethodDelete, base+"/api/desktop/sessions/"+id, nil)
	if code != http.StatusNoContent {
		t.Fatalf("关闭 adhoc 会话失败: %d", code)
	}
}
