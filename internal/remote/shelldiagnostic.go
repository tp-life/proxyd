//go:build linux || darwin || windows

package remote

// 本文件在已认证 SSH 子系统中检查真实交互登录 shell，使用固定只读命令采集白名单环境字段。
// 不读取客户端私钥，不输出完整 env，也不修改用户启动文件。

import (
	"io"
	"os/user"
	"runtime"

	ssh "github.com/tailscale/gliderssh"
)

// diagnosticShellSession 复用真实会话的输出与 PTY，并隔离客户端交互输入。
type diagnosticShellSession struct {
	ssh.Session
}

// Read 阻止客户端向固定诊断命令追加任意键盘输入。
// 参数说明：buffer 为 []byte，调用方缓冲。
// 返回值说明：始终返回 0 与 io.EOF；脚本通过服务端命令参数执行。
// 错误情况：不读取真实客户端输入，也不修改 PTY 的字节流转发规则。
func (s *diagnosticShellSession) Read(buffer []byte) (int, error) { return 0, io.EOF }

// shellDiagnosticHandler 在认证后标记进度，再运行加载用户环境的交互登录 shell。
// 参数说明：sess 为 ssh.Session，客户端应申请 PTY 并设置总超时。
// 返回值说明：无，环境字段与进度标记通过 SSH 输出，退出码由 shell 回传。
// 错误情况：用户查询失败、未申请 PTY、shell 启动失败时返回非零；客户端断开沿用会话清理逻辑。
func shellDiagnosticHandler(sess ssh.Session) {
	io.WriteString(sess, "PROXYD_DIAG_AUTHENTICATED\n")
	if _, _, ok := sess.Pty(); !ok {
		io.WriteString(sess.Stderr(), "SSH 诊断需要 PTY\n")
		sess.Exit(1)
		return
	}
	u, err := user.Current()
	if err != nil {
		io.WriteString(sess.Stderr(), "无法查询运行用户\n")
		sess.Exit(1)
		return
	}
	runShellDiagnostic(sess, u)
}

// runShellDiagnostic 为已经确认的进程用户构造交互环境探针。
// 参数说明：sess 为 ssh.Session；u 为 *user.User，必须由服务端解析，不接受客户端用户切换。
// 返回值说明：无，探针输出通过原始会话传回。
// 错误情况：启动文件阻塞时由客户端超时关闭传输，服务端沿用原有子进程清理。
func runShellDiagnostic(sess ssh.Session, u *user.User) {
	script := "printf '\\nPROXYD_DIAG_READY\\n'; printf 'USER='; printenv USER; printf 'SHELL='; printenv SHELL; printf 'TERM='; printenv TERM; printf 'PATH='; printenv PATH; printf 'SSH_TTY='; printenv SSH_TTY; printf 'PROXYD_DIAG_DONE\\n'; exit\n"
	if runtime.GOOS == "windows" {
		script = "Write-Output ''; Write-Output 'PROXYD_DIAG_READY'; Write-Output ('USER=' + $env:USERNAME); Write-Output ('SHELL=' + (Get-Process -Id $PID).Path); Write-Output ('TERM=' + $env:TERM); Write-Output ('PATH=' + $env:PATH); Write-Output 'SSH_TTY=ConPTY'; Write-Output 'PROXYD_DIAG_DONE'; exit\r\n"
	}
	cmd := newShellDiagnosticCommand(u, script)
	runShellSessionCommand(&diagnosticShellSession{Session: sess}, cmd)
}
