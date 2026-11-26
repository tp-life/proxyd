//go:build windows

package autostart

import (
	"strings"
)

// Windows 实现：HKCU\...\Run 注册表项。登录时执行 `proxyd start`（派生
// detached 后台 serve 进程后退出），避免常驻控制台窗口。

const regRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
const regValueName = "proxyd"

func on(opt Options) error {
	_, err := run("reg", "add", regRunKey, "/v", regValueName, "/t", "REG_SZ",
		"/d", RenderRunCommand(opt.Exe, opt.ConfigPath), "/f")
	return err
}

func off() error {
	_, err := run("reg", "delete", regRunKey, "/v", regValueName, "/f")
	if err != nil && strings.Contains(err.Error(), "unable to find") {
		return nil
	}
	return err
}

func status() (bool, error) {
	_, err := run("reg", "query", regRunKey, "/v", regValueName)
	if err != nil {
		if strings.Contains(err.Error(), "unable to find") || strings.Contains(err.Error(), "ERROR") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
