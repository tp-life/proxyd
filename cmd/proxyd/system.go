package main

// 通用子命令：配置导入导出、日志、版本检查（config/logs/update-check）。

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"proxyd/internal/api"
	"proxyd/internal/app"
	"proxyd/internal/config"
)

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
