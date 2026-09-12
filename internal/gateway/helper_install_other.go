//go:build !darwin

package gateway

// 非 macOS 平台的 helper 安装链路存根：特权 helper 仅为 macOS 模型
//（Linux 走 setcap 能力位，Windows 不做网关，见 docs/adr/0003）。

import (
	"context"
	"fmt"
)

// RunHelperServer 在非 macOS 平台返回不支持错误。
func RunHelperServer(context.Context, func(format string, args ...any)) error {
	return fmt.Errorf("gateway helper 仅支持 macOS: %w", ErrUnsupported)
}

// HelperInstall 在非 macOS 平台返回不支持错误。
func HelperInstall() error {
	return fmt.Errorf("gateway helper 仅支持 macOS: %w", ErrUnsupported)
}

// HelperUninstall 在非 macOS 平台返回不支持错误。
func HelperUninstall() error {
	return fmt.Errorf("gateway helper 仅支持 macOS: %w", ErrUnsupported)
}

// HelperInstalledStatus 在非 macOS 平台报告不支持。
func HelperInstalledStatus() HelperStatus {
	return HelperStatus{Detail: ErrUnsupported.Error()}
}
