package main

// 代理域子命令：连接与实时速率（conn/traffic）。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

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
