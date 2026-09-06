package remote

// 本文件适配有界 DNS、DERP 与 SSH 检查；报告只输出固定提示，不传播原始网络错误或 shell 内容。
import (
	"bufio"
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// DiagnosticCheck 是远程适配器的稳定结果，不向调用者暴露 SDK 对象。
type DiagnosticCheck struct {
	ID, Status, Detail string
	DurationMS         int64
}

// DiagnoseNetwork 检查配置地图来源的 DNS 和地图读取；参数 ctx 为总期限；返回固定检查结果，无原始 error。
// 默认来源失败继续检查官方备用来源；私有地图保持隔离。每个网络操作最多三秒。
func (m *Manager) DiagnoseNetwork(ctx context.Context) []DiagnosticCheck {
	m.mu.Lock()
	cfg := m.cfg.Clone()
	m.mu.Unlock()
	if cfg.Disabled {
		return []DiagnosticCheck{{ID: "derp", Status: "skipped", Detail: "远程访问模块已禁用"}}
	}
	if strings.Contains(cfg.Region, ".") || strings.Contains(cfg.Region, ":") {
		return []DiagnosticCheck{{ID: "derp", Status: "skipped", Detail: "使用自建中继，实际可达性由设备隧道检查验证"}}
	}
	sources := []string{cfg.DERPMapURL}
	if cfg.DERPMapURL == "" {
		sources = []string{"https://tailcat.dev/derpmap.json", fallbackDERPMapURL}
	}
	result := []DiagnosticCheck{}
	for i, source := range sources {
		label := "主地图"
		suffix := "primary"
		if i > 0 {
			label = "备用地图"
			suffix = "fallback"
		}
		start := time.Now()
		u, err := url.Parse(source)
		dnsCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err == nil && u.Hostname() != "" {
			_, err = net.DefaultResolver.LookupHost(dnsCtx, u.Hostname())
		} else {
			cancel()
			result = append(result, DiagnosticCheck{ID: "dns_" + suffix, Status: "failed", Detail: label + "地址无效"})
			continue
		}
		cancel()
		status, detail := "passed", label+"域名解析成功"
		if err != nil {
			status, detail = "failed", label+"域名解析失败，请检查 DNS 或地图地址"
		}
		result = append(result, DiagnosticCheck{ID: "dns_" + suffix, Status: status, Detail: detail, DurationMS: time.Since(start).Milliseconds()})
		if err != nil {
			continue
		}
		start = time.Now()
		fetchCtx, stop := context.WithTimeout(ctx, 3*time.Second)
		dm, fetchErr := tailcat.FetchDERPMap(fetchCtx, tailcat.DERPMapURL(source), tailcat.ExpandForServer)
		stop()
		usable := false
		if fetchErr == nil && dm != nil {
			for _, region := range dm.Regions {
				usable = usable || usableDERPRegion(region)
			}
		}
		status, detail = "passed", label+"可读取且包含中继节点（不代表节点已连通）"
		if !usable {
			status, detail = "failed", label+"读取失败或没有有效中继节点"
		}
		result = append(result, DiagnosticCheck{ID: "derp_" + suffix, Status: status, Detail: detail, DurationMS: time.Since(start).Milliseconds()})
		if usable {
			break
		}
	}
	return result
}

// diagnosticConn 保留读取 SSH 标识时预读的字节，防止后续握手丢失协议数据。
type diagnosticConn struct {
	net.Conn
	reader *bufio.Reader
}

// Read 从预读缓冲继续消费；参数 p 为目标切片；返回读取数量/error，沿用连接超时。
func (c *diagnosticConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// diagnosticMarkers 只保存协议进度及环境是否存在；单行最多 4 KiB，拒绝保存任意启动输出。
type diagnosticMarkers struct {
	mu                                 sync.Mutex
	line                               []byte
	overflow, ready, done, path, shell bool
}

// Write 消费有界协议标记；参数 p 为 SSH 字节流；返回原始长度/nil，不让无限输出增加内存。
// 只接受独立完整行，避免 PTY 回显的脚本正文被当作已完成的环境检查。
func (d *diagnosticMarkers) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			if !d.overflow {
				line := strings.TrimSuffix(string(d.line), "\r")
				switch line {
				case "PROXYD_DIAG_READY":
					d.ready = true
				case "PROXYD_DIAG_DONE":
					d.done = d.ready
				default:
					if d.ready && !d.done {
						d.path = d.path || strings.HasPrefix(line, "PATH=") && len(line) > 5
						d.shell = d.shell || strings.HasPrefix(line, "SHELL=") && len(line) > 6
					}
				}
			}
			d.line = d.line[:0]
			d.overflow = false
		} else if len(d.line) < 4096 {
			d.line = append(d.line, b)
		} else {
			d.overflow = true
		}
	}
	return len(p), nil
}

// DiagnoseSSH 建立命名设备隧道并检查 SSH 握手和真实登录环境。
// 参数 ctx 为有界上下文、token 为应用已解析的凭据；返回脱敏分步结果，失败后的依赖步骤明确跳过。
// Web/CLI 共用守护进程身份；只使用守护进程可访问的 SSH agent，不读取或上传用户私钥。
func (m *Manager) DiagnoseSSH(ctx context.Context, token string) []DiagnosticCheck {
	result := []DiagnosticCheck{{ID: "tunnel", Status: "skipped", Detail: "尚未建立隧道"}, {ID: "ssh_banner", Status: "skipped", Detail: "等待隧道连接"}, {ID: "ssh_auth", Status: "skipped", Detail: "等待 SSH 服务"}, {ID: "shell", Status: "skipped", Detail: "等待 SSH 认证"}}
	m.mu.Lock()
	if m.cfg.Disabled {
		m.mu.Unlock()
		result[0].Detail = "远程访问模块已禁用"
		return result
	}
	runtimeCtx := m.runtimeCtx
	priv := m.clientKeyLocked()
	keyErr := m.clientErr
	m.mu.Unlock()
	if keyErr != nil {
		result[0].Status = "failed"
		result[0].Detail = "无法加载客户端身份，请检查状态目录权限"
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	stopRuntime := context.AfterFunc(runtimeCtx, cancel)
	defer stopRuntime()
	start := time.Now()
	dialCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	conn, err := Dial(dialCtx, token, 22, priv)
	stop()
	result[0].DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		result[0].Status = "failed"
		result[0].Detail = "隧道连接失败，请检查设备在线状态、中继和客户端白名单"
		return result
	}
	defer conn.Close()
	result[0].Status = "passed"
	result[0].Detail = "已连接远端隧道 22 端口"
	return append(result[:1], diagnoseSSHTransport(ctx, conn)...)
}

// diagnoseSSHTransport 在已认证的隧道连接上执行 SSH 检查，也供隔离回环回归测试使用。
// 参数 ctx 为取消上下文、conn 为调用者拥有的连接；返回三个阶段结果，取消会关闭连接解开阻塞。
func diagnoseSSHTransport(ctx context.Context, conn net.Conn) []DiagnosticCheck {
	result := []DiagnosticCheck{{ID: "ssh_banner", Status: "skipped", Detail: "等待 SSH 服务"}, {ID: "ssh_auth", Status: "skipped", Detail: "等待 SSH 标识"}, {ID: "shell", Status: "skipped", Detail: "等待 SSH 认证"}}
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	start := time.Now()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(conn, 4096)
	banner, err := reader.Peek(4)
	result[0].DurationMS = time.Since(start).Milliseconds()
	if err != nil || string(banner) != "SSH-" {
		result[0].Status = "failed"
		result[0].Detail = "远端未返回 SSH 标识，请检查 22 端口服务"
		return result
	}
	result[0].Status = "passed"
	result[0].Detail = "远端 SSH 服务已响应"
	methods := []ssh.AuthMethod{}
	if socket := os.Getenv("SSH_AUTH_SOCK"); socket != "" {
		agentConn, e := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
		if e == nil {
			defer agentConn.Close()
			stopAgent := context.AfterFunc(ctx, func() { _ = agentConn.Close() })
			defer stopAgent()
			_ = agentConn.SetDeadline(time.Now().Add(8 * time.Second))
			signers, e := agent.NewClient(agentConn).Signers()
			if e == nil {
				methods = append(methods, ssh.PublicKeys(signers...))
			}
		}
	}
	start = time.Now()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// token 中的 WireGuard 身份已认证目标，SSH host key 仅在该加密隧道内部接受，不开放普通 TCP 跳过校验。
	cc, channels, requests, err := ssh.NewClientConn(&diagnosticConn{Conn: conn, reader: reader}, "tailcat", &ssh.ClientConfig{User: "proxyd", Auth: methods, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	result[1].DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		result[1].Status = "failed"
		result[1].Detail = "SSH 握手或认证失败；需要客户端密钥时，请在客户端运行 proxyd ssh <设备> --diagnose -i <密钥文件>"
		return result
	}
	client := ssh.NewClient(cc, channels, requests)
	defer client.Close()
	result[1].Status = "passed"
	result[1].Detail = "SSH 握手与认证成功"
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	start = time.Now()
	session, err := client.NewSession()
	if err != nil {
		result[2].Status = "failed"
		result[2].Detail = "无法创建 SSH 会话"
		return result
	}
	defer session.Close()
	markers := &diagnosticMarkers{}
	// RequestSubsystem 不启动 x/crypto 的命令拷贝循环，因此不能使用 Session.Wait。
	// 显式消费两个管道，避免启动文件大量 stderr 输出耗尽 SSH 窗口；取消时关闭会话并等待回收。
	stdout, pipeErr := session.StdoutPipe()
	if pipeErr != nil {
		result[2].Status = "failed"
		result[2].Detail = "无法读取诊断输出"
		return result
	}
	stderr, pipeErr := session.StderrPipe()
	if pipeErr != nil {
		result[2].Status = "failed"
		result[2].Detail = "无法读取诊断输出"
		return result
	}
	stderrDone := make(chan struct{})
	go func() { defer close(stderrDone); _, _ = io.Copy(io.Discard, stderr) }()
	defer func() { _ = session.Close(); <-stderrDone }()
	if err = session.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
		result[2].Status = "failed"
		result[2].Detail = "远端拒绝 PTY 请求"
		return result
	}
	if err = session.RequestSubsystem("proxyd-diagnostics"); err != nil {
		result[2].Status = "skipped"
		result[2].Detail = "远端未提供 proxyd 诊断子系统，请升级远端或使用普通 SSH 检查环境"
		return result
	}
	_, err = io.Copy(markers, stdout)
	result[2].DurationMS = time.Since(start).Milliseconds()
	markers.mu.Lock()
	ready := markers.ready && markers.done && markers.path && markers.shell
	markers.mu.Unlock()
	if err != nil || !ready {
		result[2].Status = "failed"
		result[2].Detail = "登录环境加载失败、超时或缺少 PATH/SHELL，请检查运行用户的启动文件"
		return result
	}
	result[2].Status = "passed"
	result[2].Detail = "交互登录 shell 已完成加载，PATH 与 SHELL 非空（不验证每个自定义命令）"
	return result
}
