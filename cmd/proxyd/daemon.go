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

// cmdStart 后台启动：派生 detached 子进程执行 serve，日志落 state-dir/proxyd.log。
func cmdStart(args []string) error {
	cfg, cfgPath, err := loadConfig(args, true)
	if err != nil {
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
		if resp, err := http.Get(base + "/healthz"); err == nil {
			_ = resp.Body.Close()
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

func cmdRestart(args []string) error {
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
	if resp, err := http.Get(base + "/healthz"); err == nil {
		_ = resp.Body.Close()
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
