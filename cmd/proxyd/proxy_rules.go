package main

// 代理域子命令：自定义规则（rules）与远程规则源（ruleurls）。

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"text/tabwriter"

	"proxyd/internal/app"
	"proxyd/internal/config"
)

func cmdRules(args []string) error {
	cfgFile, rest, err := parseCFlag("rules", args)
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
		var rules []string
		if err := c.do(http.MethodGet, "/api/rules", nil, &rules); err != nil {
			return err
		}
		for i, r := range rules {
			fmt.Printf("%d\t%s\n", i, r)
		}
		if len(rules) == 0 {
			fmt.Println("暂无自定义规则")
		}
		return nil
	case "add":
		if len(rest) != 2 {
			return fmt.Errorf("用法: proxyd rules add [-c 配置] \"DOMAIN-SUFFIX,example.com,DIRECT\"")
		}
		if err := c.do(http.MethodPost, "/api/rules", map[string]string{"rule": rest[1]}, nil); err != nil {
			return err
		}
		fmt.Println("规则已添加")
		return nil
	case "set":
		if len(rest) != 3 {
			return fmt.Errorf("用法: proxyd rules set [-c 配置] <下标> \"<规则>\"")
		}
		if _, err := strconv.Atoi(rest[1]); err != nil {
			return fmt.Errorf("下标必须是整数: %q", rest[1])
		}
		if err := c.do(http.MethodPut, "/api/rules/"+rest[1], map[string]string{"rule": rest[2]}, nil); err != nil {
			return err
		}
		fmt.Printf("规则 %s 已更新\n", rest[1])
		return nil
	case "move":
		if len(rest) != 3 {
			return fmt.Errorf("用法: proxyd rules move [-c 配置] <从> <到>")
		}
		from, err1 := strconv.Atoi(rest[1])
		to, err2 := strconv.Atoi(rest[2])
		if err1 != nil || err2 != nil {
			return fmt.Errorf("下标必须是整数: %q %q", rest[1], rest[2])
		}
		if err := c.do(http.MethodPost, "/api/rules/reorder", map[string]int{"from": from, "to": to}, nil); err != nil {
			return err
		}
		fmt.Printf("规则已从 %d 移动到 %d\n", from, to)
		return nil
	case "del":
		if len(rest) != 2 {
			return fmt.Errorf("用法: proxyd rules del [-c 配置] <下标>")
		}
		if _, err := strconv.Atoi(rest[1]); err != nil {
			return fmt.Errorf("下标必须是整数: %q", rest[1])
		}
		if err := c.do(http.MethodDelete, "/api/rules/"+rest[1], nil, nil); err != nil {
			return err
		}
		fmt.Println("规则已删除")
		return nil
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd rules list|add \"<规则>\"|set <下标> \"<规则>\"|move <从> <到>|del <下标>", sub)
	}
}

func cmdRuleURLs(args []string) error {
	cfgFile, rest, err := parseCFlag("rule-urls", args)
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
		var stats []app.RuleURLStat
		if err := c.do(http.MethodGet, "/api/rule-urls", nil, &stats); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tURL\tCOUNT\tSTATUS")
		for _, s := range stats {
			status := fmt.Sprintf("%d 条", s.Count)
			if s.Error != "" {
				status = "拉取失败: " + s.Error
			} else if s.Warn != "" {
				status += "（缓存）"
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", s.Name, s.URL, s.Count, status)
		}
		_ = tw.Flush()
		if len(stats) == 0 {
			fmt.Println("暂无规则 URL")
		}
		return nil
	case "add":
		if len(rest) != 3 {
			return fmt.Errorf("用法: proxyd rule-urls add [-c 配置] <name> <url>")
		}
		if err := c.do(http.MethodPost, "/api/rule-urls",
			config.RuleURL{Name: rest[1], URL: rest[2]}, nil); err != nil {
			return err
		}
		fmt.Printf("规则源 %q 已添加\n", rest[1])
		return nil
	case "del":
		if len(rest) != 2 {
			return fmt.Errorf("用法: proxyd rule-urls del [-c 配置] <name>")
		}
		if err := c.do(http.MethodDelete, "/api/rule-urls/"+rest[1], nil, nil); err != nil {
			return err
		}
		fmt.Printf("规则源 %q 已删除\n", rest[1])
		return nil
	case "show":
		if len(rest) != 2 {
			return fmt.Errorf("用法: proxyd rule-urls show [-c 配置] <name>")
		}
		content, err := c.getText("/api/rule-urls/" + url.PathEscape(rest[1]) + "/content")
		if err != nil {
			return err
		}
		fmt.Print(content)
		return nil
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd rule-urls list|add <name> <url>|del <name>|show <name>", sub)
	}
}
