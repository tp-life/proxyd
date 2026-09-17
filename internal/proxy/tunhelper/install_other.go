//go:build !darwin

package tunhelper

// 非 macOS 平台的 helper 安装链路存根：特权 helper 仅为 macOS 模型
//（Linux 主进程可直接创建 utun 等价设备，Windows 不适配）。

import (
	"context"
	"fmt"
)

// RunHelperServer 在非 macOS 平台返回不支持错误。
func RunHelperServer(context.Context, func(format string, args ...any)) error {
	return fmt.Errorf("tun helper 仅支持 macOS: %w", ErrUnsupported)
}

// Install 在非 macOS 平台返回不支持错误。
func Install() error {
	return fmt.Errorf("tun helper 仅支持 macOS: %w", ErrUnsupported)
}

// Uninstall 在非 macOS 平台返回不支持错误。
func Uninstall() error {
	return fmt.Errorf("tun helper 仅支持 macOS: %w", ErrUnsupported)
}

// InstalledStatus 在非 macOS 平台报告不支持。
func InstalledStatus() HelperStatus {
	return HelperStatus{Detail: ErrUnsupported.Error()}
}
