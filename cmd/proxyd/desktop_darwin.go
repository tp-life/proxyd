//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// desktopClientCommand 构造 macOS 远程桌面客户端启动命令。
//
// 参数说明：
//   - protocol: desktopProtocol，目标桌面协议。
//   - host: string，临时转发的本地回环主机。
//   - port: string，系统随机分配的本地端口。
//
// 返回值说明：desktopClientLaunch 和 error；VNC 使用系统“屏幕共享”，RDP 生成不含
// 凭据的临时 .rdp 文件并交给已安装的关联客户端，`open -W` 保持进程直到 GUI 退出。
//
// 错误情况：临时 RDP 文件创建/写入失败时返回错误；系统没有 .rdp 关联应用时错误会
// 在命令启动或等待阶段返回。cleanup 会移除临时文件。
func desktopClientCommand(protocol desktopProtocol, host, port string) (desktopClientLaunch, error) {
	address := host + ":" + port
	switch protocol {
	case desktopProtocolVNC:
		return desktopClientLaunch{
			command: exec.Command("/usr/bin/open", "-W", "vnc://"+address),
		}, nil
	case desktopProtocolRDP:
		file, err := os.CreateTemp("", "proxyd-desktop-*.rdp")
		if err != nil {
			return desktopClientLaunch{}, fmt.Errorf("创建临时 RDP 配置失败: %w", err)
		}
		path := file.Name()
		content := fmt.Sprintf("full address:s:%s\r\nprompt for credentials:i:1\r\n", address)
		if _, err := file.WriteString(content); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return desktopClientLaunch{}, fmt.Errorf("写入临时 RDP 配置失败: %w", err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(path)
			return desktopClientLaunch{}, fmt.Errorf("关闭临时 RDP 配置失败: %w", err)
		}
		return desktopClientLaunch{
			command: exec.Command("/usr/bin/open", "-W", filepath.Clean(path)),
			cleanup: func() {
				_ = os.Remove(path)
			},
		}, nil
	default:
		return desktopClientLaunch{}, fmt.Errorf("远程桌面协议 %q 不受支持", protocol)
	}
}
