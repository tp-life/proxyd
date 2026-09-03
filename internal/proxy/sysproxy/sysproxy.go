// Package sysproxy 开关系统代理（把系统 HTTP/HTTPS/SOCKS 代理指向 proxyd 主端口）。
// macOS 用 networksetup，Linux 用 gsettings（GNOME），Windows 用注册表；
// 均为 best-effort，不支持的平台返回 ErrUnsupported。
package sysproxy

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrUnsupported 表示当前平台不支持系统代理设置。
var ErrUnsupported = errors.New("当前平台不支持系统代理设置")

// run 执行外部命令；定义为包级变量以便测试替换。
var run = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// On 把系统 HTTP/HTTPS/SOCKS 代理指向 host:port。
func On(host string, port int) error { return on(host, port) }

// Off 关闭系统代理。
func Off() error { return off() }

// Status 报告系统代理是否已指向 host:port。
func Status(host string, port int) (bool, error) { return status(host, port) }
