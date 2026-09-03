package main

// 代理域子命令：订阅管理（subs）与全局刷新/测速（refresh/test）。

import (
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"
	"time"

	"proxyd/internal/api"
	"proxyd/internal/proxy/subscribe"
)

func cmdRefresh(args []string) error {
	cfgFile, _, err := parseCFlag("refresh", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/refresh", nil, nil); err != nil {
		return err
	}
	fmt.Println("已触发订阅刷新（后台执行，耗时取决于节点数量，可用 proxyd nodes 查看结果）")
	return nil
}

func cmdTest(args []string) error {
	cfgFile, _, err := parseCFlag("test", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/test", nil, nil); err != nil {
		return err
	}
	fmt.Println("已触发手动测速（后台执行，可用 proxyd nodes 查看结果）")
	return nil
}

func cmdSubs(args []string) error {
	cfgFile, rest, err := parseCFlag("subs", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	sub := "list"
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "list":
		ov, err := c.overview()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSTATE\tURL\tNODES(alive/total)\tMAPPING\tUSAGE\tEXPIRE")
		for _, s := range ov.Subs {
			usage, expire := formatSubUserInfo(s.UserInfo)
			mapping := "on"
			if !s.PortMapping {
				mapping = "off"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%s\t%s\t%s\n", s.Name, subStateText(s.State), s.URL, s.Alive, s.Total, mapping, usage, expire)
		}
		_ = tw.Flush()
		return nil
	case "add":
		if len(rest) != 3 {
			return fmt.Errorf("用法: proxyd subs add [-c 配置] <name> <url>")
		}
		if err := c.do(http.MethodPost, "/api/subscriptions",
			map[string]string{"name": rest[1], "url": rest[2]}, nil); err != nil {
			return err
		}
		fmt.Printf("订阅 %q 已添加（后台刷新中）\n", rest[1])
		return nil
	case "set":
		return cmdSubsSet(c, rest[1:])
	case "refresh", "test":
		if len(rest) != 2 {
			return fmt.Errorf("用法: proxyd subs %s [-c 配置] <name>", sub)
		}
		// 单订阅刷新/测速为同步接口（服务端最长 3 分钟），放宽客户端超时
		if err := c.doTimeout(http.MethodPost, "/api/subscriptions/"+url.PathEscape(rest[1])+"/"+sub,
			nil, nil, 200*time.Second); err != nil {
			return err
		}
		if sub == "refresh" {
			fmt.Printf("订阅 %q 已刷新（节点列表与端口映射已热更新）\n", rest[1])
		} else {
			fmt.Printf("订阅 %q 测速完成（可用 proxyd nodes 查看延迟）\n", rest[1])
		}
		return nil
	case "del":
		if len(rest) != 2 {
			return fmt.Errorf("用法: proxyd subs del [-c 配置] <name>")
		}
		if err := c.do(http.MethodDelete, "/api/subscriptions/"+url.PathEscape(rest[1]), nil, nil); err != nil {
			return err
		}
		fmt.Printf("订阅 %q 已删除（后台刷新中）\n", rest[1])
		return nil
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd subs list|add <name> <url>|set [flags] <name>|refresh <name>|test <name>|del <name>", sub)
	}
}

// subStateText 把订阅聚合状态翻译成 CLI 展示文本。
func subStateText(state string) string {
	switch state {
	case "disabled":
		return "已禁用"
	case "empty":
		return "无节点"
	case "error":
		return "全部失效"
	case "degraded":
		return "部分可用"
	case "healthy":
		return "正常"
	default:
		return state
	}
}

// cmdSubsSet 修改订阅的名称/地址/类型/启停/端口映射；未给出的字段保持原值（客户端先读现状再合并提交）。
func cmdSubsSet(c *apiClient, args []string) error {
	setFS := flag.NewFlagSet("subs set", flag.ExitOnError)
	rename := setFS.String("rename", "", "新名称")
	newURL := setFS.String("url", "", "新订阅地址")
	newType := setFS.String("type", "", "订阅类型（auto|clash|share）")
	enable := setFS.Bool("enable", false, "启用订阅")
	disable := setFS.Bool("disable", false, "禁用订阅")
	mapping := setFS.String("mapping", "", "订阅级端口映射开关（on|off）")
	_ = setFS.Parse(args)
	items := setFS.Args()
	if len(items) != 1 {
		return fmt.Errorf("用法: proxyd subs set [-c 配置] [--rename 新名] [--url 地址] [--type 类型] [--enable|--disable] [--mapping on|off] <name>")
	}
	if *enable && *disable {
		return fmt.Errorf("--enable 与 --disable 不能同时使用")
	}
	if *mapping != "" && *mapping != "on" && *mapping != "off" {
		return fmt.Errorf("--mapping 只接受 on 或 off")
	}
	if *rename == "" && *newURL == "" && *newType == "" && !*enable && !*disable && *mapping == "" {
		return fmt.Errorf("未给出任何修改项（--rename/--url/--type/--enable/--disable/--mapping）")
	}
	ov, err := c.overview()
	if err != nil {
		return err
	}
	var cur *api.SubEntry
	for i := range ov.Subs {
		if ov.Subs[i].Name == items[0] {
			cur = &ov.Subs[i]
			break
		}
	}
	if cur == nil {
		return fmt.Errorf("订阅 %q 不存在", items[0])
	}
	body := map[string]any{
		"name": cur.Name,
		"url":  cur.URL,
		"type": cur.Type,
	}
	if *rename != "" {
		body["name"] = *rename
	}
	if *newURL != "" {
		body["url"] = *newURL
	}
	if *newType != "" {
		body["type"] = *newType
	}
	if *enable || *disable {
		body["enabled"] = *enable
	}
	if *mapping != "" {
		body["port_mapping"] = *mapping == "on"
	}
	// 修改启用中的订阅会同步重新拉取（服务端最长 3 分钟），放宽客户端超时
	if err := c.doTimeout(http.MethodPut, "/api/subscriptions/"+url.PathEscape(items[0]), body, nil, 200*time.Second); err != nil {
		return err
	}
	fmt.Printf("订阅 %q 已更新\n", items[0])
	return nil
}

// formatSubUserInfo 把订阅用量信息格式化为 CLI 列表字段。
//
// 参数：
//   - info: *subscribe.UserInfo，API overview 返回的订阅用量；nil 表示暂无数据。
//
// 返回值：
//   - usage: string，`已用/总量` 或 `-`。
//   - expire: string，到期日期（本地时区 YYYY-MM-DD）或 `-`。
//
// 错误情况：
//   - expire 为 0 或非法时返回 `-`；字节数缺失时尽量展示已知字段。
func formatSubUserInfo(info *subscribe.UserInfo) (usage, expire string) {
	if info == nil {
		return "-", "-"
	}
	used := info.Used()
	switch {
	case info.Total > 0:
		usage = fmt.Sprintf("%s/%s", formatBytes(used), formatBytes(info.Total))
	case used > 0:
		usage = formatBytes(used)
	default:
		usage = "-"
	}
	if info.Expire > 0 {
		expire = time.Unix(info.Expire, 0).Format("2006-01-02")
	} else {
		expire = "-"
	}
	return usage, expire
}
