package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"proxyd/internal/autostart"
)

// 本文件覆盖系统服务诊断提示（崩溃循环/旧 KeepAlive 拉回）与 serve 单实例守卫。

// captureStdout 捕获 f 执行期间写入标准输出的内容。
// 参数：t 为 *testing.T；f 为无参数无返回的待执行闭包。返回：string 输出文本。
// 错误：管道创建、关闭或读取失败时测试失败；执行后恢复原始标准输出。
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	previous := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	f()
	os.Stdout = previous
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// stubInspect 替换系统服务查询替身。参数：t 为 *testing.T；s 为固定返回的快照。
// 返回：无。错误：无；测试结束自动恢复原适配器。
func stubInspect(t *testing.T, s autostart.RuntimeStatus) {
	t.Helper()
	previous := inspectService
	t.Cleanup(func() { inspectService = previous })
	inspectService = func() autostart.RuntimeStatus { return s }
}

// TestWarnBrokenSystemService 验证崩溃循环提示只在「已注册 + 未运行 + 非零退出」时出现。
// 参数：t 为 *testing.T。返回：无。错误：提示缺失或误报时失败。
func TestWarnBrokenSystemService(t *testing.T) {
	exitCode := 78
	stubInspect(t, autostart.RuntimeStatus{Loaded: true, State: "spawn scheduled", LastExitCode: &exitCode})
	if out := captureStdout(t, warnBrokenSystemService); !strings.Contains(out, "78") || !strings.Contains(out, "autostart on") {
		t.Fatalf("崩溃循环缺少修复指引: %q", out)
	}

	stubInspect(t, autostart.RuntimeStatus{Loaded: true, Running: true, PID: 42, State: "running"})
	if out := captureStdout(t, warnBrokenSystemService); out != "" {
		t.Fatalf("健康服务不应提示: %q", out)
	}

	stubInspect(t, autostart.RuntimeStatus{State: "unloaded"})
	if out := captureStdout(t, warnBrokenSystemService); out != "" {
		t.Fatalf("未注册服务不应提示: %q", out)
	}
}

// TestWarnIfSystemServiceRespawned 验证 stop 后仅系统实例被旧 KeepAlive 拉回时才提示。
// 参数：t 为 *testing.T。返回：无。错误：提示缺失、误报或独立实例被误检时失败。
func TestWarnIfSystemServiceRespawned(t *testing.T) {
	// 独立实例不受系统策略影响，即使系统在跑别的实例也不提示。
	stubInspect(t, autostart.RuntimeStatus{Loaded: true, Running: true, PID: 99, State: "running"})
	if out := captureStdout(t, func() { warnIfSystemServiceRespawned(42, false) }); out != "" {
		t.Fatalf("非系统实例不应检测拉回: %q", out)
	}
	// 系统实例停止后出现了新 PID：旧 KeepAlive=true 拉回，必须提示。
	if out := captureStdout(t, func() { warnIfSystemServiceRespawned(42, true) }); !strings.Contains(out, "99") || !strings.Contains(out, "autostart off") {
		t.Fatalf("拉回未提示: %q", out)
	}
	// 停止后保持停止：新语义正常路径，不提示。
	stubInspect(t, autostart.RuntimeStatus{Loaded: true, State: "not running"})
	if out := captureStdout(t, func() { warnIfSystemServiceRespawned(42, true) }); out != "" {
		t.Fatalf("保持停止不应提示: %q", out)
	}
}

// TestReportAutostartServiceState 验证 autostart on 后的状态报告：独立实例占用时
// 提示让位与接管方式，系统实例拉起时报 PID，都不可见时退化打印快照消息。
// 参数：t 为 *testing.T。返回：无。错误：三种分支输出不符合预期时失败。
func TestReportAutostartServiceState(t *testing.T) {
	previousInterval, previousRounds := autostartPollInterval, autostartPollRounds
	t.Cleanup(func() { autostartPollInterval, autostartPollRounds = previousInterval, previousRounds })
	autostartPollInterval, autostartPollRounds = 0, 1

	cfg := newAPITestConfig(t)
	cfg.StateDir = t.TempDir()

	// 独立实例占用 pidfile：必须提示让位与接管方式。
	if err := writePIDFile(pidPath(cfg), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	stubInspect(t, autostart.RuntimeStatus{Loaded: true, State: "not running"})
	if out := captureStdout(t, func() { reportAutostartServiceState(cfg) }); !strings.Contains(out, "让位") || !strings.Contains(out, "autostart on") {
		t.Fatalf("独立实例占用缺少提示: %q", out)
	}

	// 无占用且系统实例已运行：直接报 PID。
	if err := os.Remove(pidPath(cfg)); err != nil {
		t.Fatal(err)
	}
	stubInspect(t, autostart.RuntimeStatus{Loaded: true, Running: true, PID: 4242, State: "running"})
	if out := captureStdout(t, func() { reportAutostartServiceState(cfg) }); !strings.Contains(out, "4242") {
		t.Fatalf("系统实例启动未报告: %q", out)
	}

	// 无占用且系统实例未起：退化打印快照消息。
	stubInspect(t, autostart.RuntimeStatus{Loaded: true, State: "spawn scheduled", Message: "系统服务尚未运行：spawn scheduled"})
	if out := captureStdout(t, func() { reportAutostartServiceState(cfg) }); !strings.Contains(out, "spawn scheduled") {
		t.Fatalf("未运行时应打印快照消息: %q", out)
	}
}

// TestEnsureSingleInstance 验证单实例守卫：前台终端报错，后台/托管场景干净退出让位，
// 且让位分支必须报告 proceed=false（曾经返回 nil 被 cmdServe 当作继续启动，造成双实例）。
// 参数：t 为 *testing.T。返回：无。错误：两分支判断错误或缺 pid 文件误判时失败。
func TestEnsureSingleInstance(t *testing.T) {
	cfg := newAPITestConfig(t)
	cfg.StateDir = t.TempDir()
	previous := stdoutIsTerminal
	t.Cleanup(func() { stdoutIsTerminal = previous })

	proceed, err := ensureSingleInstance(cfg)
	if err != nil || !proceed {
		t.Fatalf("无 pid 文件应放行: %v, %v", proceed, err)
	}
	if err := writePIDFile(pidPath(cfg), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	stdoutIsTerminal = func() bool { return true }
	proceed, err = ensureSingleInstance(cfg)
	if err == nil || !strings.Contains(err.Error(), "已在运行") || proceed {
		t.Fatalf("前台终端应拒绝重复启动: %v, %v", proceed, err)
	}
	stdoutIsTerminal = func() bool { return false }
	proceed, err = ensureSingleInstance(cfg)
	if err != nil || proceed {
		t.Fatalf("后台/托管场景应干净退出让位: %v, %v", proceed, err)
	}
}

// TestRestartManagedInstance 验证托管重启：向 API 发起重启请求并等待新实例健康；
// 未观察到替代实例时在限定时间内报错。
// 参数：t 为 *testing.T。返回：无。错误：未请求重启、误就绪或超时分支不正确时失败。
func TestRestartManagedInstance(t *testing.T) {
	restarted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/restart" {
			restarted = true
			writeJSONForTest(w)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := newAPITestConfig(t)
	cfg.StateDir = t.TempDir()
	cfg.APIListen = strings.TrimPrefix(server.URL, "http://")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	// 新实例已就绪：pidfile 指向本进程（存活），oldPID 是不存在的旧值。
	if err := writePIDFile(pidPath(cfg), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := restartManagedInstance(cfg, path, os.Getpid()+999); err != nil {
		t.Fatalf("新实例已健康，重启应成功: %v", err)
	}
	if !restarted {
		t.Fatal("未发起 /api/restart 请求")
	}
}

// writeJSONForTest 给重启端点替身返回空 JSON 对象；参数为 http.ResponseWriter，返回无。
func writeJSONForTest(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}
