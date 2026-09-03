// Package autostart 管理 proxyd 的开机自启项：
// macOS 写 /Library/LaunchDaemons/com.proxyd.plist（系统启动即托管 serve）；
// Linux 写 ~/.config/systemd/user/proxyd.service 并 systemctl --user enable；
// Windows 写注册表 HKCU\...\Run（执行 start 派生后台进程，避免弹控制台窗口）。
// 不支持的平台返回 ErrUnsupported。
package autostart

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrUnsupported 表示当前平台不支持开机自启。
var ErrUnsupported = errors.New("当前平台不支持开机自启")

// run 执行外部命令；定义为包级变量以便测试替换。
//
// 参数说明：
//   - name: string，可执行文件名称或绝对路径。
//   - args: ...string，保持参数边界传给目标程序的参数列表。
//
// 返回值说明：string 和 error，分别为合并后的标准输出/错误与执行结果。
//
// 错误情况：进程无法启动或退出码非零时返回包含命令、原始错误和输出摘要的错误。
var run = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Options 是注册自启项所需的参数；路径都应为绝对路径。
type Options struct {
	Exe        string // proxyd 二进制绝对路径
	ConfigPath string // 配置文件绝对路径
	StateDir   string // 状态目录（日志文件落在其下）
}

// On 注册开机自启并立即启动一次。
//
// 参数说明：
//   - opt: Options，包含可执行文件、配置文件和状态目录的绝对路径。
//
// 返回值说明：error，注册且启动成功时为 nil。
//
// 错误情况：路径准备、系统服务注册或立即启动失败时返回底层错误；macOS 非 root
// 进程会请求管理员授权以写入系统 LaunchDaemon。
func On(opt Options) error { return on(opt) }

// Off 移除开机自启；由服务管理器托管的实例可能同时被停止。
//
// 参数说明：无。
//
// 返回值说明：error，自启项不存在或移除成功时为 nil。
//
// 错误情况：系统服务卸载或文件删除失败时返回错误；macOS 可能请求管理员授权。
func Off() error { return off() }

// Status 报告自启项是否存在。
//
// 参数说明：无。
//
// 返回值说明：bool 和 error，分别表示自启项是否存在以及查询错误。
//
// 错误情况：平台不支持或文件状态查询失败时返回错误。
func Status() (bool, error) { return status() }
