//go:build windows

package main

import (
	"fmt"
	"os/exec"
)

// desktopClientCommand 选择 Windows 上可用的 RDP/VNC 图形客户端。
//
// 参数说明：
//   - protocol: desktopProtocol，目标桌面协议。
//   - host: string，临时转发的本地回环主机。
//   - port: string，系统随机分配的本地端口。
//
// 返回值说明：desktopClientLaunch 和 error；RDP 使用系统 mstsc，VNC 使用 PATH 中的
// vncviewer。启动命令保持运行，供上层在窗口退出后清理临时隧道。
//
// 错误情况：系统找不到对应客户端时返回可操作错误，不会尝试打开不受信任的 URL。
func desktopClientCommand(protocol desktopProtocol, host, port string) (desktopClientLaunch, error) {
	address := host + ":" + port
	switch protocol {
	case desktopProtocolRDP:
		executable, err := exec.LookPath("mstsc.exe")
		if err != nil {
			return desktopClientLaunch{}, fmt.Errorf("未找到 Windows 远程桌面客户端 mstsc.exe: %w", err)
		}
		return desktopClientLaunch{command: exec.Command(executable, "/v:"+address)}, nil
	case desktopProtocolVNC:
		for _, name := range []string{"vncviewer.exe", "vncviewer"} {
			if executable, err := exec.LookPath(name); err == nil {
				return desktopClientLaunch{command: exec.Command(executable, host+"::"+port)}, nil
			}
		}
		return desktopClientLaunch{}, fmt.Errorf("未找到 VNC 客户端，请安装 TigerVNC 并确保 vncviewer 在 PATH 中")
	default:
		return desktopClientLaunch{}, fmt.Errorf("远程桌面协议 %q 不受支持", protocol)
	}
}
