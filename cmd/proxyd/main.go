// proxyd — 多节点端口映射代理工具。
// 将多个订阅的可用节点各映射到一个本地端口（HTTP+SOCKS5 混合），
// 另有一个主端口走常规 Clash 规则模式。
//
// 最简单的用法：proxyd serve <订阅地址> [更多订阅地址...]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"text/tabwriter"

	"proxyd/internal/api"
	"proxyd/internal/app"
	"proxyd/internal/config"
	"proxyd/internal/node"
	"proxyd/internal/pool"
	"proxyd/internal/subscribe"
	"proxyd/internal/sysproxy"
)

var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "sysproxy":
		err = cmdSysproxy(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Printf("proxyd %s\n", version)
	default:
		// 快捷形式：proxyd <订阅地址> 等价于 proxyd serve <订阅地址>
		if looksLikeURL(os.Args[1]) {
			err = cmdServe(os.Args[1:])
		} else {
			usage()
			os.Exit(2)
		}
	}
	if err != nil {
		log.Fatalf("error: %v", err)
	}
}

func looksLikeURL(s string) bool {
	return len(s) > 8 && (s[:7] == "http://" || s[:8] == "https://")
}

func usage() {
	fmt.Fprintf(os.Stderr, `proxyd %s — 多节点端口映射代理工具

usage:
  proxyd serve [flags] [订阅地址...]   常驻运行（映射端口 + 定时刷新/检测 + Web 控制台）
  proxyd check [flags] [订阅地址...]   一次性拉取订阅、测速并打印端口映射表
  proxyd sysproxy on|off|status [-c 配置文件]   开关/查看系统代理（指向主端口）
  proxyd <订阅地址>                    serve 的快捷形式
  proxyd version                       打印版本

flags:
  -c <文件>      配置文件路径（默认 ~/.config/proxyd/config.yaml；命令行给的订阅地址会自动保存进去）
  -range <区间>  节点映射端口区间，如 42000-42100（默认 %s）

examples:
  proxyd serve https://example.com/sub?token=xxx
  proxyd serve -range 43000-43100 https://a.com/sub https://b.com/link
  proxyd serve -c proxyd.yaml
`, version, config.DefaultPortRange)
}

// loadConfig 统一 serve/check 的配置来源：
//  1. 配置文件存在（-c 指定，默认 ~/.config/proxyd/config.yaml）→ 加载，
//     位置参数中的新订阅地址按 URL 去重后合并进去；
//  2. 否则用位置参数的订阅地址走快捷配置（默认端口区间/规则/周期）。
//
// persist 为 true 时（serve）把合并结果写回配置文件，实现"给一次地址以后直接用"。
// 返回配置与配置文件路径。
func loadConfig(args []string, persist bool) (*config.Config, string, error) {
	fs := flag.NewFlagSet("proxyd", flag.ExitOnError)
	cfgFile := fs.String("c", config.DefaultPath(), "配置文件路径")
	portRange := fs.String("range", "", "节点映射端口区间，如 42000-42100")
	_ = fs.Parse(args)
	urls := fs.Args()

	if _, err := os.Stat(*cfgFile); err != nil {
		// 配置文件不存在
		if len(urls) == 0 {
			return nil, "", fmt.Errorf("未找到配置文件 %q，也未给出订阅地址；用法见 proxyd（不带参数）", *cfgFile)
		}
		cfg, err := config.Quick(urls, *portRange)
		if err != nil {
			return nil, "", err
		}
		if persist {
			if err := cfg.Save(*cfgFile); err != nil {
				log.Printf("[proxyd] 保存配置失败（不影响本次运行）: %v", err)
			} else {
				log.Printf("[proxyd] 订阅已保存到 %s，下次直接 proxyd serve 即可", *cfgFile)
			}
		}
		return cfg, *cfgFile, nil
	}

	cfg, err := config.Load(*cfgFile)
	if err != nil {
		return nil, "", err
	}
	changed := false
	for _, u := range urls {
		exists := false
		for _, s := range cfg.Subscriptions {
			if s.URL == u {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		cfg.Subscriptions = append(cfg.Subscriptions, config.Subscription{
			Name: fmt.Sprintf("cli-%d", len(cfg.Subscriptions)+1),
			URL:  u,
			Type: "auto",
		})
		changed = true
	}
	if changed && persist {
		if err := cfg.Save(*cfgFile); err != nil {
			log.Printf("[proxyd] 保存配置失败（不影响本次运行）: %v", err)
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, "", err
	}
	return cfg, *cfgFile, nil
}

func cmdServe(args []string) error {
	cfg, cfgPath, err := loadConfig(args, true)
	if err != nil {
		return err
	}
	a, err := app.New(cfg, cfgPath)
	if err != nil {
		return err
	}

	apiSrv := api.New(cfg.APIListen, a)
	if err := apiSrv.Start(); err != nil {
		return fmt.Errorf("start api on %s: %w", cfg.APIListen, err)
	}
	log.Printf("[proxyd] web 控制台: http://%s/", cfg.APIListen)
	log.Printf("[proxyd] external controller: http://%s (可对接 metacubexd/yacd)", cfg.ExternalController)
	log.Printf("[proxyd] 主端口(规则模式): %s:%d，节点映射区间: %d-%d",
		cfg.Listen, cfg.MixedPort, cfg.PortRange[0], cfg.PortRange[1])

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	defer apiSrv.Shutdown(context.Background())

	// 配置开启系统代理时启动即应用，退出时恢复关闭
	if cfg.SystemProxy {
		if err := sysproxy.On("127.0.0.1", cfg.MixedPort); err != nil {
			log.Printf("[sysproxy] 应用系统代理失败: %v", err)
		} else {
			log.Printf("[sysproxy] 系统代理已指向 127.0.0.1:%d（退出时自动关闭）", cfg.MixedPort)
			defer func() {
				if err := sysproxy.Off(); err != nil {
					log.Printf("[sysproxy] 关闭系统代理失败（可手动 proxyd sysproxy off）: %v", err)
				}
			}()
		}
	}
	return a.Run(ctx)
}

// cmdSysproxy 开关/查看系统代理（指向配置的主端口）。
func cmdSysproxy(args []string) error {
	fs := flag.NewFlagSet("sysproxy", flag.ExitOnError)
	cfgFile := fs.String("c", config.DefaultPath(), "配置文件路径")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("用法: proxyd sysproxy on|off|status [-c 配置文件]")
	}
	cfg, err := config.Load(*cfgFile)
	if err != nil {
		return err
	}
	switch fs.Arg(0) {
	case "on":
		if err := sysproxy.On("127.0.0.1", cfg.MixedPort); err != nil {
			return err
		}
		fmt.Printf("系统代理已开启 → 127.0.0.1:%d\n", cfg.MixedPort)
	case "off":
		if err := sysproxy.Off(); err != nil {
			return err
		}
		fmt.Println("系统代理已关闭")
	case "status":
		on, err := sysproxy.Status("127.0.0.1", cfg.MixedPort)
		if err != nil {
			return err
		}
		if on {
			fmt.Printf("系统代理：开启（127.0.0.1:%d）\n", cfg.MixedPort)
		} else {
			fmt.Println("系统代理：关闭")
		}
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd sysproxy on|off|status", fs.Arg(0))
	}
	return nil
}

func cmdCheck(args []string) error {
	cfg, _, err := loadConfig(args, false)
	if err != nil {
		return err
	}
	var excludeRe *regexp.Regexp
	if cfg.Exclude != "" {
		if excludeRe, err = regexp.Compile(cfg.Exclude); err != nil {
			return err
		}
	}

	ctx := context.Background()
	nodes, errs := subscribe.FetchAll(ctx, cfg.Subscriptions, cfg.StateDir, excludeRe)
	for _, e := range errs {
		if e != nil {
			log.Printf("[subscribe] %v", e)
		}
	}
	if len(nodes) == 0 {
		return fmt.Errorf("no nodes parsed from subscriptions")
	}
	log.Printf("[check] %d nodes after merge, running health checks...", len(nodes))
	pool.Check(ctx, nodes, cfg.HealthURL, cfg.HealthTimeout.D(), 32)

	aliveNodes := make([]*node.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Alive {
			aliveNodes = append(aliveNodes, n)
		}
	}

	prev, _ := pool.LoadSnapshot(cfg.StateDir + "/mapping.json")
	assigns := pool.Allocate(aliveNodes, cfg.PortRange[0], cfg.PortRange[1], prev)

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PORT\tNODE\tSUBSCRIPTION\tDELAY")
	for _, as := range assigns {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%dms\n", as.Port, as.Node.Name, as.Node.Subscription, as.Node.Delay)
	}
	_ = tw.Flush()
	fmt.Printf("\n%d/%d nodes alive, %d ports mapped (range %d-%d), main mixed port: %d\n",
		len(aliveNodes), len(nodes), len(assigns), cfg.PortRange[0], cfg.PortRange[1], cfg.MixedPort)
	return nil
}
