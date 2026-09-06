package main

// 本文件实现有界 SSH 分阶段诊断；认证仍由系统 OpenSSH 处理，沿用私钥文件与 ssh-agent。

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"proxyd/internal/remote"
)

// diagnosticOutput 仅转发固定标记与白名单环境字段，避免启动脚本输出泄露其他环境变量。
type diagnosticOutput struct {
	out           io.Writer
	pending       string
	authenticated bool
	ready         bool
	done          bool
}

// Write 逐行消费子系统输出并更新诊断进度。
// 参数说明：data 为 []byte，SSH 标准输出片段。
// 返回值说明：int 与 error，返回原始长度与 nil；会话结果由完整标记判断。
// 错误情况：单行超过 64 KiB 则丢弃，避免故障启动脚本耗尽客户端内存。
func (d *diagnosticOutput) Write(data []byte) (int, error) {
	d.pending += string(data)
	for {
		line, rest, ok := strings.Cut(d.pending, "\n")
		if !ok {
			break
		}
		d.pending = rest
		line = strings.TrimSpace(line)
		switch line {
		case "PROXYD_DIAG_AUTHENTICATED":
			d.authenticated = true
			fmt.Fprintln(d.out, "[通过] SSH 握手与认证\n[检查] 交互登录 shell 与用户环境")
		case "PROXYD_DIAG_READY":
			d.ready = true
			fmt.Fprintln(d.out, "[通过] 交互登录 shell 已加载")
		case "PROXYD_DIAG_DONE":
			d.done = true
		default:
			if d.ready && !d.done {
				for _, prefix := range []string{"USER=", "SHELL=", "TERM=", "PATH=", "SSH_TTY="} {
					if strings.HasPrefix(line, prefix) {
						fmt.Fprintln(d.out, strings.Map(func(r rune) rune {
							if unicode.IsControl(r) {
								return -1
							}
							return r
						}, line))
						break
					}
				}
			}
		}
	}
	if len(d.pending) > 64*1024 {
		d.pending = ""
	}
	return len(data), nil
}

// diagnosticErrors 保存有限的 OpenSSH 错误输出，用于失败归因。
type diagnosticErrors struct{ bytes.Buffer }

// Write 截断超过 64 KiB 的 stderr，仍报告消费了全部输入以避免子进程阻塞。
// 参数说明：data 为 []byte；返回值说明：原始长度与 nil。
// 错误情况：超量内容丢弃，不保存 token、密钥文件路径等作为持久日志。
func (d *diagnosticErrors) Write(data []byte) (int, error) {
	n := len(data)
	remaining := 64*1024 - d.Len()
	if remaining > 0 {
		_, _ = d.Buffer.Write(data[:min(remaining, n)])
	}
	return n, nil
}

// runSSHDiagnostic 检查隧道、SSH 服务响应、认证与真实交互 shell。
// 参数说明：sshExe 为 string；argv 为 []string，包含现有 OpenSSH ProxyCommand；
// cfgFile、token、clientKeyText 为 string；port 为 int。
// 返回值说明：error，全部阶段成功时为 nil。
// 错误情况：隧道最多 30 秒，服务版本最多 5 秒，认证与 shell 最多 30 秒；
// 不支持诊断子系统的旧服务端会明确提示升级，不以普通命令替代交互 shell 检查。
func runSSHDiagnostic(sshExe string, argv []string, cfgFile, token, clientKeyText string, port int) error {
	clientKey := pipeClientKey(cfgFile)
	if clientKeyText != "" {
		var err error
		clientKey, err = remote.ParseClientKey(clientKeyText)
		if err != nil {
			return err
		}
	}
	fmt.Println("[检查] tailcat 隧道与远端端口")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := remote.Dial(ctx, token, port, clientKey)
	if err != nil {
		return fmt.Errorf("隧道或端口不可达；检查 token、客户端白名单及暴露端口: %w", err)
	}
	fmt.Println("[通过] tailcat 隧道与远端端口")
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 512), 4096)
	banner := false
	for i := 0; i < 20 && scanner.Scan(); i++ {
		if strings.HasPrefix(scanner.Text(), "SSH-2.0-") {
			banner = true
			break
		}
	}
	_ = conn.Close()
	if !banner {
		return fmt.Errorf("端口可达，但未在 5 秒内收到 SSH-2.0 服务响应；检查目标端口和 SSH 服务")
	}
	fmt.Println("[通过] SSH 服务响应\n[检查] SSH 握手与认证（使用系统 OpenSSH 配置）")
	commandCtx, commandCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer commandCancel()
	cmd := exec.CommandContext(commandCtx, sshExe, argv...)
	// 固定子系统在服务端注入只读检查脚本；客户端 stdin 保持为空，避免诊断时进入不可控的交互提示。
	cmd.Stdin = strings.NewReader("")
	output := &diagnosticOutput{out: os.Stdout}
	failure := &diagnosticErrors{}
	cmd.Stdout, cmd.Stderr = output, failure
	if clientKeyText != "" {
		raw, _ := clientKey.MarshalText()
		cmd.Env = append(os.Environ(), "PROXYD_CLIENT_KEY="+string(raw))
	}
	err = cmd.Run()
	if commandCtx.Err() != nil {
		if output.authenticated {
			return fmt.Errorf("SSH 已认证，但登录 shell 在 30 秒内未完成；检查用户启动脚本中的等待、网络请求或交互命令")
		}
		return fmt.Errorf("SSH 握手或认证超过 30 秒；检查私钥、ssh-agent 和连接状态")
	}
	if !output.authenticated {
		if strings.Contains(failure.String(), "subsystem request failed") {
			return fmt.Errorf("远端不支持 proxyd-diagnostics 子系统；请升级远端 proxyd 并启用内嵌 SSH")
		}
		return fmt.Errorf("SSH 握手或认证失败；请检查 -i 私钥、公钥有效期与禁用状态；加密私钥可先用 ssh-add 加入 agent（原连接命令加 -v 可查看详细日志）")
	}
	if err != nil || !output.ready || !output.done {
		return fmt.Errorf("SSH 已认证，但交互 shell 环境检查未完成；检查服务端运行用户和登录 shell 配置")
	}
	fmt.Println("[完成] SSH 诊断通过；请核对上述 USER、SHELL 和 PATH 是否符合预期")
	return nil
}
