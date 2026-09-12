package main

// 「LAN 网关」子命令：全部作为运行中实例的 HTTP API 客户端实现，
// 保持与 Web 相同的事务与错误语义。模块启停走 proxyd modules gateway on|off。

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"

	"proxyd/internal/app"
	"proxyd/internal/config"
	"proxyd/internal/gateway"
)

// cmdGateway 是 gateway 子命令入口：status|precheck|devices|helper。
// 参数：args 为 []string，支持 -c；返回 error，参数或 API 错误原样上报。
// helper 子命令是本地特权操作（安装/卸载 root helper），不经运行中实例的 API。
func cmdGateway(args []string) error {
	cfgFile, rest, err := parseCFlag("gateway", args)
	if err != nil {
		return err
	}
	sub := "status"
	if len(rest) > 0 {
		sub = rest[0]
	}
	if sub == "helper" {
		return cmdGatewayHelperLocal(rest[1:])
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	switch sub {
	case "status":
		return gatewayStatus(c)
	case "precheck":
		return gatewayPrecheck(c)
	case "devices":
		return gatewayDevices(c, rest[1:])
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd gateway [-c 配置] status|precheck|devices list|add <名> <ip> [策略]|set <名> [--ip 地址] [--mac 地址] [--policy 策略]|del <名>|helper install|uninstall|status", sub)
	}
}

// cmdGatewayHelperLocal 处理 helper 安装管理子命令（install|uninstall|status）。
// 安装/卸载是本地特权操作：需要 root（sudo）或经管理员授权弹窗，不经 HTTP API。
//
// 参数：args 为 []string，helper 之后的子命令参数；返回 error。
//
// 错误情况：非 macOS 平台返回不支持错误；授权拒绝或 launchctl 失败原样上报。
func cmdGatewayHelperLocal(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("用法: proxyd gateway helper install|uninstall|status（macOS 特权 helper 的本地安装管理）")
	}
	switch args[0] {
	case "install":
		if err := gateway.HelperInstall(); err != nil {
			return err
		}
		fmt.Println("gateway helper 已安装并启动（launchd 系统域常驻，root 运行）")
		return nil
	case "uninstall":
		if err := gateway.HelperUninstall(); err != nil {
			return err
		}
		fmt.Println("gateway helper 已卸载（pf 规则已清除，IPv4 转发恢复原值）")
		return nil
	case "status":
		status := gateway.HelperInstalledStatus()
		fmt.Printf("已安装: %s，已加载: %s，握手可达: %s\n",
			onOffText(status.Installed), onOffText(status.Running), onOffText(status.Reachable))
		if status.Detail != "" {
			fmt.Printf("说明: %s\n", status.Detail)
		}
		if !status.Installed || !status.Running || !status.Reachable {
			return fmt.Errorf("helper 未就绪")
		}
		return nil
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd gateway helper install|uninstall|status", args[0])
	}
}

// cmdGatewayHelperServe 是 helper 服务端入口（内部子命令 `proxyd gateway-helper`，
// 由 launchd 以 root 托管运行，不应手工调用）。
//
// 参数：args 为 []string（忽略，无参数）；返回 error。
//
// 错误情况：非 root、非 macOS 或监听失败时返回错误，launchd 按 KeepAlive 重启。
func cmdGatewayHelperServe(_ []string) error {
	return gateway.RunHelperServer(context.Background(), func(format string, args ...any) {
		log.Printf(format, args...)
	})
}

// gatewayStatus 打印网关模块状态：相位、平台、转发与规则应用状态、生效端口、设备表。
func gatewayStatus(c *apiClient) error {
	var overview app.GatewayOverview
	if err := c.do(http.MethodGet, "/api/gateway", nil, &overview); err != nil {
		return err
	}
	fmt.Printf("相位: %s\n", overview.Phase)
	if overview.Error != "" {
		fmt.Printf("错误: %s\n", overview.Error)
	}
	fmt.Printf("平台: %s（支持: %s）\n", overview.Platform, onOffText(overview.Supported))
	fmt.Printf("IPv4 转发: %s，转发规则: %s\n", onOffText(overview.Forwarding), appliedText(overview.Applied))
	fmt.Printf("入口端口: redir=%d tproxy=%d（仅 Linux） dns=%d（劫持 53: %s）\n",
		overview.RedirPort, overview.TProxyPort, overview.DNSListenPort, onOffText(overview.DNSRedirect))
	if overview.Runner != "" {
		fmt.Printf("执行层: %s\n", overview.Runner)
	}
	if overview.Err != "" {
		fmt.Printf("执行层错误: %s\n", overview.Err)
	}
	if overview.NextRetryAt != nil {
		fmt.Printf("下次重试: %s\n", overview.NextRetryAt.Local().Format("15:04:05"))
	}
	printGatewayDevices(overview.Devices)
	return nil
}

// appliedText 把规则应用状态格式化为中文开关文本。
func appliedText(applied bool) string {
	if applied {
		return "已应用"
	}
	return "未应用"
}

// gatewayPrecheck 打印「启用前检查」结果（平台/权限/helper 状态与修复指引）。
func gatewayPrecheck(c *apiClient) error {
	var result gateway.PrecheckResult
	if err := c.do(http.MethodGet, "/api/gateway/precheck", nil, &result); err != nil {
		return err
	}
	fmt.Printf("平台: %s（支持: %s）\n", result.Platform, onOffText(result.Supported))
	fmt.Printf("特权前置: %s\n", onOffText(result.Ready))
	if result.Detail != "" {
		fmt.Printf("说明: %s\n", result.Detail)
	}
	if !result.Supported {
		return fmt.Errorf("当前平台不支持 LAN 网关")
	}
	if !result.Ready {
		return fmt.Errorf("前置条件未就绪，请按上方说明修复")
	}
	return nil
}

// gatewayDevices 处理 devices 子命令：list|add|set|del。
func gatewayDevices(c *apiClient, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		var overview app.GatewayOverview
		if err := c.do(http.MethodGet, "/api/gateway", nil, &overview); err != nil {
			return err
		}
		printGatewayDevices(overview.Devices)
		return nil
	case "add":
		if len(args) < 3 || len(args) > 4 {
			return fmt.Errorf("用法: proxyd gateway devices add [-c 配置] <名> <ip> [策略: direct|proxy|group:<分组名>，默认 proxy]")
		}
		device := map[string]string{"name": args[1], "ip": args[2]}
		if len(args) == 4 {
			device["policy"] = args[3]
		}
		if err := c.do(http.MethodPost, "/api/gateway/devices", device, nil); err != nil {
			return err
		}
		fmt.Printf("设备 %q 已登记（%s）\n", args[1], args[2])
		return nil
	case "set":
		setFS := flag.NewFlagSet("gateway devices set", flag.ExitOnError)
		ip := setFS.String("ip", "", "设备 IPv4 地址")
		mac := setFS.String("mac", "", "设备 MAC（仅识别/展示）")
		policy := setFS.String("policy", "", "出口策略: direct|proxy|group:<分组名>")
		_ = setFS.Parse(args[1:])
		items := setFS.Args()
		if len(items) != 1 {
			return fmt.Errorf("用法: proxyd gateway devices set [-c 配置] [--ip 地址] [--mac 地址] [--policy 策略] <名>")
		}
		// 未给出的字段保持原值：先取当前设备表再合并。
		var overview app.GatewayOverview
		if err := c.do(http.MethodGet, "/api/gateway", nil, &overview); err != nil {
			return err
		}
		var current *config.GatewayDevice
		for i := range overview.Devices {
			if overview.Devices[i].Name == items[0] {
				current = &overview.Devices[i]
				break
			}
		}
		if current == nil {
			return fmt.Errorf("设备 %q 不存在", items[0])
		}
		if *ip != "" {
			current.IP = *ip
		}
		if *mac != "" {
			current.MAC = *mac
		}
		if *policy != "" {
			current.Policy = *policy
		}
		if err := c.do(http.MethodPut, "/api/gateway/devices/"+url.PathEscape(items[0]), current, nil); err != nil {
			return err
		}
		fmt.Printf("设备 %q 已更新（%s → %s）\n", items[0], current.IP, current.Policy)
		return nil
	case "del":
		if len(args) != 2 {
			return fmt.Errorf("用法: proxyd gateway devices del [-c 配置] <名>")
		}
		if err := c.do(http.MethodDelete, "/api/gateway/devices/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return err
		}
		fmt.Printf("设备 %q 已删除\n", args[1])
		return nil
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd gateway devices list|add <名> <ip> [策略]|set <名> [--ip 地址] [--mac 地址] [--policy 策略]|del <名>", sub)
	}
}

// printGatewayDevices 以表格打印设备登记表。
func printGatewayDevices(devices []config.GatewayDevice) {
	if len(devices) == 0 {
		fmt.Println("设备表为空（登记设备后网关才开始分流）")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tIP\tMAC\tPOLICY")
	for _, d := range devices {
		mac := d.MAC
		if mac == "" {
			mac = "-"
		}
		policy := d.Policy
		if policy == "" {
			policy = "proxy"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Name, d.IP, mac, policy)
	}
	_ = tw.Flush()
}
