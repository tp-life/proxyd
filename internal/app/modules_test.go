package app

// 模块开关回归覆盖真实代理监听、后台刷新门与失败回滚；系统设置使用注入的替身。
import (
	"net"
	"path/filepath"
	"proxyd/internal/config"
	"testing"
	"time"
)

// TestProxyModuleStopsAndRestoresListener 验证停用后端口释放，配置更新和刷新不能重新打开，启用可恢复。
// 参数：t 为 *testing.T；返回无；实际 TCP 可连接性与开关状态不一致时失败。
func TestProxyModuleStopsAndRestoresListener(t *testing.T) {
	a := newSystemProxyTestApp(t, filepath.Join(t.TempDir(), "config.yaml"))
	a.cfg.ManualNodes = []string{"http://127.0.0.1:9#local"}
	a.cfg.SystemProxy = true
	recorder := &recordingSystemProxy{}
	a.systemProxy = recorder
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	a.cfg.MixedPort = ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	a.cfg.ExternalController = ""
	if err := a.Regenerate(); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if c != nil {
			c.Close()
		}
		if (err == nil) != want {
			t.Fatalf("端口可用性=%t，期望=%t: %v", err == nil, want, err)
		}
	}
	check(true)
	if err := a.SetModuleEnabled("proxy", false); err != nil {
		t.Fatal(err)
	}
	check(false)
	if !a.Config().SystemProxy || !a.Config().ProxyDisabled {
		t.Fatal("停用覆盖了原有系统代理偏好")
	}
	if err := a.Regenerate(); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	check(false)
	saved, err := config.Load(a.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.ProxyDisabled {
		t.Fatal("停用未持久化")
	}
	if err := a.SetModuleEnabled("proxy", true); err != nil {
		t.Fatal(err)
	}
	check(true)
}

// TestModulePersistenceRollback 验证落盘失败恢复原来的代理与远程模块开关。
// 参数：t 为 *testing.T；返回无；任一事务假成功或未回滚时失败。
func TestModulePersistenceRollback(t *testing.T) {
	a := newSystemProxyTestApp(t, t.TempDir())
	a.systemProxy = &recordingSystemProxy{}
	for _, id := range []string{"proxy", "remote"} {
		if err := a.SetModuleEnabled(id, false); err == nil {
			t.Fatalf("%s 应落盘失败", id)
		}
		for _, state := range a.Modules() {
			if !state.Enabled {
				t.Fatalf("%s 未回滚", state.ID)
			}
		}
	}
	if err := a.SetModuleEnabled("unknown", false); err == nil {
		t.Fatal("未知模块未拒绝")
	}
}

// TestRemoteModuleRetryRecoversPortConflict 验证端口冲突显示降级和计划重试，释放端口后可恢复。
// 参数 t 为测试对象；返回无；状态伪成功、重试残留或监听未恢复时失败，不需要外网连接。
func TestRemoteModuleRetryRecoversPortConflict(t *testing.T) {
	a := newSystemProxyTestApp(t, filepath.Join(t.TempDir(), "config.yaml"))
	a.cfg.ProxyDisabled = true
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	a.cfg.Remote.Forwards = []config.RemoteForward{{Name: "conflict", Listen: listener.Addr().String(), Remote: newRemoteTestToken(t), RemotePort: 22}}
	a.startRemote()
	state := a.Modules()[1]
	if state.Phase != "degraded" || state.NextRetryAt == nil {
		t.Fatalf("端口冲突未安排重试: %+v", state)
	}
	listener.Close()
	if err = a.RetryModule(t.Context(), "remote"); err != nil {
		t.Fatal(err)
	}
	state = a.Modules()[1]
	if state.Phase != "running" || !state.Running || state.NextRetryAt != nil || state.Error != "" {
		t.Fatalf("恢复后状态错误: %+v", state)
	}
}
