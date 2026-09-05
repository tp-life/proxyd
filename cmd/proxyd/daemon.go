package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"proxyd/internal/config"
)

// 后台守护模式：start/stop/restart/status。
// pid 文件由 serve 进程自身登记/清理（state-dir/proxyd.pid），
// 日志重定向到 state-dir/proxyd.log。

const daemonHealthTimeout = time.Second

// daemonHealthClient 专用于 start/status 的本地健康探测。
// 不使用无超时的 http.DefaultClient，因为某个占用 API 端口但不返回
// HTTP 响应的异常进程，会让原本“最长等待 10 秒”的启动流程永久卡住。
var daemonHealthClient = &http.Client{Timeout: daemonHealthTimeout}

func pidPath(cfg *config.Config) string { return filepath.Join(cfg.StateDir, "proxyd.pid") }

func logPathFor(cfg *config.Config) string { return filepath.Join(cfg.StateDir, "proxyd.log") }

// readPIDFile 读 pid 文件并检查进程是否存活；文件缺失/损坏/进程已退出都返回 alive=false。
func readPIDFile(path string) (pid int, alive bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, pidAlive(pid)
}

func writePIDFile(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// loadConfigFile 仅供 start/stop/status 等读取配置文件（不合并订阅地址）。
func loadConfigFile(name string, args []string) (*config.Config, string, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfgFile := fs.String("c", config.DefaultPath(), "配置文件路径")
	_ = fs.Parse(args)
	cfg, err := config.Load(*cfgFile)
	if err != nil {
		return nil, "", fmt.Errorf("读取配置 %s 失败: %w", *cfgFile, err)
	}
	return cfg, *cfgFile, nil
}

// cmdStart 后台启动，已由系统托管时等待系统实例，否则派生 serve。
// 参数：args 为 []string，配置及订阅参数。返回：error，启动成功时为 nil。
// 错误：配置、权限、系统查询或就绪等待失败；系统托管分支绝不回退到独立派生。
func cmdStart(args []string) error {
	cfg, cfgPath, err := loadConfigOrRepair(args, true)
	if err != nil {
		return err
	}
	// 子进程 detached 后无法交互，权限修复必须在父进程完成
	if err := offerStateDirRepair(cfg); err != nil {
		return err
	}
	if handled, err := managedStart(cfg, cfgPath, false); handled {
		return err
	}
	if pid, alive := readPIDFile(pidPath(cfg)); alive {
		return fmt.Errorf("proxyd 已在运行 (pid %d)", pid)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	cfgAbs, err := filepath.Abs(cfgPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	pid, err := spawnDaemon(exe, cfgAbs, logPathFor(cfg))
	if err != nil {
		return fmt.Errorf("启动后台进程失败: %w", err)
	}

	// 就绪等待：轮询 API healthz，最长 10s
	base := "http://" + cfg.APIListen
	ready := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return fmt.Errorf("后台进程 (pid %d) 启动后即退出，请查看日志 %s", pid, logPathFor(cfg))
		}
		if healthEndpointResponds(daemonHealthClient, base, cfg.APISecret) {
			ready = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	fmt.Printf("proxyd 已启动 (pid %d)\n", pid)
	fmt.Printf("web 控制台: %s/\n", base)
	fmt.Printf("主端口(规则模式): %s:%d，节点映射区间: %d-%d\n", cfg.Listen, cfg.MixedPort, cfg.PortRange[0], cfg.PortRange[1])
	fmt.Printf("日志: %s\n", logPathFor(cfg))
	if !ready {
		fmt.Println("注意：就绪等待超时，服务可能仍在初始化（首次需拉取订阅/下载 geo 数据），可稍后 proxyd status 确认")
	}
	return nil
}

// cmdStop 停止后台实例：SIGTERM，等待退出，清理 pid 文件；兼容 stale pid。
func cmdStop(args []string) error {
	cfg, _, err := loadConfigFile("stop", args)
	if err != nil {
		return err
	}
	path := pidPath(cfg)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("proxyd 未在运行")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		_ = os.Remove(path)
		return fmt.Errorf("pid 文件损坏，已清理；proxyd 视为未在运行")
	}
	if !pidAlive(pid) {
		_ = os.Remove(path)
		return fmt.Errorf("proxyd 未在运行（已清理过期 pid 文件）")
	}
	if err := terminate(pid); err != nil {
		return fmt.Errorf("发送退出信号失败: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			_ = os.Remove(path) // serve 退出时也会删，幂等
			fmt.Println("proxyd 已停止")
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("等待进程 %d 退出超时（仍在运行）", pid)
}

// cmdRestart 重启当前配置的实例，系统托管时由 KeepAlive 创建替代进程。
// 参数：args 为 []string，配置参数。返回：error。错误：配置、停止或启动失败。
func cmdRestart(args []string) error {
	cfg, path, err := loadConfigFile("restart", args)
	if err != nil {
		return err
	}
	if handled, err := managedStart(cfg, path, true); handled {
		return err
	}
	if err := cmdStop(args); err != nil && !strings.Contains(err.Error(), "未在运行") {
		return err
	}
	return cmdStart(args)
}

// cmdStatus 显示后台实例状态：pid、监听端口、web 地址。
func cmdStatus(args []string) error {
	cfg, cfgPath, err := loadConfigFile("status", args)
	if err != nil {
		return err
	}
	pid, alive := readPIDFile(pidPath(cfg))
	if !alive {
		fmt.Println("proxyd 未在运行")
		return nil
	}
	fmt.Printf("proxyd 运行中 (pid %d)\n", pid)
	fmt.Printf("主端口(规则模式): %s:%d，节点映射区间: %d-%d\n", cfg.Listen, cfg.MixedPort, cfg.PortRange[0], cfg.PortRange[1])
	if cfg.AutoPort > 0 {
		fmt.Printf("自动选优端口: %d\n", cfg.AutoPort)
	}
	base := "http://" + cfg.APIListen
	fmt.Printf("web 控制台: %s/\n", base)
	apiUp := false
	if healthEndpointResponds(daemonHealthClient, base, cfg.APISecret) {
		fmt.Println("API: 正常")
		apiUp = true
	} else {
		fmt.Println("API: 无响应（可能在初始化，或进程异常）")
	}
	if apiUp {
		if c, err := newAPIClient(cfgPath); err == nil {
			printOverviewSummary(c)
		}
	}
	fmt.Printf("日志: %s\n", logPathFor(cfg))
	return nil
}

// healthEndpointResponds 检查目标 API 是否能在调用方给定的超时内返回 HTTP 响应。
//
// 参数说明：
//   - client: *http.Client，必须配置有限超时，生产调用使用 daemonHealthClient。
//   - base: string，API 基础地址，例如 http://127.0.0.1:19091。
//   - secret: string，可选管理面 Basic Auth 口令；非空时用户名固定为 proxyd。
//
// 返回值说明：bool，收到 healthz 的 HTTP 204 或兼容旧版的 HTTP 200 时为 true。
//
// 错误情况：拨号失败、超时或读取响应头失败都返回 false；
// 认证失败、服务错误等其他响应返回 false；响应体无论状态码如何都会
// 立即关闭，使 keep-alive 与 fd 不泄漏。
func healthEndpointResponds(client *http.Client, base, secret string) bool {
	request, err := http.NewRequest(http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return false
	}
	if secret != "" {
		request.SetBasicAuth("proxyd", secret)
	}
	resp, err := client.Do(request)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent
}

// printOverviewSummary 在 status 输出后追加运行中实例的汇总信息；失败时静默跳过，不影响基础状态输出。
func printOverviewSummary(c *apiClient) {
	ov, err := c.overview()
	if err != nil {
		return
	}
	fmt.Printf("模式: %s\n", ov.Mode)
	alive, total := 0, len(ov.Nodes)
	for _, n := range ov.Nodes {
		if n.Alive {
			alive++
		}
	}
	mapping := "关闭"
	if ov.PortMappingEnabled {
		mapping = fmt.Sprintf("开启（%d 个节点端口）", len(ov.Ports))
	}
	fmt.Printf("节点: %d/%d 可用；端口映射: %s（区间 %d-%d）\n",
		alive, total, mapping, ov.PortRange[0], ov.PortRange[1])
	if ov.AutoPort > 0 {
		fmt.Printf("自动选优端口: %d\n", ov.AutoPort)
	}
	main := "规则模式"
	switch {
	case ov.MainAuto:
		main = "固定走最优节点（main-auto）"
	case ov.MainNode != "" && ov.MainNodeUp:
		main = "固定节点（main-node，生效中）"
	case ov.MainNode != "":
		main = "固定节点（main-node，节点暂不可用，已回退规则模式）"
	}
	fmt.Printf("主端口: %s\n", main)
	fmt.Printf("系统代理: %s；TUN: %s；DNS 预设: %s；开机自启: %s\n",
		onOffText(ov.SystemProxy), onOffText(ov.TUN.Enabled), ov.DNSPreset, onOffText(ov.Autostart))
	if ov.Version.Enabled && ov.Version.Latest != "" && ov.Version.Latest != ov.Version.Current {
		fmt.Printf("发现新版本: %s（当前 %s，%s）\n", ov.Version.Latest, ov.Version.Current, ov.Version.URL)
	}
}

func onOffText(on bool) string {
	if on {
		return "开启"
	}
	return "关闭"
}
