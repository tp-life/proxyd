package api

// 本文件验证系统摘要不依赖代理核心或远程隧道启动，且不输出配置凭据。

import (
	"net/http"
	"testing"
)

// TestSystemStatusWithoutBusinessRuntime 验证独立系统摘要的公开契约。
// 参数：t 为 *testing.T；返回无；状态缺失、错误时长或额外暴露字段时测试失败。
func TestSystemStatusWithoutBusinessRuntime(t *testing.T) {
	_, address := newRemoteTestServer(t)
	code, payload := remoteAPIReq(t, http.MethodGet, "http://"+address+"/api/system/status", nil)
	if code != http.StatusOK || len(payload) != 2 {
		t.Fatalf("系统摘要状态或字段不符: code=%d payload=%v", code, payload)
	}
	uptime, ok := payload["uptime_seconds"].(float64)
	if !ok || uptime < 0 || uptime > 60 || payload["pending_restart"] != false {
		t.Fatalf("无业务运行态时摘要不正确: %v", payload)
	}
}

// TestSystemSettingsWithoutProxy 验证通用设置可独立读取，且不夹带代理字段或配置凭据。
// 参数：t 为 *testing.T；返回无；缺少公共状态、字段越界或路由失效时测试失败。
func TestSystemSettingsWithoutProxy(t *testing.T) {
	_, address := newRemoteTestServer(t)
	code, payload := remoteAPIReq(t, http.MethodGet, "http://"+address+"/api/system/settings", nil)
	if code != http.StatusOK || len(payload) != 3 {
		t.Fatalf("公共设置响应异常: code=%d", code)
	}
	for _, key := range []string{"autostart", "autostart_runtime", "version_check"} {
		if _, exists := payload[key]; !exists {
			t.Errorf("公共设置缺少 %s", key)
		}
	}
}
