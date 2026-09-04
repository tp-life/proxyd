//go:build linux

package main

import (
	"fmt"
	"os/exec"
)

// desktopClientCommand 选择 Linux 上可用的 RDP/VNC 图形客户端。
//
// 参数说明：
//   - protocol: desktopProtocol，目标桌面协议。
//   - host: string，临时转发的本地回环主机。
//   - port: string，系统随机分配的本地端口。
//
// 返回值说明：desktopClientLaunch 和 error；优先使用专用客户端，缺失时回退 Remmina。
//
// 错误情况：PATH 中不存在 xfreerdp/xfreerdp3/remmina 或 vncviewer/remmina 时返回包含
// 安装建议的错误，不会启动外部进程。
func desktopClientCommand(protocol desktopProtocol, host, port string) (desktopClientLaunch, error) {
	address := host + ":" + port
	switch protocol {
	case desktopProtocolRDP:
		for _, name := range []string{"xfreerdp3", "xfreerdp"} {
			if executable, err := exec.LookPath(name); err == nil {
				return desktopClientLaunch{command: exec.Command(executable, "/v:"+address)}, nil
			}
		}
		if executable, err := exec.LookPath("remmina"); err == nil {
			return desktopClientLaunch{command: exec.Command(executable, "-c", "rdp://"+address)}, nil
		}
		return desktopClientLaunch{}, fmt.Errorf("未找到 RDP 客户端，请安装 xfreerdp3、xfreerdp 或 remmina")
	case desktopProtocolVNC:
		if executable, err := exec.LookPath("vncviewer"); err == nil {
			return desktopClientLaunch{command: exec.Command(executable, host+"::"+port)}, nil
		}
		if executable, err := exec.LookPath("remmina"); err == nil {
			return desktopClientLaunch{command: exec.Command(executable, "-c", "vnc://"+address)}, nil
		}
		return desktopClientLaunch{}, fmt.Errorf("未找到 VNC 客户端，请安装 vncviewer 或 remmina")
	default:
		return desktopClientLaunch{}, fmt.Errorf("远程桌面协议 %q 不受支持", protocol)
	}
}
