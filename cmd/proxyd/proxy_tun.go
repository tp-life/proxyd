package main

// 代理域子命令：TUN 与 DNS 预设（tun/dns-preset）。
// tun helper install|uninstall|status 是 macOS TUN 特权助手的本地安装管理，
// 不经运行中实例的 API；隐藏子命令 `proxyd tun-helper` 由 launchd 以 root 托管。

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"proxyd/internal/app"
	"proxyd/internal/proxy/tunhelper"
)

// confirmPrompt 输出确认提示并从终端读取 y/n（回车默认同意）；声明为变量以便测试替换。
var confirmPrompt = func(prompt string) bool {
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "" || ans == "y" || ans == "yes"
}

// cmdTun 通过运行中实例的 API 查看或切换 TUN 模式。
//
// 参数：
//   - args: []string，支持 `-c <配置文件>` 和一个 `on|off|status` 位置参数，
//     或 `helper install|uninstall|status`（本地特权操作，不经 API）。
//
// 返回值：
//   - error：参数无效、实例未运行、权限不足或热更新失败时返回错误。
//
// 错误情况：macOS 普通用户开启 TUN 依赖 tun-helper；API 返回 helper 缺失指引且
// 当前是交互终端时，提供就地安装（终端 sudo 授权）并重试一次。
func cmdTun(args []string) error {
	cfgFile, rest, err := parseCFlag("tun", args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("用法: proxyd tun [-c 配置] on|off|status|helper install|uninstall|status")
	}
	if rest[0] == "helper" {
		return cmdTunHelperLocal(rest[1:])
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd tun [-c 配置] on|off|status|helper install|uninstall|status")
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
		if status.Helper != nil {
			fmt.Printf("特权助手：已安装 %s，可达 %s\n", onOffText(status.Helper.Installed), onOffText(status.Helper.Reachable))
			if status.Helper.Detail != "" {
				fmt.Printf("助手说明：%s\n", status.Helper.Detail)
			}
		}
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
		if enabled && offerTunHelperInstall(err) {
			// 就地安装 helper 成功后重试一次开启。
			if retryErr := c.do(http.MethodPost, "/api/tun", map[string]bool{"enabled": enabled}, &status); retryErr != nil {
				return retryErr
			}
		} else {
			return err
		}
	}
	if enabled {
		fmt.Printf("TUN 已开启并确认生效（%s，系统流量由 mihomo 接管）\n", status.Platform)
	} else {
		fmt.Println("TUN 已关闭（系统路由已由 mihomo 恢复）")
	}
	return nil
}

// cmdTunHelperLocal 处理 tun helper 安装管理子命令（install|uninstall|status）。
// 安装/卸载是本地特权操作：终端 sudo 或管理员授权弹窗，不经 HTTP API。
//
// 参数：args 为 []string，helper 之后的子命令参数；返回 error。
//
// 错误情况：非 macOS 平台返回不支持错误；授权拒绝或 launchctl 失败原样上报。
func cmdTunHelperLocal(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("用法: proxyd tun helper install|uninstall|status（macOS TUN 特权助手的本地安装管理）")
	}
	switch args[0] {
	case "install":
		if err := tunhelper.Install(); err != nil {
			return err
		}
		fmt.Println("tun-helper 已安装并启动（launchd 系统域常驻，root 运行；一次性授权，之后开启 TUN 无需 sudo）")
		return nil
	case "uninstall":
		if err := tunhelper.Uninstall(); err != nil {
			return err
		}
		fmt.Println("tun-helper 已卸载")
		return nil
	case "status":
		status := tunhelper.InstalledStatus()
		fmt.Printf("已安装: %s，已加载: %s，握手可达: %s\n",
			onOffText(status.Installed), onOffText(status.Running), onOffText(status.Reachable))
		if status.Detail != "" {
			fmt.Printf("说明: %s\n", status.Detail)
		}
		if !status.Installed || !status.Running || !status.Reachable {
			return fmt.Errorf("tun-helper 未就绪")
		}
		return nil
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd tun helper install|uninstall|status", args[0])
	}
}

// cmdTunHelperServe 是 tun-helper 服务端入口（内部子命令 `proxyd tun-helper`，
// 由 launchd 以 root 托管运行，不应手工调用）。
//
// 参数：args 为 []string（忽略，无参数）；返回 error。
//
// 错误情况：非 root、非 macOS 或监听失败时返回错误，launchd 按 KeepAlive 重启。
func cmdTunHelperServe(_ []string) error {
	return tunhelper.RunHelperServer(context.Background(), func(format string, args ...any) {
		log.Printf(format, args...)
	})
}

// offerTunHelperInstall 在 tun on 因 helper 缺失失败且当前为交互终端时，
// 确认后就地安装 tun-helper（终端 sudo 授权），成功返回 true 供调用方重试。
//
// 参数：
//   - cause: error，API 返回的开启失败错误。
//
// 返回值：
//   - bool：用户确认且安装成功时为 true；其余情况（非终端、错误与 helper 无关、
//     用户取消、安装失败）为 false，由调用方原样返回 cause。
//
// 错误情况：安装失败会打印原因，错误本身不向上替换原始 cause。
func offerTunHelperInstall(cause error) bool {
	if !isTerminal() || !strings.Contains(cause.Error(), "tun helper install") {
		return false
	}
	if !confirmPrompt("需要安装 TUN 特权助手 tun-helper（一次性管理员授权，密码在终端输入），现在安装？[Y/n] ") {
		return false
	}
	if err := tunhelper.Install(); err != nil {
		fmt.Printf("tun-helper 安装失败: %v\n", err)
		return false
	}
	fmt.Println("tun-helper 已安装，重试开启 TUN…")
	return true
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
