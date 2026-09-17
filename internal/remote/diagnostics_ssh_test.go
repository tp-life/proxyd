//go:build linux || darwin || windows

package remote

// 真实 SSH 传输回归覆盖认证拒绝与登录卡住，无公网依赖，不读取开发者启动文件。
import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	gliderssh "github.com/tailscale/gliderssh"
)

// TestSSHDiagnosticsAuthenticationFailure 验证认证失败定位且不继续执行 shell；参数 t 为测试对象；无返回，阶段错误时失败。
func TestSSHDiagnosticsAuthenticationFailure(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	handler, err := configuredShellSSHHandler(t.TempDir(), true, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	conn, err := openAuthenticatedLoopback(ctx, handler)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	checks := diagnoseSSHTransport(ctx, conn)
	if checks[0].Status != "passed" || checks[1].Status != "failed" || checks[2].Status != "skipped" {
		t.Fatalf("认证失败定位错误: %+v", checks)
	}
}

// TestSSHDiagnosticsShellAndCancellation 验证完成标记和卡住时的取消回收；参数 t 为测试对象；无返回，超时失效或误判成功时失败。
func TestSSHDiagnosticsShellAndCancellation(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	for _, hang := range []bool{false, true} {
		name := "success"
		if hang {
			name = "blocked-shell"
		}
		t.Run(name, func(t *testing.T) {
			signer, err := loadOrCreateShellHostKey(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			server := &gliderssh.Server{ChannelHandlers: map[string]gliderssh.ChannelHandler{"session": gliderssh.DefaultSessionHandler}, SubsystemHandlers: map[string]gliderssh.SubsystemHandler{
				// 固定探针模拟 shell 完成或启动文件阻塞；参数会话由测试服务拥有，取消必须结束等待。
				"proxyd-diagnostics": func(session gliderssh.Session) {
					if hang {
						<-session.Context().Done()
						return
					}
					_, _ = io.WriteString(session, "PROXYD_DIAG_READY\nPATH=/usr/bin\nSHELL=/bin/sh\nPROXYD_DIAG_DONE\n")
					_ = session.Exit(0)
				},
			}}
			server.AddHostKey(signer)
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
			defer cancel()
			conn, err := openAuthenticatedLoopback(ctx, server.HandleConn)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			start := time.Now()
			checks := diagnoseSSHTransport(ctx, conn)
			if time.Since(start) > 2*time.Second {
				t.Fatal("SSH 取消未及时生效")
			}
			want := "passed"
			if hang {
				want = "failed"
			}
			if checks[2].Status != want {
				t.Fatalf("shell 结果错误: %+v", checks)
			}
		})
	}
}

// TestSSHDiagnosticsBannerCancellation 验证未发送标识的 TCP 服务无法永久占用检查；参数 t 为测试对象；无返回，未取消时失败。
func TestSSHDiagnosticsBannerCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	checks := diagnoseSSHTransport(ctx, client)
	if checks[0].Status != "failed" || checks[1].Status != "skipped" {
		t.Fatal("标识超时未隔离后续阶段")
	}
}
