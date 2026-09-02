package main

// 本地管理子命令：全部作为运行中实例的 HTTP API 客户端实现
// （读取配置拿 api-listen 地址；实例未运行时给出明确提示）。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"proxyd/internal/api"
	"proxyd/internal/app"
	"proxyd/internal/config"
	"proxyd/internal/subscribe"
)

// parseCFlag 解析通用的 -c 配置文件 flag（flag 需放在位置参数之前）。
// Go flag 解析遇位置参数即停止，写在后面的 -c 会被静默忽略，导致误操作默认配置
// 对应的实例（可能不是用户预期的那台）；检测到后位置 -c 时直接报错并提示正确写法。
func parseCFlag(name string, args []string) (string, []string, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfgFile := fs.String("c", config.DefaultPath(), "配置文件路径")
	_ = fs.Parse(args)
	rest := fs.Args()
	for i, a := range rest {
		if a == "-c" || a == "--c" || strings.HasPrefix(a, "-c=") || strings.HasPrefix(a, "--c=") {
			return "", nil, fmt.Errorf("-c 需放在子命令参数之前，例如: proxyd %s -c <配置> %s", name, strings.Join(rest[:i], " "))
		}
	}
	return *cfgFile, rest, nil
}

// apiClient 是 proxyd 自有 API 的简易客户端。
type apiClient struct {
	base string
}

func newAPIClient(cfgFile string) (*apiClient, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s 失败: %w", cfgFile, err)
	}
	return &apiClient{base: "http://" + cfg.APIListen}, nil
}

// do 发起一次 API 调用；连接失败提示先启动实例，HTTP 错误原样透传服务端报错。
func (c *apiClient) do(method, path string, body, out any) error {
	return c.doTimeout(method, path, body, out, 60*time.Second)
}

// doTimeout 与 do 相同，但允许调用方指定超时；
// 订阅刷新/测速、导入等同步接口最长可能执行 3 分钟，需要放宽默认 60s。
func (c *apiClient) doTimeout(method, path string, body, out any, timeout time.Duration) error {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(data)
	}
	resp, err := c.send(method, path, rdr, nil, timeout)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkAPIError(resp); err != nil {
		return err
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// raw 发起一次原始字节请求（配置导出/导入等非 JSON 接口），返回响应正文。
func (c *apiClient) raw(method, path string, body []byte, headers map[string]string, timeout time.Duration) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	resp, err := c.send(method, path, rdr, headers, timeout)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkAPIError(resp); err != nil {
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

// send 构造并执行一次请求；连接失败统一给出"先启动实例"的提示。
func (c *apiClient) send(method, path string, body io.Reader, headers map[string]string, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	hc := &http.Client{Timeout: timeout}
	if timeout <= 0 {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接 proxyd（%s）：实例未在运行？请先 proxyd start（或 proxyd serve）", c.base)
	}
	return resp, nil
}

// checkAPIError 把 >=400 的响应转成原样透传的服务端错误文本。
func checkAPIError(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	b, _ := io.ReadAll(resp.Body)
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("%s", msg)
}

func (c *apiClient) overview() (*api.Overview, error) {
	var ov api.Overview
	if err := c.do(http.MethodGet, "/api/overview", nil, &ov); err != nil {
		return nil, err
	}
	return &ov, nil
}

// getText 发起一次 GET 并返回原始文本（不做 JSON 解析）；错误处理与 do 一致。
func (c *apiClient) getText(path string) (string, error) {
	b, err := c.raw(http.MethodGet, path, nil, nil, 60*time.Second)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func cmdMode(args []string) error {
	cfgFile, rest, err := parseCFlag("mode", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		fmt.Printf("当前模式: %s（主端口 %d）\n", ov.Mode, ov.MixedPort)
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd mode [-c 配置] [rule|global|direct]")
	}
	if err := c.do(http.MethodPost, "/api/mode", map[string]string{"mode": rest[0]}, nil); err != nil {
		return err
	}
	fmt.Printf("已切换到 %s 模式\n", rest[0])
	return nil
}

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
		fmt.Fprintln(tw, "NAME\tSTATE\tURL\tNODES(alive/total)\tUSAGE\tEXPIRE")
		for _, s := range ov.Subs {
			usage, expire := formatSubUserInfo(s.UserInfo)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%s\t%s\n", s.Name, subStateText(s.State), s.URL, s.Alive, s.Total, usage, expire)
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

// cmdSubsSet 修改订阅的名称/地址/类型/启停；未给出的字段保持原值（客户端先读现状再合并提交）。
func cmdSubsSet(c *apiClient, args []string) error {
	setFS := flag.NewFlagSet("subs set", flag.ExitOnError)
	rename := setFS.String("rename", "", "新名称")
	newURL := setFS.String("url", "", "新订阅地址")
	newType := setFS.String("type", "", "订阅类型（auto|clash|share）")
	enable := setFS.Bool("enable", false, "启用订阅")
	disable := setFS.Bool("disable", false, "禁用订阅")
	_ = setFS.Parse(args)
	items := setFS.Args()
	if len(items) != 1 {
		return fmt.Errorf("用法: proxyd subs set [-c 配置] [--rename 新名] [--url 地址] [--type 类型] [--enable|--disable] <name>")
	}
	if *enable && *disable {
		return fmt.Errorf("--enable 与 --disable 不能同时使用")
	}
	if *rename == "" && *newURL == "" && *newType == "" && !*enable && !*disable {
		return fmt.Errorf("未给出任何修改项（--rename/--url/--type/--enable/--disable）")
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

// formatBytes 把字节数格式化为适合命令行阅读的二进制单位。
//
// 参数：
//   - n: int64，字节数。
//
// 返回值：
//   - string，格式化后的容量文本。
//
// 错误情况：
//   - 负数按 0B 展示，避免上游异常值污染输出。
func formatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}

func cmdNodes(args []string) error {
	cfgFile, rest, err := parseCFlag("nodes", args)
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
		cur := ""
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		for _, n := range ov.Nodes {
			if n.Subscription != cur {
				cur = n.Subscription
				fmt.Fprintf(tw, "[%s]\n", cur)
				fmt.Fprintln(tw, "PORT\tNAME\tTYPE\tDELAY\tSTATUS")
			}
			status := "可用"
			if !n.Alive {
				status = "失效 " + n.FailReason
			}
			port := "—"
			if n.Port != 0 {
				port = strconv.Itoa(n.Port)
			}
			delay := "—"
			if n.Alive {
				delay = fmt.Sprintf("%dms", n.Delay)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", port, n.Name, n.Type, delay, status)
		}
		_ = tw.Flush()
		if len(ov.Nodes) == 0 {
			fmt.Println("暂无节点（可用 proxyd nodes add 添加手动节点，或 proxyd refresh 刷新订阅）")
		}
		return nil
	case "add":
		if len(rest) < 2 || len(rest) > 3 {
			return fmt.Errorf("用法: proxyd nodes add [-c 配置] <url> [名称]")
		}
		body := map[string]string{"url": rest[1]}
		if len(rest) == 3 {
			body["name"] = rest[2]
		}
		var entry app.ManualNodeEntry
		if err := c.do(http.MethodPost, "/api/manual-nodes", body, &entry); err != nil {
			return err
		}
		fmt.Printf("手动节点已添加: %s（下标 %d，后台刷新中）\n", entry.Name, entry.Index)
		return nil
	case "del":
		if len(rest) != 2 {
			return fmt.Errorf("用法: proxyd nodes del [-c 配置] <名称|下标>")
		}
		var entries []app.ManualNodeEntry
		if err := c.do(http.MethodGet, "/api/manual-nodes", nil, &entries); err != nil {
			return err
		}
		idx, err := resolveManualIndex(entries, rest[1])
		if err != nil {
			return err
		}
		if err := c.do(http.MethodDelete, "/api/manual-nodes/"+strconv.Itoa(idx), nil, nil); err != nil {
			return err
		}
		fmt.Printf("手动节点 %q 已删除（后台刷新中）\n", rest[1])
		return nil
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd nodes [list]|add <url> [名称]|del <名称|下标>", sub)
	}
}

// resolveManualIndex 把 "名称或下标" 解析为手动节点下标。
func resolveManualIndex(entries []app.ManualNodeEntry, target string) (int, error) {
	if i, err := strconv.Atoi(target); err == nil {
		for _, e := range entries {
			if e.Index == i {
				return i, nil
			}
		}
		return 0, fmt.Errorf("手动节点下标 %d 不存在", i)
	}
	for _, e := range entries {
		if e.Name == target {
			return e.Index, nil
		}
	}
	return 0, fmt.Errorf("手动节点 %q 不存在", target)
}

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

// cmdLogs 查看运行中实例的内存日志尾部。
//
// 参数：
//   - args: []string，支持 `-c`、`--tail`、`--level`。
//
// 返回值：
//   - error，API 不可达或后端返回错误时返回。
//
// 错误情况：
//   - 实例未运行时 newAPIClient/do 会给出“请先 proxyd start/serve”的提示。
//   - level 未知不会本地拦截，交给后端过滤为空结果。
func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	cfgFile := fs.String("c", config.DefaultPath(), "配置文件路径")
	tail := fs.Int("tail", 200, "返回最近 N 条日志")
	level := fs.String("level", "", "按日志等级过滤（debug|info|warning|error）")
	_ = fs.Parse(args)
	c, err := newAPIClient(*cfgFile)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/logs?tail=%d", *tail)
	if *level != "" {
		path += "&level=" + url.QueryEscape(*level)
	}
	var out api.LogsResponse
	if err := c.do(http.MethodGet, path, nil, &out); err != nil {
		return err
	}
	for _, entry := range out.Entries {
		fmt.Println(entry.Line)
	}
	return nil
}

func cmdPortRange(args []string) error {
	cfgFile, rest, err := parseCFlag("port-range", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd port-range [-c 配置] <起-止>，如 42000-42100")
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/port-range", map[string]string{"range": rest[0]}, nil); err != nil {
		return err
	}
	fmt.Printf("端口区间已更新为 %s（已重新分配端口）\n", rest[0])
	return nil
}

// parseAutoPortArg 解析 auto-port 参数："off"/"0" 表示关闭，否则为端口号。
func parseAutoPortArg(s string) (int, error) {
	if strings.EqualFold(s, "off") {
		return 0, nil
	}
	p, err := strconv.Atoi(s)
	if err != nil || p < 0 || p > 65535 {
		return 0, fmt.Errorf("无效端口 %q（1-65535，或 off 关闭）", s)
	}
	return p, nil
}

func cmdAutoPort(args []string) error {
	cfgFile, rest, err := parseCFlag("auto-port", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		if ov.AutoPort == 0 {
			fmt.Println("自动选优端口: 关闭")
		} else {
			fmt.Printf("自动选优端口: %d\n", ov.AutoPort)
		}
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd auto-port [-c 配置] <端口|off>")
	}
	port, err := parseAutoPortArg(rest[0])
	if err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/auto-port", map[string]int{"port": port}, nil); err != nil {
		return err
	}
	if port == 0 {
		fmt.Println("自动选优端口已关闭")
	} else {
		fmt.Printf("自动选优端口已设置为 %d\n", port)
	}
	return nil
}

// parseOnOff 解析 on/off 开关参数。
func parseOnOff(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	return false, fmt.Errorf("无效参数 %q（on|off）", s)
}

// cmdTun 通过运行中实例的 API 查看或切换 TUN 模式。
//
// 参数：
//   - args: []string，支持 `-c <配置文件>` 和一个 `on|off|status` 位置参数。
//
// 返回值：
//   - error：参数无效、实例未运行、权限不足或热更新失败时返回错误。
//
// 错误情况：开启 TUN 需要运行中的 proxyd 进程具备平台权限；服务端返回的 sudo、
// setcap 或管理员指引会由 apiClient 原样传递到终端。
func cmdTun(args []string) error {
	cfgFile, rest, err := parseCFlag("tun", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd tun [-c 配置] on|off|status")
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}

	var status app.TUNStatus
	if strings.EqualFold(rest[0], "status") {
		if err := c.do(http.MethodGet, "/api/tun", nil, &status); err != nil {
			return err
		}
		state := "关闭"
		if status.Enabled && status.Active {
			state = "开启（已生效）"
		} else if status.Enabled {
			state = "配置已开启，但实际未生效（请检查日志）"
		}
		fmt.Printf("TUN：%s（平台 %s）\n", state, status.Platform)
		if !status.Allowed && status.Permission != "" {
			fmt.Printf("权限：不足\n指引：%s\n", status.Permission)
		} else {
			fmt.Println("权限：可用")
		}
		return nil
	}

	enabled, err := parseOnOff(rest[0])
	if err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/tun", map[string]bool{"enabled": enabled}, &status); err != nil {
		return err
	}
	if enabled {
		fmt.Printf("TUN 已开启并确认生效（%s，系统流量由 mihomo 接管）\n", status.Platform)
	} else {
		fmt.Println("TUN 已关闭（系统路由已由 mihomo 恢复）")
	}
	return nil
}

// cmdPortMapping 通过运行中实例的 API 查看或切换健康节点一对一端口映射。
//
// 参数：
//   - args: []string，支持 `-c <配置文件>` 和一个可选的 `on|off|status` 位置参数；
//     未提供位置参数时等价于 status。
//
// 返回值：
//   - error：参数无效、实例未运行、mihomo 热更新或配置持久化失败时返回。
//
// 错误情况：服务端事务回滚失败会作为组合错误原样输出，提醒用户检查配置与实际监听。
func cmdPortMapping(args []string) error {
	cfgFile, rest, err := parseCFlag("port-mapping", args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return fmt.Errorf("用法: proxyd port-mapping [-c 配置] [on|off|status]")
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 || strings.EqualFold(rest[0], "status") {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		state := "关闭"
		if ov.PortMappingEnabled {
			state = "开启"
		}
		fmt.Printf("节点端口映射: %s（稳定分配 %d 个，当前监听 %d 个）\n", state, len(ov.PortAssignments), len(ov.Ports))
		return nil
	}
	enabled, err := parseOnOff(rest[0])
	if err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/port-mapping", map[string]bool{"enabled": enabled}, nil); err != nil {
		return err
	}
	if enabled {
		fmt.Println("节点端口映射已开启，稳定分配的一对一监听已恢复")
	} else {
		fmt.Println("节点端口映射已关闭；主端口、自动端口、分组端口和稳定分配快照保持不变")
	}
	return nil
}

func cmdMainAuto(args []string) error {
	cfgFile, rest, err := parseCFlag("main-auto", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		if ov.MainAuto {
			fmt.Printf("主端口最优节点: 开启（主端口 %d 跳过规则、直连最优节点）\n", ov.MixedPort)
		} else {
			fmt.Printf("主端口最优节点: 关闭（主端口 %d 走规则模式）\n", ov.MixedPort)
		}
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd main-auto [-c 配置] [on|off]")
	}
	on, err := parseOnOff(rest[0])
	if err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/main-auto", map[string]bool{"enabled": on}, nil); err != nil {
		return err
	}
	if on {
		fmt.Println("主端口已切换为最优节点模式（跳过规则，与 auto-port 互不影响）")
	} else {
		fmt.Println("主端口已恢复规则模式")
	}
	return nil
}

// cmdMainNode 查看/设置主端口固定节点：无参显示当前设置；
// `off` 清除（恢复规则模式）；其余参数视为节点 key（overview 里每个节点的 key 字段）。
func cmdMainNode(args []string) error {
	cfgFile, rest, err := parseCFlag("main-node", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		if ov.MainNode == "" {
			fmt.Printf("主端口固定节点: 未设置（主端口 %d 走规则模式）\n", ov.MixedPort)
			return nil
		}
		name := ""
		for _, n := range ov.Nodes {
			if n.Key == ov.MainNode {
				name = n.Name
				break
			}
		}
		state := "生效中（主端口跳过规则、直达该节点）"
		switch {
		case ov.MainAuto:
			state = "main-auto 已开启，当前被忽略（auto 优先）"
		case !ov.MainNodeUp:
			state = "节点当前不可用，已回退规则模式（恢复后自动再生效）"
		}
		if name == "" {
			name = "（节点已不在列表中）"
		}
		fmt.Printf("主端口固定节点: %s\n  key: %s\n  状态: %s\n", name, ov.MainNode, state)
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd main-node [-c 配置] [节点名|节点key|off]")
	}
	key := rest[0]
	if strings.ToLower(key) == "off" {
		key = ""
	} else {
		// 允许直接给节点名称；key 精确匹配优先，名称需唯一
		ov, err := c.overview()
		if err != nil {
			return err
		}
		key, err = resolveNodeKey(ov, key)
		if err != nil {
			return err
		}
	}
	if err := c.do(http.MethodPost, "/api/main-node", map[string]string{"node": key}, nil); err != nil {
		return err
	}
	if key == "" {
		fmt.Println("主端口已恢复规则模式（main-node 已清除）")
	} else {
		fmt.Println("主端口已固定到指定节点（跳过规则）；main-auto 开启时该设置不生效")
	}
	return nil
}

func cmdMainPort(args []string) error {
	cfgFile, rest, err := parseCFlag("main-port", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		fmt.Printf("主端口: %d\n", ov.MixedPort)
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd main-port [-c 配置] <端口>")
	}
	port, err := strconv.Atoi(rest[0])
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("无效端口 %q（1-65535）", rest[0])
	}
	if err := c.do(http.MethodPost, "/api/main-port", map[string]int{"port": port}, nil); err != nil {
		return err
	}
	fmt.Printf("主端口已改为 %d（已热更新；系统代理开启时已自动重绑）\n", port)
	return nil
}

// resolveNodeKey 把用户输入的节点 key 或名称解析为节点 key。
// key 精确匹配优先；名称匹配要求唯一，重名时列出候选引导用户改用 key。
func resolveNodeKey(ov *api.Overview, target string) (string, error) {
	for _, n := range ov.Nodes {
		if n.Key == target {
			return n.Key, nil
		}
	}
	var matches []api.NodeEntry
	for _, n := range ov.Nodes {
		if n.Name == target {
			matches = append(matches, n)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("节点 %q 不存在（可用 proxyd nodes 查看节点列表）", target)
	case 1:
		return matches[0].Key, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "存在 %d 个同名节点 %q，请改用 key 指定：", len(matches), target)
	for _, n := range matches {
		fmt.Fprintf(&b, "\n  %s（订阅 %s，端口 %d）", n.Key, n.Subscription, n.Port)
	}
	return "", fmt.Errorf("%s", b.String())
}

// cmdDNSPreset 查看/切换 DNS 预设（off|fake-ip|redir-host）。
func cmdDNSPreset(args []string) error {
	cfgFile, rest, err := parseCFlag("dns-preset", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		fmt.Printf("DNS 预设: %s\n", ov.DNSPreset)
		if ov.DNSCustom {
			fmt.Println("注意：配置文件中存在手写 dns 段，预设暂不生效（删除该 dns 段后预设才会接管）")
		}
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd dns-preset [-c 配置] [off|fake-ip|redir-host]")
	}
	if err := c.do(http.MethodPost, "/api/dns-preset", map[string]string{"preset": rest[0]}, nil); err != nil {
		return err
	}
	fmt.Printf("DNS 预设已切换为 %s\n", rest[0])
	return nil
}

// cmdUpdateCheck 查看/开关启动时的版本检查。
func cmdUpdateCheck(args []string) error {
	cfgFile, rest, err := parseCFlag("update-check", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		v := ov.Version
		state := "关闭"
		if v.Enabled {
			state = "开启"
		}
		fmt.Printf("版本检查: %s（当前 %s）\n", state, v.Current)
		switch {
		case v.Latest != "" && v.Latest != v.Current:
			fmt.Printf("发现新版本: %s（%s）\n", v.Latest, v.URL)
		case v.Latest != "":
			fmt.Println("已是最新版本")
		case v.Message != "":
			fmt.Printf("检查状态: %s\n", v.Message)
		}
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd update-check [-c 配置] [on|off]")
	}
	on, err := parseOnOff(rest[0])
	if err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/update-check", map[string]bool{"enabled": on}, nil); err != nil {
		return err
	}
	if on {
		fmt.Println("版本检查已开启（已触发一次后台检查，稍后可用 proxyd update-check 查看结果）")
	} else {
		fmt.Println("版本检查已关闭")
	}
	return nil
}

// cmdConfig 配置文件的导出/导入与路径查看。
func cmdConfig(args []string) error {
	cfgFile, rest, err := parseCFlag("config", args)
	if err != nil {
		return err
	}
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "path":
		abs, err := filepath.Abs(cfgFile)
		if err != nil {
			abs = cfgFile
		}
		fmt.Println(abs)
		return nil
	case "export":
		expFS := flag.NewFlagSet("config export", flag.ExitOnError)
		full := expFS.Bool("full", false, "导出完整备份（包含订阅 token 等敏感信息）")
		out := expFS.String("o", "", "输出文件（默认打印到标准输出）")
		_ = expFS.Parse(rest[1:])
		c, err := newAPIClient(cfgFile)
		if err != nil {
			return err
		}
		path := "/api/config/export"
		if *full {
			path += "?mask_tokens=false"
		}
		body, err := c.raw(http.MethodGet, path, nil, nil, 60*time.Second)
		if err != nil {
			return err
		}
		if *out == "" {
			if *full {
				fmt.Fprintln(os.Stderr, "警告：以下为包含敏感凭据的完整备份，请勿外发")
			}
			_, _ = os.Stdout.Write(body)
			return nil
		}
		if err := os.WriteFile(*out, body, 0o600); err != nil {
			return err
		}
		if *full {
			fmt.Printf("完整配置备份（含敏感凭据）已写入 %s，请妥善保管\n", *out)
		} else {
			fmt.Printf("脱敏配置已导出到 %s（如需含凭据的完整备份，加 --full）\n", *out)
		}
		return nil
	case "import":
		impFS := flag.NewFlagSet("config import", flag.ExitOnError)
		yes := impFS.Bool("yes", false, "跳过确认直接导入")
		_ = impFS.Parse(rest[1:])
		items := impFS.Args()
		if len(items) != 1 {
			return fmt.Errorf("用法: proxyd config import [-c 配置] [--yes] <文件>")
		}
		return cmdConfigImport(nil, cfgFile, items[0], *yes)
	default:
		return fmt.Errorf("用法: proxyd config [-c 配置] path|export [--full] [-o 文件]|import [--yes] <文件>")
	}
}

// cmdConfigImport 预检导入配置、展示影响摘要，确认后执行导入。
func cmdConfigImport(c *apiClient, cfgFile, file string, yes bool) error {
	body, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", file, err)
	}
	if c == nil {
		if c, err = newAPIClient(cfgFile); err != nil {
			return err
		}
	}
	yamlHeader := map[string]string{"Content-Type": "application/yaml"}
	previewBody, err := c.raw(http.MethodPost, "/api/config/import/preview", body, yamlHeader, 60*time.Second)
	if err != nil {
		return err
	}
	var preview app.ConfigImportPreview
	if err := json.Unmarshal(previewBody, &preview); err != nil {
		return fmt.Errorf("解析预检结果失败: %w", err)
	}
	fmt.Println("导入预检（不会修改任何配置）：")
	labels := map[string]string{
		"subscriptions": "订阅",
		"manual_nodes":  "手动节点",
		"groups":        "分组",
		"custom_rules":  "自定义规则",
		"rule_urls":     "规则源",
	}
	for _, key := range []string{"subscriptions", "manual_nodes", "groups", "custom_rules", "rule_urls"} {
		ch, ok := preview.Counts[key]
		if !ok {
			continue
		}
		fmt.Printf("  %s: %d → %d\n", labels[key], ch.Before, ch.After)
	}
	for _, f := range preview.ChangedFields {
		fmt.Printf("  变更: %s\n", f)
	}
	for _, w := range preview.Warnings {
		fmt.Printf("  警告: %s\n", w)
	}
	if !yes {
		fmt.Fprint(os.Stderr, "确认导入并覆盖当前配置？[y/N] ")
		var answer string
		if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil ||
			!strings.EqualFold(strings.TrimSpace(answer), "y") {
			return fmt.Errorf("已取消导入")
		}
	}
	_, err = c.raw(http.MethodPost, "/api/config/import", body, map[string]string{
		"Content-Type":           "application/yaml",
		"X-Proxyd-Config-Digest": preview.Digest,
	}, 60*time.Second)
	if err != nil {
		return err
	}
	fmt.Println("配置已导入；需重启后生效：proxyd restart")
	return nil
}

// connEntry 是 mihomo /connections 返回的单条连接（只取 CLI 展示所需字段）。
type connEntry struct {
	ID       string   `json:"id"`
	Upload   int64    `json:"upload"`
	Download int64    `json:"download"`
	Start    string   `json:"start"`
	Chains   []string `json:"chains"`
	Rule     string   `json:"rule"`
	Metadata struct {
		Network         string `json:"network"`
		Type            string `json:"type"`
		SourceIP        string `json:"sourceIP"`
		SourcePort      string `json:"sourcePort"`
		DestinationIP   string `json:"destinationIP"`
		DestinationPort string `json:"destinationPort"`
		Host            string `json:"host"`
	} `json:"metadata"`
}

// connListResponse 是 /api/connections 的顶层响应（memory 由 proxyd 注入）。
type connListResponse struct {
	DownloadTotal int64       `json:"downloadTotal"`
	UploadTotal   int64       `json:"uploadTotal"`
	Memory        uint64      `json:"memory"`
	Connections   []connEntry `json:"connections"`
}

// cmdConn 查看/关闭运行中实例的活动连接。
func cmdConn(args []string) error {
	cfgFile, rest, err := parseCFlag("conn", args)
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
		body, err := c.raw(http.MethodGet, "/api/connections", nil, nil, 60*time.Second)
		if err != nil {
			return err
		}
		var out connListResponse
		if err := json.Unmarshal(body, &out); err != nil {
			return fmt.Errorf("解析连接列表失败: %w", err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tCHAIN\tRULE\tNETWORK\tDESTINATION\tUP\tDOWN\tAGE")
		for _, cn := range out.Connections {
			dest := cn.Metadata.Host
			if dest == "" {
				dest = cn.Metadata.DestinationIP
			}
			if cn.Metadata.DestinationPort != "" {
				dest += ":" + cn.Metadata.DestinationPort
			}
			chain := "-"
			if len(cn.Chains) > 0 {
				chain = cn.Chains[0]
			}
			rule := cn.Rule
			if rule == "" {
				rule = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				shortID(cn.ID), chain, rule, cn.Metadata.Network, dest,
				formatBytes(cn.Upload), formatBytes(cn.Download), connAge(cn.Start))
		}
		_ = tw.Flush()
		fmt.Printf("\n共 %d 条连接，累计 ↑%s ↓%s", len(out.Connections),
			formatBytes(out.UploadTotal), formatBytes(out.DownloadTotal))
		if out.Memory > 0 {
			fmt.Printf("，内存占用 %s", formatBytes(int64(out.Memory)))
		}
		fmt.Println()
		return nil
	case "close":
		if len(rest) != 2 {
			return fmt.Errorf("用法: proxyd conn close [-c 配置] <id|all>")
		}
		if strings.EqualFold(rest[1], "all") {
			if err := c.do(http.MethodDelete, "/api/connections", nil, nil); err != nil {
				return err
			}
			fmt.Println("已关闭全部连接")
			return nil
		}
		if err := c.do(http.MethodDelete, "/api/connections/"+url.PathEscape(rest[1]), nil, nil); err != nil {
			return err
		}
		fmt.Printf("连接 %s 已关闭\n", shortID(rest[1]))
		return nil
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd conn list|close <id|all>", sub)
	}
}

// shortID 把连接 UUID 截短展示；不足 8 位时原样返回。
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// connAge 把 mihomo 连接的开始时间格式化为存活时长。
func connAge(start string) string {
	t, err := time.Parse(time.RFC3339Nano, start)
	if err != nil {
		return "-"
	}
	d := time.Since(t).Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// cmdTraffic 实时显示主内核的上/下行速率（NDJSON 流，每秒一行，Ctrl-C 退出）。
func cmdTraffic(args []string) error {
	cfgFile, rest, err := parseCFlag("traffic", args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("用法: proxyd traffic [-c 配置]（Ctrl-C 退出）")
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	resp, err := c.send(http.MethodGet, "/api/traffic", nil, nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkAPIError(resp); err != nil {
		return err
	}
	fmt.Println("实时流量（Ctrl-C 退出）：")
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var t struct {
				Up   int64 `json:"up"`
				Down int64 `json:"down"`
			}
			if json.Unmarshal(line, &t) == nil {
				fmt.Printf("\r↑ %-12s ↓ %-12s", formatBytes(t.Up)+"/s", formatBytes(t.Down)+"/s")
			}
		}
		if err != nil {
			fmt.Println()
			if err == io.EOF {
				return fmt.Errorf("流量流已结束（实例可能已退出）")
			}
			return err
		}
	}
}
