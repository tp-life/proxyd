package app

// 本文件验证系统代理与主端口的跨 OS、mihomo、内存和磁盘事务回滚。

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"proxyd/internal/config"
)

// recordingSystemProxy 是可注入失败的系统代理端口，用于验证调用顺序和回滚。
type recordingSystemProxy struct {
	calls      []string
	failOnPort int
}

// On 记录开启或重绑请求，并可对指定端口返回模拟失败。
//
// 参数说明：
//   - host: string，应用层传入的回环主机。
//   - port: int，应用层传入的主端口。
//
// 返回值说明：error，port 命中 failOnPort 时返回模拟错误。
//
// 错误情况：仅由 failOnPort 显式触发，便于稳定复现 OS 重绑失败。
func (r *recordingSystemProxy) On(host string, port int) error {
	r.calls = append(r.calls, host+":"+strconv.Itoa(port))
	if port == r.failOnPort {
		return errors.New("模拟系统代理设置失败")
	}
	return nil
}

// Off 记录关闭请求。
//
// 参数说明：无。
//
// 返回值说明：error，当前模拟始终返回 nil。
//
// 错误情况：无；需要的失败分支由 On 提供。
func (r *recordingSystemProxy) Off() error {
	r.calls = append(r.calls, "off")
	return nil
}

// newSystemProxyTestApp 创建一个可执行 mihomo 热更新但不需要外部节点的测试 App。
//
// 参数说明：
//   - t: *testing.T，提供隔离状态目录与失败报告。
//   - cfgPath: string，持久化目标；可传目录路径稳定制造 rename 失败。
//
// 返回值说明：*App，已注册测试清理的应用实例。
//
// 错误情况：App 构造失败时立即终止当前测试。
func newSystemProxyTestApp(t *testing.T, cfgPath string) *App {
	t.Helper()
	cfg := &config.Config{
		Listen:             "127.0.0.1",
		MixedPort:          41999,
		PortRange:          [2]int{42000, 42100},
		Mode:               "rule",
		LogLevel:           "silent",
		StateDir:           t.TempDir(),
		ExternalController: "127.0.0.1:19090",
		APIListen:          "127.0.0.1:19091",
		HealthURL:          "https://www.gstatic.com/generate_204",
		HealthTimeout:      config.Duration(3 * time.Second),
		Rules:              []string{"MATCH,PROXY"},
	}
	a, err := New(cfg, cfgPath)
	if err != nil {
		t.Fatalf("创建测试 App 失败: %v", err)
	}
	t.Cleanup(a.Shutdown)
	return a
}

// TestSetSystemProxyRollsBackWhenPersistenceFails 验证系统代理已开启但配置无法落盘时，
// 应恢复原来的关闭状态。
//
// 参数说明：
//   - t: *testing.T，负责构造失败路径与断言。
//
// 返回值说明：无。
//
// 错误情况：设置未返回错误、内存开关未回滚或 OS 适配器未收到 off 时测试失败。
func TestSetSystemProxyRollsBackWhenPersistenceFails(t *testing.T) {
	a := newSystemProxyTestApp(t, t.TempDir())
	recorder := &recordingSystemProxy{}
	a.systemProxy = recorder

	if err := a.SetSystemProxy(true); err == nil {
		t.Fatal("配置路径是目录时应返回持久化错误")
	}
	if a.Config().SystemProxy {
		t.Fatal("持久化失败后内存系统代理开关未回滚")
	}
	if len(recorder.calls) != 2 || recorder.calls[0] != "127.0.0.1:41999" || recorder.calls[1] != "off" {
		t.Fatalf("系统代理调用顺序 = %v，期望先开启后回滚关闭", recorder.calls)
	}
}

// TestSetMainPortRollsBackWhenSystemProxyRebindFails 验证系统代理重绑新端口失败时，
// 新的 mihomo 入口和内存配置不会被提交。
//
// 参数说明：
//   - t: *testing.T，负责注入 OS 失败与断言回滚结果。
//
// 返回值说明：无。
//
// 错误情况：方法未返回错误、主端口没有恢复，或未尝试把 OS 代理指回旧端口时测试失败。
func TestSetMainPortRollsBackWhenSystemProxyRebindFails(t *testing.T) {
	a := newSystemProxyTestApp(t, "")
	a.cfg.SystemProxy = true
	recorder := &recordingSystemProxy{failOnPort: 41998}
	a.systemProxy = recorder

	if err := a.SetMainPort(41998); err == nil {
		t.Fatal("系统代理重绑失败时 SetMainPort 应返回错误")
	}
	if got := a.Config().MixedPort; got != 41999 {
		t.Fatalf("回滚后主端口 = %d，期望 41999", got)
	}
	if len(recorder.calls) != 2 || recorder.calls[0] != "127.0.0.1:41998" || recorder.calls[1] != "127.0.0.1:41999" {
		t.Fatalf("系统代理重绑/回滚顺序 = %v", recorder.calls)
	}
}
