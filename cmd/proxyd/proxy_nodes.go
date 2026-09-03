package main

// 代理域子命令：节点列表与手动节点管理（nodes）。

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"text/tabwriter"

	"proxyd/internal/app"
)

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
