package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"tailscale.com/types/key"

	"proxyd/internal/config"
	"proxyd/internal/remote"
)

// desktopOptions 是 desk 命令完成参数解析后的应用层输入。
//
// 它只保留配置位置、桌面协议、远端标识与可选客户端身份，不承载端口和平台命令等
// 可由领域值对象或基础设施适配器推导的信息。
type desktopOptions struct {
	configFile   string
	protocol     desktopProtocol
	remoteTarget string
	clientKey    string
}

// desktopProtocol 表示 CLI 支持拉起的远程桌面协议。
//
// 协议和默认端口属于用户交互及平台客户端适配概念，因此保留在 cmd 层；remote 数据面
// 只接收最终 TCP 端口，不感知 RDP/VNC，避免反向依赖桌面功能。
type desktopProtocol string

const (
	// desktopProtocolRDP 表示 Microsoft Remote Desktop Protocol，默认使用 TCP 3389。
	desktopProtocolRDP desktopProtocol = "rdp"
	// desktopProtocolVNC 表示 Remote Framebuffer/VNC，默认使用 TCP 5900。
	desktopProtocolVNC desktopProtocol = "vnc"
)

// desktopClientLaunch 描述一次待启动的本机远程桌面客户端。
//
// cleanup 用于移除 macOS RDP 配置等临时资源；它不负责关闭 tailcat 转发，转发生命周期
// 由 cmdDesk 统一管理，避免平台适配器绕过应用层释放顺序。
type desktopClientLaunch struct {
	command *exec.Cmd
	cleanup func()
}

// parseDesktopProtocol 把用户输入转换为受支持的桌面协议。
//
// 参数说明：
//   - value: string，CLI 传入的协议名称；忽略首尾空白和大小写。
//
// 返回值说明：desktopProtocol 和 error；rdp/vnc 返回对应值。
//
// 错误情况：输入不是 rdp 或 vnc 时返回包含合法取值的错误。
func parseDesktopProtocol(value string) (desktopProtocol, error) {
	protocol := desktopProtocol(strings.ToLower(strings.TrimSpace(value)))
	switch protocol {
	case desktopProtocolRDP, desktopProtocolVNC:
		return protocol, nil
	default:
		return "", fmt.Errorf("远程桌面协议 %q 不受支持（可选 rdp 或 vnc）", value)
	}
}

// port 返回远程桌面协议约定的服务端 TCP 端口。
//
// 参数说明：无。
//
// 返回值说明：int；RDP 返回 3389，VNC 返回 5900，非法零值返回 0。
//
// 错误情况：方法本身不返回错误；非法值以 0 触发临时转发的端口校验。
func (p desktopProtocol) port() int {
	switch p {
	case desktopProtocolRDP:
		return 3389
	case desktopProtocolVNC:
		return 5900
	default:
		return 0
	}
}

// parseDesktopOptions 解析 desk 命令参数并构造受约束的协议值。
//
// 参数说明：
//   - args: []string，`proxyd desk` 后的参数；flag 必须位于协议和远端名称之前。
//
// 返回值说明：desktopOptions 和 error；成功时包含完整配置路径及合法 rdp/vnc 协议。
//
// 错误情况：flag 非法、缺少协议/远端或协议不受支持时返回用法错误，不会读取配置
// 或建立网络连接。协议后的多个参数会按空格重新拼接，以兼容未加引号的中文显示名。
func parseDesktopOptions(args []string) (desktopOptions, error) {
	fs := flag.NewFlagSet("desk", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configFile := fs.String("c", config.DefaultPath(), "配置文件路径")
	clientKey := fs.String("client-key", "", "临时客户端身份私钥")
	if err := fs.Parse(args); err != nil {
		return desktopOptions{}, fmt.Errorf("解析 desk 参数失败: %w", err)
	}
	if fs.NArg() < 2 {
		return desktopOptions{}, fmt.Errorf("用法: proxyd desk [-c 配置] [--client-key privkey:私钥] <rdp|vnc> <远端名|token>")
	}
	protocol, err := parseDesktopProtocol(fs.Arg(0))
	if err != nil {
		return desktopOptions{}, err
	}
	return desktopOptions{
		configFile: *configFile,
		protocol:   protocol,
		// 远端显示名允许包含空格。shell 会把未加引号的名称拆成多个参数，因此这里
		// 重新拼接协议后的参数；tailcat token 不含空格，不会因此改变直接 token 模式。
		remoteTarget: strings.TrimSpace(strings.Join(fs.Args()[1:], " ")),
		clientKey:    strings.TrimSpace(*clientKey),
	}, nil
}

// resolveDesktopToken 把 desk 命令的远端名称解析为完整 tailcat token。
//
// 参数说明：
//   - configFile: string，保存远端别名的 proxyd 配置文件路径。
//   - target: string，远端别名或完整 tc... token。
//
// 返回值说明：string 和 error；成功返回经过格式校验的完整 token。
//
// 错误情况：直接 token 格式非法、配置读取失败或别名不存在时返回错误；直接传 token
// 时不强制要求配置文件存在，便于在临时机器上使用。
func resolveDesktopToken(configFile, target string) (string, error) {
	target = strings.TrimSpace(target)
	if strings.HasPrefix(target, "tc") {
		if err := remote.ValidateToken(target); err != nil {
			return "", err
		}
		return target, nil
	}
	cfg, err := config.Load(configFile)
	if err != nil {
		return "", fmt.Errorf("读取配置 %s 失败（用于解析远端名）: %w", configFile, err)
	}
	return remote.ResolveToken(cfg.Remote.Remotes, target)
}

// desktopClientKey 解析 desk 命令使用的客户端身份。
//
// 参数说明：
//   - options: desktopOptions，包含显式 --client-key 与配置文件路径。
//
// 返回值说明：key.NodePrivate 和 error；显式密钥优先，否则复用 proxyd 持久身份。
//
// 错误情况：显式私钥格式非法时返回错误；持久密钥读取失败沿用 pipeClientKey 的保守
// 降级策略并打印警告，返回零值身份。
func desktopClientKey(options desktopOptions) (key.NodePrivate, error) {
	if options.clientKey == "" {
		return pipeClientKey(options.configFile), nil
	}
	clientKey, err := remote.ParseClientKey(options.clientKey)
	if err != nil {
		return key.NodePrivate{}, err
	}
	return clientKey, nil
}

// cmdDesk 经 tailcat 临时转发启动本机 RDP 或 VNC 客户端。
//
// 功能说明：
// 命令在当前登录用户会话中绑定随机回环端口，把流量转发到远端默认桌面端口，再调用
// 平台原生客户端。GUI 客户端退出或收到 Ctrl+C（SIGINT）后关闭 listener、活动连接与
// tailcat 客户端；临时转发不会写入 proxyd 配置，也不依赖守护进程运行。
//
// 参数说明：
//   - args: []string，格式为 `[-c 配置] [--client-key 私钥] <rdp|vnc> <远端名|token>`。
//
// 返回值说明：error；桌面客户端正常退出或用户中断时返回 nil。
//
// 错误情况：参数/配置/token/身份非法、本地监听失败、系统未安装对应客户端、GUI
// 启动失败或客户端异常退出时返回带上下文的错误；所有已创建资源仍会清理。
func cmdDesk(args []string) error {
	options, err := parseDesktopOptions(args)
	if err != nil {
		return err
	}
	token, err := resolveDesktopToken(options.configFile, options.remoteTarget)
	if err != nil {
		return err
	}
	clientKey, err := desktopClientKey(options)
	if err != nil {
		return err
	}

	forward, err := remote.StartTransientForward(token, options.protocol.port(), clientKey)
	if err != nil {
		return fmt.Errorf("创建远程桌面临时转发失败: %w", err)
	}
	defer forward.Close()
	host, portText, err := net.SplitHostPort(forward.Address())
	if err != nil {
		return fmt.Errorf("解析临时转发地址 %q 失败: %w", forward.Address(), err)
	}
	launch, err := desktopClientCommand(options.protocol, host, portText)
	if err != nil {
		return err
	}
	if launch.cleanup != nil {
		defer launch.cleanup()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Printf("远程桌面临时转发：%s → %s:%d（tailcat 会优先尝试 NAT 直连，失败时回退 DERP）\n", forward.Address(), options.remoteTarget, options.protocol.port())
	fmt.Println("桌面客户端退出后将自动释放转发；也可按 Ctrl+C 主动断开。")
	if err := launch.command.Start(); err != nil {
		return fmt.Errorf("启动 %s 客户端失败: %w", strings.ToUpper(string(options.protocol)), err)
	}

	wait := make(chan error, 1)
	go func() {
		wait <- launch.command.Wait()
	}()
	select {
	case waitErr := <-wait:
		if waitErr != nil {
			return fmt.Errorf("%s 客户端异常退出: %w", strings.ToUpper(string(options.protocol)), waitErr)
		}
		return nil
	case <-ctx.Done():
		// 用户明确中断时先关闭转发，确保 listener、活动连接及 tailcat 客户端立即释放。
		// 随后终止本次启动的包装进程；部分平台会把 GUI 会话移交给既有进程，因此等待
		// 设置上限，避免客户端不响应退出时 desk 命令自身长期挂住。
		_ = forward.Close()
		if launch.command.Process != nil {
			_ = launch.command.Process.Kill()
		}
		select {
		case <-wait:
		case <-time.After(2 * time.Second):
		}
		return nil
	}
}
