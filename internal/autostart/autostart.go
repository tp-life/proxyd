// Package autostart 管理 proxyd 的开机自启项：
// macOS 写 ~/Library/LaunchAgents/com.proxyd.plist（launchd 直接托管 serve）；
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
func On(opt Options) error { return on(opt) }

// Off 移除开机自启（不停止当前可能在运行的实例）。
func Off() error { return off() }

// Status 报告自启项是否存在。
func Status() (bool, error) { return status() }
