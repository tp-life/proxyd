package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"proxyd/internal/autostart"
)

// 本文件通过真实 CLI 入口、临时 PID 文件与 HTTP 健康响应覆盖启动所有权协调。

// TestCmdStartWaitsForManagedProcess 验证 start 遇到系统实例时幂等等待，不误报重复也不派生进程。
// 参数：t 为 *testing.T。返回：无。错误：身份校验、认证、204 响应或 CLI 协调不正确时失败。
func TestCmdStartWaitsForManagedProcess(t *testing.T) {
	// 健康服务替身校验真实请求认证；参数为 HTTP 响应和请求，返回无，认证错误输出 401。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, secret, ok := r.BasicAuth()
		if !ok || user != "proxyd" || secret != "test-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cfg := newAPITestConfig(t)
	cfg.StateDir = t.TempDir()
	cfg.APISecret = "test-secret"
	cfg.APIListen = strings.TrimPrefix(server.URL, "http://")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := writePIDFile(pidPath(cfg), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	previousManaged, previousInspect := managedService, inspectService
	// 恢复适配器闭包：无参数、无返回和错误，保证其他测试不受替身影响。
	defer func() { managedService, inspectService = previousManaged, previousInspect }()
	// 所有权替身校验 CLI 传递的配置路径；参数为绝对路径，返回匹配结果，无错误。
	managedService = func(got string) (bool, error) { return got == path, nil }
	// 运行态替身指向测试自身的有效 PID；无参数，返回快照，无错误。
	inspectService = func() autostart.RuntimeStatus {
		return autostart.RuntimeStatus{Loaded: true, Running: true, PID: os.Getpid(), State: "running"}
	}
	if err := cmdStart([]string{"-c", path}); err != nil {
		t.Fatalf("系统实例已健康，start 应成功: %v", err)
	}
	// 查询错误不能回退到旧的独立启动分支；参数未使用，返回模拟故障。
	managedService = func(string) (bool, error) { return false, fmt.Errorf("system-query-failed") }
	if err := cmdStart([]string{"-c", path}); err == nil || !strings.Contains(err.Error(), "system-query-failed") {
		t.Fatalf("系统查询错误未被保留: %v", err)
	}
	// 即使健康端点正常，旧 PID 和不匹配的托管 PID 都不代表系统替代实例已经就绪。
	if err := waitManagedService(cfg, os.Getpid(), time.Now().Add(time.Millisecond)); err == nil {
		t.Fatal("旧实例不得满足重启就绪条件")
	}
	// 不匹配 PID 替身：无参数，返回另一实例的快照，不访问或发送任何系统信号。
	inspectService = func() autostart.RuntimeStatus {
		return autostart.RuntimeStatus{Loaded: true, Running: true, PID: os.Getpid() + 1, State: "running"}
	}
	if err := waitManagedService(cfg, 0, time.Now().Add(time.Millisecond)); err == nil {
		t.Fatal("其他实例的健康响应不得满足托管就绪条件")
	}
}

// TestCmdRestartPreservesManagedOwnership 验证 restart 查询系统所有权后请求退出，终止失败时不得继续派生。
// 参数：t 为 *testing.T。返回：无。错误：未终止配置 PID、吞掉信号错误或走独立启动分支时失败。
func TestCmdRestartPreservesManagedOwnership(t *testing.T) {
	cfg := newAPITestConfig(t)
	cfg.StateDir = t.TempDir()
	cfg.APISecret = "test-secret"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := writePIDFile(pidPath(cfg), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	previousManaged, previousStop := managedService, requestManagedStop
	// 恢复适配器闭包：无参数、无返回值，无错误。
	defer func() { managedService, requestManagedStop = previousManaged, previousStop }()
	// 匹配服务替身：string 参数未使用，返回 true、nil，不操作本机服务。
	managedService = func(string) (bool, error) { return true, nil }
	// 终止替身只检查 PID 后模拟错误；绝不对测试进程发送信号。
	requestManagedStop = func(pid int) error {
		if pid != os.Getpid() {
			t.Fatalf("请求了错误的 PID: %d", pid)
		}
		return fmt.Errorf("stop-request-failed")
	}
	if err := cmdRestart([]string{"-c", path}); err == nil || !strings.Contains(err.Error(), "stop-request-failed") {
		t.Fatalf("未保留托管重启的信号错误: %v", err)
	}
	if pid, alive := readPIDFile(pidPath(cfg)); !alive || pid != os.Getpid() {
		t.Fatal("重启失败不应删除仍在运行实例的 PID")
	}
}
