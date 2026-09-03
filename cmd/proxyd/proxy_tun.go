package main

// 代理域子命令：TUN 与 DNS 预设（tun/dns-preset）。

import (
	"fmt"
	"net/http"
	"strings"

	"proxyd/internal/app"
)

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
