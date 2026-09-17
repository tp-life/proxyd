package remote

// 模块暂停测试只使用回环资源，验证子功能配置保留与活动会话释放。
import (
	"errors"
	"net"
	"proxyd/internal/config"
	"testing"
	"time"
)

// TestRemoteModuleClosesTerminalAndForwards 验证模块关闭的真实资源效果与再次开启。
// 参数：t 为 *testing.T；返回无；监听泄漏、会话未关闭或子开关丢失时失败。
func TestRemoteModuleClosesTerminalAndForwards(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	defer m.Close()
	cfg := config.RemoteConfig{WebTerminal: true}
	if err := m.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := localShellSSHHandler(t.TempDir(), ""); err != nil {
		t.Skip("当前平台不支持 shell")
	}
	session, err := m.OpenWebTerminal(t.Context(), TerminalSize{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	forward, err := m.StartTransientForward(newTestToken(t), 22)
	if err != nil {
		t.Fatal(err)
	}
	addr := forward.Address()
	defer forward.Close()
	cfg.Disabled = true
	if err := m.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.done:
	case <-time.After(3 * time.Second):
		t.Fatal("停用未结束终端")
	}
	select {
	case <-forward.runner.done:
	case <-time.After(3 * time.Second):
		t.Fatal("停用未结束临时转发")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Fatal("临时端口仍在监听")
	}
	if _, err := m.OpenWebTerminal(t.Context(), TerminalSize{}); !errors.Is(err, ErrWebTerminalDisabled) {
		t.Fatalf("停用仍可创建终端: %v", err)
	}
	if !m.cfg.WebTerminal || m.Status().WebTerminal || m.Status().ModuleEnabled {
		t.Fatal("原配置和有效状态混淆")
	}
	cfg.Disabled = false
	if err := m.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	restored, err := m.OpenWebTerminal(t.Context(), TerminalSize{})
	if err != nil {
		t.Fatal(err)
	}
	restored.Close()
}

// TestRemoteModuleCancelsInbound 验证已通过授权的入站连接也会在模块禁用时结束。
// 参数：t 为 *testing.T；返回无；处理器无法解阻塞或禁用后授权仍通过时失败。
func TestRemoteModuleCancelsInbound(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	defer m.Close()
	server, client := net.Pipe()
	defer client.Close()
	entered, done := make(chan struct{}), make(chan struct{})
	go func() {
		m.guardConnection(22, func(c net.Conn) { close(entered); buffer := make([]byte, 1); _, _ = c.Read(buffer) })(server)
		close(done)
	}()
	<-entered
	if err := m.Apply(config.RemoteConfig{Disabled: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("入站连接未被关闭")
	}
	if decideConnection(config.RemoteConfig{Disabled: true}, client.RemoteAddr(), 22, time.Now()).Allowed {
		t.Fatal("停用后仍授权入站")
	}
}

// TestWebTerminalSwitchClosesSession 验证单独关闭终端开关不会留下全局最小化会话。
// 参数：t 为 *testing.T；返回无；关闭失败或影响模块开关时失败。
func TestWebTerminalSwitchClosesSession(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	defer m.Close()
	if _, err := localShellSSHHandler(t.TempDir(), ""); err != nil {
		t.Skip("平台不支持 shell")
	}
	if err := m.Apply(config.RemoteConfig{WebTerminal: true}); err != nil {
		t.Fatal(err)
	}
	session, err := m.OpenWebTerminal(t.Context(), TerminalSize{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := m.Apply(config.RemoteConfig{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("独立终端开关未关闭已有会话")
	}
	if !m.Status().ModuleEnabled {
		t.Fatal("独立开关修改了模块状态")
	}
}
