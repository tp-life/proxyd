//go:build !darwin && !linux

package tunhelper

// 非 unix 平台的 fd 回传存根：SCM_RIGHTS 机制仅 unix 系有意义，
// Windows 不支持本 helper（安装/客户端链路同样返回 ErrUnsupported）。

import (
	"fmt"
	"net"
)

// closeHelperFD 在非 unix 平台为空操作。
func closeHelperFD(int) {}

// CloseFD 在非 unix 平台为空操作（本包不支持这些平台，不会产出 fd）。
func CloseFD(int) {}

// sendHelperFD 在非 unix 平台返回不支持错误。
func sendHelperFD(net.Conn, int) error {
	return fmt.Errorf("fd 回传仅支持 unix 平台: %w", ErrUnsupported)
}

// recvHelperFD 在非 unix 平台返回不支持错误。
func recvHelperFD(net.Conn) (int, error) {
	return -1, fmt.Errorf("fd 回传仅支持 unix 平台: %w", ErrUnsupported)
}
