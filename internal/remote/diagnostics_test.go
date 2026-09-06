package remote

// 诊断回归使用本地 HTTP 服务和协议字节流，不依赖公共 DERP 网络。
import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDiagnosticMarkersRejectEchoAndBoundMemory 验证脚本回显不算成功且输出内存有界；参数 t 为测试对象；无返回，误判时失败。
func TestDiagnosticMarkersRejectEchoAndBoundMemory(t *testing.T) {
	d := &diagnosticMarkers{}
	_, _ = d.Write([]byte("printf 'PROXYD_DIAG_READY'; printf 'PROXYD_DIAG_DONE'\n"))
	if d.ready || d.done {
		t.Fatal("脚本回显被当作结果")
	}
	_, _ = d.Write([]byte(strings.Repeat("x", 1<<20)))
	if len(d.line) > 4096 {
		t.Fatal("输出缓冲无界")
	}
	_, _ = d.Write([]byte("PROXYD_DIAG_READY\n"))
	if d.ready {
		t.Fatal("截断行被当作协议")
	}
	_, _ = d.Write([]byte("PROXYD_DIAG_READY\r\nPATH=/usr/bin\r\nSHELL=/bin/sh\r\nPROXYD_DIAG_DONE\r\n"))
	if !d.ready || !d.done || !d.path || !d.shell {
		t.Fatal("正常环境未识别")
	}
}

// TestNetworkDiagnosticsCancellationAndPrivateSource 验证私有地图失败不会回退公网且取消及时；参数 t 为测试对象；无返回，越界或泄密时失败。
func TestNetworkDiagnosticsCancellationAndPrivateSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	m := NewManager(t.TempDir(), nil)
	defer m.Close()
	m.cfg.DERPMapURL = server.URL + "/private-token"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	checks := m.DiagnoseNetwork(ctx)
	if time.Since(start) > time.Second {
		t.Fatal("取消未及时返回")
	}
	if len(checks) != 2 || checks[1].Status != "failed" {
		t.Fatalf("错误结果: %+v", checks)
	}
	for _, check := range checks {
		if strings.Contains(check.Detail, "private-token") || strings.Contains(check.ID, "fallback") {
			t.Fatal("私有来源泄漏或越界回退")
		}
	}
}
