package main

// 代理域子命令：节点分组（groups）。

import (
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"proxyd/internal/config"
)

func cmdGroups(args []string) error {
	cfgFile, rest, err := parseCFlag("groups", args)
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
		var groups []config.NodeGroup
		if err := c.do(http.MethodGet, "/api/groups", nil, &groups); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tPORT\tTYPE\tSUBSCRIPTION\tNODES")
		for _, g := range groups {
			subscription := g.Subscription
			if subscription == "" {
				subscription = "-"
			}
			nodes := strings.Join(g.Nodes, ",")
			if nodes == "" {
				nodes = "-"
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", g.Name, g.Port, g.Type, subscription, nodes)
		}
		_ = tw.Flush()
		if len(groups) == 0 {
			fmt.Println("暂无节点分组")
		}
		return nil
	case "add":
		addFS := flag.NewFlagSet("groups add", flag.ExitOnError)
		groupType := addFS.String("type", config.GroupTypeFallback, "分组类型（url-test|fallback|load-balance）")
		subscription := addFS.String("subscription", "", "按订阅名动态生成分组成员")
		_ = addFS.Parse(rest[1:])
		items := addFS.Args()
		if len(items) < 2 || (*subscription == "" && len(items) < 3) {
			return fmt.Errorf("用法: proxyd groups add [-c 配置] [--type fallback|url-test|load-balance] [--subscription 订阅名] <name> <port> [节点名...]")
		}
		port, err := strconv.Atoi(items[1])
		if err != nil {
			return fmt.Errorf("端口必须是整数: %q", items[1])
		}
		if err := c.do(http.MethodPost, "/api/groups",
			config.NodeGroup{Name: items[0], Port: port, Type: *groupType, Subscription: *subscription, Nodes: items[2:]}, nil); err != nil {
			return err
		}
		fmt.Printf("分组 %q 已添加（端口 %d）\n", items[0], port)
		return nil
	case "set":
		setFS := flag.NewFlagSet("groups set", flag.ExitOnError)
		groupType := setFS.String("type", "", "分组类型（url-test|fallback|load-balance）")
		subscription := setFS.String("subscription", "", "按订阅名动态生成分组成员")
		port := setFS.Int("port", 0, "分组端口")
		_ = setFS.Parse(rest[1:])
		items := setFS.Args()
		if len(items) < 1 {
			return fmt.Errorf("用法: proxyd groups set [-c 配置] [--type 类型] [--subscription 订阅名] [--port 端口] <name> [节点名...]")
		}
		var groups []config.NodeGroup
		if err := c.do(http.MethodGet, "/api/groups", nil, &groups); err != nil {
			return err
		}
		var cur *config.NodeGroup
		for i := range groups {
			if groups[i].Name == items[0] {
				cur = &groups[i]
				break
			}
		}
		if cur == nil {
			return fmt.Errorf("分组 %q 不存在", items[0])
		}
		// 未给出的字段保持原值；位置参数给出节点名时整体替换成员列表
		next := *cur
		if *groupType != "" {
			next.Type = *groupType
		}
		if *subscription != "" {
			next.Subscription = *subscription
		}
		if *port > 0 {
			next.Port = *port
		}
		if len(items) > 1 {
			next.Nodes = items[1:]
		}
		if err := c.do(http.MethodPut, "/api/groups/"+url.PathEscape(items[0]), next, nil); err != nil {
			return err
		}
		fmt.Printf("分组 %q 已更新（端口 %d）\n", items[0], next.Port)
		return nil
	case "del":
		if len(rest) != 2 {
			return fmt.Errorf("用法: proxyd groups del [-c 配置] <name>")
		}
		if err := c.do(http.MethodDelete, "/api/groups/"+url.PathEscape(rest[1]), nil, nil); err != nil {
			return err
		}
		fmt.Printf("分组 %q 已删除\n", rest[1])
		return nil
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd groups list|add [--type 类型] [--subscription 订阅名] <name> <port> [节点...]|set [--type 类型] [--subscription 订阅名] [--port 端口] <name> [节点...]|del <name>", sub)
	}
}
