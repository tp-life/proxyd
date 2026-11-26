//go:build linux

package autostart

import (
	"os"
	"path/filepath"
	"strconv"
)

// Linux 实现：普通用户写 systemd user unit（日志走 journalctl --user）；
// 以 root 运行（TUN 模式）时没有 user session bus，`systemctl --user` 必然失败，
// 因此改写系统级 unit /etc/systemd/system/proxyd.service（日志走 journalctl -u proxyd）。

const unitName = "proxyd.service"

// systemUnitDir 为系统级 unit 目录；测试可替换。
var systemUnitDir = "/etc/systemd/system"

// systemScope 判断是否使用系统级 unit：以 root 运行时为 true；测试可替换。
var systemScope = func() bool { return os.Geteuid() == 0 }

// userRuntimeDir 返回当前用户的 XDG 运行目录；环境变量缺失时回退 /run/user/<uid>（存在才用）。测试可替换。
var userRuntimeDir = func() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return ""
	}
	d := "/run/user/" + strconv.Itoa(os.Getuid())
	if fi, err := os.Stat(d); err == nil && fi.IsDir() {
		return d
	}
	return ""
}

// systemctl 按当前作用域调用 systemctl：系统级不带 --user，用户级带 --user。
// 用户级且进程环境缺少 XDG_RUNTIME_DIR（如非登录会话拉起）时，经 env 补上运行目录，
// 否则 systemctl 无法连接 user bus。
func systemctl(args ...string) (string, error) {
	if systemScope() {
		return run("systemctl", args...)
	}
	userArgs := append([]string{"--user"}, args...)
	if d := userRuntimeDir(); d != "" {
		return run("env", append([]string{"XDG_RUNTIME_DIR=" + d, "systemctl"}, userArgs...)...)
	}
	return run("systemctl", userArgs...)
}

// scopeLabel 返回用于状态提示的 unit 作用域描述。
func scopeLabel() string {
	if systemScope() {
		return "systemd system unit"
	}
	return "systemd user unit"
}

func unitPath() (string, error) {
	if systemScope() {
		return filepath.Join(systemUnitDir, unitName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", unitName), nil
}

func on(opt Options) error {
	path, err := unitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	unit := RenderUnit(opt.Exe, opt.ConfigPath)
	if systemScope() {
		unit = RenderSystemUnit(opt.Exe, opt.ConfigPath)
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return err
	}
	if _, err := systemctl("daemon-reload"); err != nil {
		return err
	}
	_, err = systemctl("enable", "--now", unitName)
	return err
}

func off() error {
	path, err := unitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		// 只有「文件不存在」才算未开启；权限等真实故障必须上报，
		// 否则 unit 仍在却报告移除成功。
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// 只 disable 不 --now：正在运行的实例继续跑，重启后不再拉起。
	if _, err := systemctl("disable", unitName); err != nil {
		return err
	}
	_ = os.Remove(path)
	_, err = systemctl("daemon-reload")
	return err
}

func status() (bool, error) {
	path, err := unitPath()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// RootDaemonPlistInstalled 在 Linux 上恒为 false（无 root LaunchDaemon 概念）。
func RootDaemonPlistInstalled() bool { return false }
