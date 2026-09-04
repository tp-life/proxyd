//go:build !linux && !darwin && !windows

package remote

// 本文件为不支持内嵌 shell 的平台提供空实现。

import (
	"errors"
	"net"
)

// errShellTerminalUnsupported 表示当前平台没有可用的进程内 shell 服务。
var errShellTerminalUnsupported = errors.New("当前平台不支持 Web 终端")

// localShellSSHHandler 在不支持的平台上始终返回 errShellTerminalUnsupported。
//
// 参数说明：无。
//
// 返回值说明：func(net.Conn) 恒为 nil；error 恒为平台不支持错误。
//
// 错误情况：无条件返回 errShellTerminalUnsupported。
func localShellSSHHandler() (func(net.Conn), error) {
	return nil, errShellTerminalUnsupported
}
