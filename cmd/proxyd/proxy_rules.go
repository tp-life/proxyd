package main

// 代理域子命令：自定义规则（rules）与远程规则源（ruleurls）。

import (
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
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
	case "effective":
		return runEffectiveRules(c, rest[1:])
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd rules list|effective|add \"<规则>\"|set <下标> \"<规则>\"|move <从> <到>|del <下标>", sub)
	}
}

// effectiveRulesPageLimit 是查看生效规则时每页请求的条数（服务端上限 1000）。
const effectiveRulesPageLimit = 500

// runEffectiveRules 打印当前生效配置里的完整规则列表（含来源标注）。
//
// 参数：
//   - c: *apiClient，已就绪的本地 API 客户端。
//   - args: []string，`rules effective` 之后的参数：--source、--grep、--limit。
//
// 返回值：error，参数非法或接口失败时返回。
//
// 错误情况：结果可能上万条（订阅自带规则），因此按页循环拉取直到取完；每页请求失败
// 立即返回错误，不打印半份列表之外的任何推测内容。来源名写错由服务端返回 400 并列出
// 可选值，这里直接透传该错误。
func runEffectiveRules(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("rules effective", flag.ContinueOnError)
	source := fs.String("source", "", "按来源筛选（键或中文名，如 subscription / 订阅规则）")
	grep := fs.String("grep", "", "按规则文本子串筛选（大小写不敏感）")
	limit := fs.Int("limit", effectiveRulesPageLimit, "每页拉取条数（服务端上限 1000）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limit <= 0 {
		return fmt.Errorf("--limit 必须为正整数")
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("用法: proxyd rules effective [-c 配置] [--source 来源] [--grep 关键字] [--limit N]")
	}
	printed := 0
	offset := 0
	for {
		path := fmt.Sprintf("/api/rules/effective?offset=%d&limit=%d", offset, *limit)
		if *source != "" {
			path += "&source=" + url.QueryEscape(*source)
		}
		if *grep != "" {
			path += "&q=" + url.QueryEscape(*grep)
		}
		var status app.EffectiveRulesStatus
		if err := c.do(http.MethodGet, path, nil, &status); err != nil {
			return err
		}
		if offset == 0 {
			if !status.Available {
				fmt.Println("当前没有已生效的配置（代理可能未启动或已停用）")
				return nil
			}
			fmt.Printf("生效规则 %d 条", status.Total)
			if status.Matched != status.Total {
				fmt.Printf("，筛选后 %d 条", status.Matched)
			}
			fmt.Println()
			if sourceCounts := formatEffectiveRuleSources(status.Sources); sourceCounts != "" {
				fmt.Printf("来源分布: %s\n", sourceCounts)
			}
			if status.Matched == 0 {
				fmt.Println("没有匹配的规则")
				return nil
			}
		}
		writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, rule := range status.Rules {
			fmt.Fprintf(writer, "%d\t%s\t%s\n", rule.Position, rule.SourceLabel, rule.Line)
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		printed += len(status.Rules)
		offset += len(status.Rules)
		if len(status.Rules) == 0 || offset >= status.Matched {
			break
		}
	}
	fmt.Printf("共列出 %d 条\n", printed)
	return nil
}

// formatEffectiveRuleSources 把来源统计拼成一行「中文名 条数」摘要。
//
// 参数：sources 为服务端返回的来源统计（含 0 条来源）。
// 返回值：string，形如「自定义规则 2 · 订阅规则 9475」；全部为 0 时返回空串。
// 错误情况：无。
func formatEffectiveRuleSources(sources []app.EffectiveRuleSource) string {
	parts := make([]string, 0, len(sources))
	for _, source := range sources {
		if source.Count == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %d", source.Label, source.Count))
	}
	return strings.Join(parts, " · ")
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
