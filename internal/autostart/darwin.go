//go:build darwin

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// macOS 实现：LaunchAgents plist，launchd 直接托管 serve 前台进程
//（RunAtLoad + KeepAlive，崩溃自动拉起；日志落 state-dir/proxyd.log）。

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", plistLabel+".plist"), nil
}

func on(opt Options) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(opt.StateDir, 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(opt.StateDir, "proxyd.log")
	if err := os.WriteFile(path, []byte(RenderPlist(opt.Exe, opt.ConfigPath, logPath)), 0o644); err != nil {
		return err
	}
	// 立即加载启动；bootstrap 失败时退回旧的 load -w
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if _, err := run("launchctl", "bootstrap", domain, path); err != nil {
		// 已加载过时 bootstrap 会报错，先 bootout 再试
		_, _ = run("launchctl", "bootout", domain, path)
		if _, err2 := run("launchctl", "bootstrap", domain, path); err2 != nil {
			if _, err3 := run("launchctl", "load", "-w", path); err3 != nil {
				return fmt.Errorf("launchctl 加载失败: %v / %v", err2, err3)
			}
		}
	}
	return nil
}

func off() error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return nil // 不存在，视为已关闭
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_, _ = run("launchctl", "bootout", domain, path) // 未加载时报错可忽略
	_, _ = run("launchctl", "unload", "-w", path)
	return os.Remove(path)
}

func status() (bool, error) {
	path, err := plistPath()
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	return err == nil, nil
}
