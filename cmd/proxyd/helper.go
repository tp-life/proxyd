package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"proxyd/internal/gateway"
	"proxyd/internal/privhelper"
	"proxyd/internal/proxy/tunhelper"
)

// 统一特权助手命令：`proxyd helper install|uninstall|status` 是用户入口，
// 管理 macOS 统一 root 特权助手（TUN 与 LAN 网关及后续模块的特权操作共用，
// plist 与安装链路见 internal/privhelper）；无参形式是 launchd 托管的服务端
// 入口（内部使用，不应手工调用）。旧写法 `proxyd tun helper ...` 与
// `proxyd gateway helper ...` 仍可用，内部转发到同一实现。

// cmdHelper 分派统一助手命令：无参且 root 视为 launchd 拉起的服务端；
// 其余进入安装管理子命令。参数：args 为 []string，helper 之后的参数。
// 返回：error。错误：非法参数、非 macOS、授权拒绝或 launchctl 失败原样上报。
func cmdHelper(args []string) error {
	if len(args) == 0 {
		if os.Geteuid() == 0 {
			return cmdHelperServe(nil)
		}
		return fmt.Errorf("用法: proxyd helper install|uninstall|status（macOS 统一特权助手，各模块特权操作共用）")
	}
	switch args[0] {
	case "install", "uninstall", "status":
		return cmdHelperLocal(args[0])
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd helper install|uninstall|status", args[0])
	}
}

// cmdHelperLocal 执行统一特权助手的本地安装管理。
// 安装/卸载是本地特权操作：终端 sudo 或管理员授权弹窗，不经 HTTP API。
//
// 参数：op 为 string，install|uninstall|status。返回：error。
//
// 错误情况：非 macOS 平台返回不支持错误；status 在任一模块握手不可达时
// 返回「特权助手未就绪」。
func cmdHelperLocal(op string) error {
	switch op {
	case "install":
		if err := privhelper.Install(); err != nil {
			return err
		}
		fmt.Println("统一特权助手已安装并启动（launchd 系统域常驻，root 运行；TUN 与 LAN 网关及后续模块的特权操作共用）")
		return nil
	case "uninstall":
		if err := privhelper.Uninstall(); err != nil {
			return err
		}
		fmt.Println("统一特权助手已卸载（pf 规则已清除，IPv4 转发恢复原值；各模块特权操作均不可用）")
		return nil
	case "status":
		tunStatus := tunhelper.InstalledStatus()
		gatewayStatus := gateway.HelperInstalledStatus()
		fmt.Printf("已安装: %s，已加载: %s\n", onOffText(tunStatus.Installed), onOffText(tunStatus.Running))
		fmt.Printf("TUN 握手可达: %s；LAN 网关握手可达: %s\n", onOffText(tunStatus.Reachable), onOffText(gatewayStatus.Reachable))
		if tunStatus.Detail != "" {
			fmt.Printf("说明: %s\n", tunStatus.Detail)
		}
		if !tunStatus.Installed || !tunStatus.Running || !tunStatus.Reachable || !gatewayStatus.Reachable {
			return fmt.Errorf("特权助手未就绪")
		}
		return nil
	}
	return fmt.Errorf("未知操作 %q，用法: proxyd helper install|uninstall|status", op)
}

// cmdHelperServe 是统一特权助手服务端入口（无参 `proxyd helper`，
// 由 launchd 系统域以 root 托管）。同一进程内服务两个模块 socket：
// TUN（utun 创建 + fd 回传）与 LAN 网关（pf/转发管理，带看门狗）；
// 任一服务端返回即整体退出，launchd KeepAlive 会立即拉起。
// 旧版独立子命令 `tun-helper`/`gateway-helper` 保留：旧 plist 在重装
// 统一助手前仍可运行。
func cmdHelperServe(_ []string) error {
	logf := func(format string, args ...any) { log.Printf(format, args...) }
	errCh := make(chan error, 2)
	go func() { errCh <- gateway.RunHelperServer(context.Background(), logf) }()
	go func() { errCh <- tunhelper.RunHelperServer(context.Background(), logf) }()
	return <-errCh
}
