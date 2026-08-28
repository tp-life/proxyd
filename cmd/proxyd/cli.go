package main

// 本地管理子命令：全部作为运行中实例的 HTTP API 客户端实现
// （读取配置拿 api-listen 地址；实例未运行时给出明确提示）。

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"proxyd/internal/api"
	"proxyd/internal/app"
	"proxyd/internal/config"
)

// parseCFlag 解析通用的 -c 配置文件 flag（flag 需放在位置参数之前）。
func parseCFlag(name string, args []string) (string, []string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfgFile := fs.String("c", config.DefaultPath(), "配置文件路径")
	_ = fs.Parse(args)
	return *cfgFile, fs.Args()
}

// apiClient 是 proxyd 自有 API 的简易客户端。
type apiClient struct {
	base string
	hc   *http.Client
}

func newAPIClient(cfgFile string) (*apiClient, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s 失败: %w", cfgFile, err)
	}
	return &apiClient{base: "http://" + cfg.APIListen, hc: &http.Client{Timeout: 60 * time.Second}}, nil
}

// do 发起一次 API 调用；连接失败提示先启动实例，HTTP 错误原样透传服务端报错。
func (c *apiClient) do(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接 proxyd（%s）：实例未在运行？请先 proxyd start（或 proxyd serve）", c.base)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("%s", msg)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
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
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("无法连接 proxyd（%s）：实例未在运行？请先 proxyd start（或 proxyd serve）", c.base)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = resp.Status
		}
		return "", fmt.Errorf("%s", msg)
	}
	return string(b), nil
}

func cmdMode(args []string) error {
	cfgFile, rest := parseCFlag("mode", args)
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
	cfgFile, _ := parseCFlag("refresh", args)
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
	cfgFile, _ := parseCFlag("test", args)
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
	cfgFile, rest := parseCFlag("subs", args)
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
		fmt.Fprintln(tw, "NAME\tURL\tNODES(alive/total)")
		for _, s := range ov.Subs {
			fmt.Fprintf(tw, "%s\t%s\t%d/%d\n", s.Name, s.URL, s.Alive, s.Total)
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
	case "del":
		if len(rest) != 2 {
			return fmt.Errorf("用法: proxyd subs del [-c 配置] <name>")
		}
		if err := c.do(http.MethodDelete, "/api/subscriptions/"+rest[1], nil, nil); err != nil {
			return err
		}
		fmt.Printf("订阅 %q 已删除（后台刷新中）\n", rest[1])
		return nil
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd subs list|add <name> <url>|del <name>", sub)
	}
}

func cmdNodes(args []string) error {
	cfgFile, rest := parseCFlag("nodes", args)
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
	cfgFile, rest := parseCFlag("rules", args)
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
		return fmt.Errorf("未知操作 %q，用法: proxyd rules list|add \"<规则>\"|del <下标>", sub)
	}
}

func cmdRuleURLs(args []string) error {
	cfgFile, rest := parseCFlag("rule-urls", args)
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
	cfgFile, rest := parseCFlag("groups", args)
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
		fmt.Fprintln(tw, "NAME\tPORT\tNODES")
		for _, g := range groups {
			fmt.Fprintf(tw, "%s\t%d\t%s\n", g.Name, g.Port, strings.Join(g.Nodes, ","))
		}
		_ = tw.Flush()
		if len(groups) == 0 {
			fmt.Println("暂无节点分组")
		}
		return nil
	case "add":
		if len(rest) < 4 {
			return fmt.Errorf("用法: proxyd groups add [-c 配置] <name> <port> <节点名...>")
		}
		port, err := strconv.Atoi(rest[2])
		if err != nil {
			return fmt.Errorf("端口必须是整数: %q", rest[2])
		}
		if err := c.do(http.MethodPost, "/api/groups",
			config.NodeGroup{Name: rest[1], Port: port, Nodes: rest[3:]}, nil); err != nil {
			return err
		}
		fmt.Printf("分组 %q 已添加（端口 %d）\n", rest[1], port)
		return nil
	case "del":
		if len(rest) != 2 {
			return fmt.Errorf("用法: proxyd groups del [-c 配置] <name>")
		}
		if err := c.do(http.MethodDelete, "/api/groups/"+rest[1], nil, nil); err != nil {
			return err
		}
		fmt.Printf("分组 %q 已删除\n", rest[1])
		return nil
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd groups list|add <name> <port> <节点...>|del <name>", sub)
	}
}

func cmdPortRange(args []string) error {
	cfgFile, rest := parseCFlag("port-range", args)
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
	cfgFile, rest := parseCFlag("auto-port", args)
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

func cmdMainAuto(args []string) error {
	cfgFile, rest := parseCFlag("main-auto", args)
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
	cfgFile, rest := parseCFlag("main-node", args)
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
		return fmt.Errorf("用法: proxyd main-node [-c 配置] [节点key|off]")
	}
	key := rest[0]
	if strings.ToLower(key) == "off" {
		key = ""
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
	cfgFile, rest := parseCFlag("main-port", args)
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
