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
	var err error
	switch {
	case len(os.Args) < 2:
		// 无参数默认 serve（沿用默认配置路径）
		err = cmdServe(nil)
	default:
		switch os.Args[1] {
		case "serve":
			err = cmdServe(os.Args[2:])
		case "start":
			err = cmdStart(os.Args[2:])
		case "stop":
			err = cmdStop(os.Args[2:])
		case "restart":
			err = cmdRestart(os.Args[2:])
		case "status":
			err = cmdStatus(os.Args[2:])
		case "check":
			err = cmdCheck(os.Args[2:])
		case "sysproxy":
			err = cmdSysproxy(os.Args[2:])
		case "autostart":
			err = cmdAutostart(os.Args[2:])
		case "mode":
			err = cmdMode(os.Args[2:])
		case "refresh":
			err = cmdRefresh(os.Args[2:])
		case "test":
			err = cmdTest(os.Args[2:])
		case "subs":
			err = cmdSubs(os.Args[2:])
		case "nodes":
			err = cmdNodes(os.Args[2:])
		case "rules":
			err = cmdRules(os.Args[2:])
		case "rule-urls":
			err = cmdRuleURLs(os.Args[2:])
		case "groups":
			err = cmdGroups(os.Args[2:])
		case "port-range":
			err = cmdPortRange(os.Args[2:])
		case "auto-port":
			err = cmdAutoPort(os.Args[2:])
		case "main-auto":
			err = cmdMainAuto(os.Args[2:])
		case "main-node":
			err = cmdMainNode(os.Args[2:])
		case "main-port":
			err = cmdMainPort(os.Args[2:])
		case "version", "-v", "--version":
			fmt.Printf("proxyd %s\n", version)
		case "-h", "--help", "help":
			usage()
		default:
			// 快捷形式：proxyd <订阅地址> 等价于 proxyd serve <订阅地址>
			if looksLikeURL(os.Args[1]) {
				err = cmdServe(os.Args[1:])
			} else {
				usage()
				os.Exit(2)
			}
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
  proxyd                                等价于 proxyd serve（无参数直接运行）
  proxyd serve [flags] [订阅地址...]    前台常驻运行（日志输出到终端）
  proxyd start|stop|restart|status [flags]   后台守护模式（日志落 state-dir/proxyd.log）
  proxyd check [flags] [订阅地址...]    一次性拉取订阅、测速并打印端口映射表
  proxyd sysproxy [-c 配置] on|off|status    开关/查看系统代理（指向主端口）
  proxyd autostart [-c 配置] on|off|status   开关/查看开机自启
  proxyd <订阅地址>                     serve 的快捷形式
  proxyd version                        打印版本

本地管理命令（操作运行中实例的 API，需先 proxyd start/serve）:
  proxyd mode [rule|global|direct]      查看/切换主端口代理模式
  proxyd refresh                        刷新订阅与规则源
  proxyd test                           手动测速
  proxyd subs list|add <名> <url>|del <名>          订阅管理
  proxyd nodes                          按订阅分组列出节点/端口/延迟
  proxyd nodes add <url> [名称]         添加手动节点（http/socks5/分享链接）
  proxyd nodes del <名称|下标>          删除手动节点
  proxyd rules list|add "<规则>"|del <下标>        自定义规则
  proxyd rule-urls list|add <名> <url>|del <名>|show <名>   远程规则源（show 查看原始内容）
  proxyd groups list|add <名> <端口> <节点...>|del <名>   节点分组
  proxyd port-range <起-止>             修改节点映射端口区间
  proxyd auto-port <端口|off>           设置自动选优端口
  proxyd main-auto [on|off]             主端口固定走最优节点（跳过规则）；无参查看
  proxyd main-node [节点key|off]        主端口固定走指定节点（跳过规则）；无参查看
  proxyd main-port <端口>               修改主端口；无参查看

flags:
  -c <文件>      配置文件路径（默认 ~/.config/proxyd/config.yaml；命令行给的订阅地址会自动保存进去）
  -range <区间>  节点映射端口区间，如 42000-42100（默认 %s）
  注意：flag 需放在位置参数之前（Go flag 解析遇位置参数即停止）。

examples:
  proxyd serve https://example.com/sub?token=xxx
  proxyd serve -range 43000-43100 https://a.com/sub https://b.com/link
  proxyd start -c proxyd.yaml
  proxyd nodes add socks5://user:pass@1.2.3.4:1080 我的节点
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
	if pid, alive := readPIDFile(pidPath(cfg)); alive {
		return fmt.Errorf("proxyd 已在运行 (pid %d)，请先 proxyd stop", pid)
	}
	a, err := app.New(cfg, cfgPath)
	if err != nil {
		return err
	}

	apiSrv := api.New(cfg.APIListen, a)
	if err := apiSrv.Start(); err != nil {
		return fmt.Errorf("start api on %s: %w", cfg.APIListen, err)
	}
	// 登记 pid 文件（供 stop/status/防重复启动），退出时清理
	if err := writePIDFile(pidPath(cfg), os.Getpid()); err != nil {
		log.Printf("[proxyd] 写 pid 文件失败（不影响运行）: %v", err)
	} else {
		defer func() { _ = os.Remove(pidPath(cfg)) }()
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
		return fmt.Errorf("用法: proxyd sysproxy [-c 配置文件] on|off|status")
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
