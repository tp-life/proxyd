//go:build linux

package autostart

import (
	"os"
	"path/filepath"
)

// Linux 实现：systemd user unit（enable --now 立即启动；日志走 journalctl --user）。

const unitName = "proxyd.service"

func unitPath() (string, error) {
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
	if err := os.WriteFile(path, []byte(RenderUnit(opt.Exe, opt.ConfigPath)), 0o644); err != nil {
		return err
	}
	if _, err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	_, err = run("systemctl", "--user", "enable", "--now", unitName)
	return err
}

func off() error {
	path, err := unitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	// 只 disable 不 --now：正在运行的实例继续跑，重启后不再拉起。
	if _, err := run("systemctl", "--user", "disable", unitName); err != nil {
		return err
	}
	_ = os.Remove(path)
	_, err = run("systemctl", "--user", "daemon-reload")
	return err
}

func status() (bool, error) {
	path, err := unitPath()
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	return err == nil, nil
}
