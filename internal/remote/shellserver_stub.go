//go:build !linux && !darwin && !windows

package remote

// 本文件为不支持内嵌 shell 的平台提供空实现。

import (
	"errors"
	"net"

	"proxyd/internal/config"
)

// errShellTerminalUnsupported 表示当前平台没有可用的进程内 shell 服务。
var errShellTerminalUnsupported = errors.New("当前平台不支持 Web 终端")

// localShellSSHHandler 在不支持的平台上始终返回 errShellTerminalUnsupported。
//
// 参数说明：
//   - stateDir: string，不支持平台不使用该路径。
//
// 返回值说明：func(net.Conn) 恒为 nil；error 恒为平台不支持错误。
//
// 错误情况：无条件返回 errShellTerminalUnsupported。
func localShellSSHHandler(_ string) (func(net.Conn), error) {
	return nil, errShellTerminalUnsupported
}

// configuredShellSSHHandler 为不支持的平台提供一致的配置入口。
// 参数说明：string 为状态目录，bool 为公钥开关，[]config.RemoteSSHKey 为授权集合，均不使用。
// 返回值说明：nil 处理器与平台不支持错误。
// 错误情况：始终返回 errShellTerminalUnsupported，不创建任何监听或 shell。
func configuredShellSSHHandler(_ string, _ bool, _ []config.RemoteSSHKey) (func(net.Conn), error) {
	return nil, errShellTerminalUnsupported
}

// managedShellSSHHandler 为不支持的平台保留动态授权入口。
// 参数说明：string 为状态目录，*sshAccess 为授权服务，均不使用。
// 返回值说明：nil 与平台错误。
// 错误情况：始终拒绝启动，不创建监听或后台协程。
func managedShellSSHHandler(_ string, _ *sshAccess) (func(net.Conn), error) {
	return nil, errShellTerminalUnsupported
}
