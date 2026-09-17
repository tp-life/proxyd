//go:build !darwin

package tunhelper

// 非 macOS 平台的 helper 客户端存根。

import "fmt"

// Precheck 在非 macOS 平台返回不支持错误。
func Precheck() error {
	return fmt.Errorf("tun 特权 helper 仅支持 macOS: %w", ErrUnsupported)
}

// RequestTUN 在非 macOS 平台返回不支持错误。
func RequestTUN(CreateTUNParams) (int, string, error) {
	return -1, "", fmt.Errorf("tun 特权 helper 仅支持 macOS: %w", ErrUnsupported)
}
