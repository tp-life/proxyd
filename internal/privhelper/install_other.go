//go:build !darwin

package privhelper

// 非 macOS 平台的统一助手安装链路存根：特权助手仅为 macOS 模型
//（Linux 主进程可直接创建 utun 等价设备、走 setcap 能力位，Windows 不适配）。

import "fmt"

// Install 在非 macOS 平台返回不支持错误。
func Install() error {
	return fmt.Errorf("统一特权助手仅支持 macOS: %w", ErrUnsupported)
}

// Uninstall 在非 macOS 平台返回不支持错误。
func Uninstall() error {
	return fmt.Errorf("统一特权助手仅支持 macOS: %w", ErrUnsupported)
}
