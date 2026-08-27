//go:build linux

package sysproxy

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Linux 实现：gsettings（GNOME），best-effort；无 gsettings 时报不支持。

func checkGSettings() error {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return fmt.Errorf("%w（未找到 gsettings，仅支持 GNOME 桌面）", ErrUnsupported)
	}
	return nil
}

func on(host string, port int) error {
	if err := checkGSettings(); err != nil {
		return err
	}
	p := strconv.Itoa(port)
	cmds := [][]string{
		{"set", "org.gnome.system.proxy", "mode", "manual"},
		{"set", "org.gnome.system.proxy.http", "host", host},
		{"set", "org.gnome.system.proxy.http", "port", p},
		{"set", "org.gnome.system.proxy.https", "host", host},
		{"set", "org.gnome.system.proxy.https", "port", p},
		{"set", "org.gnome.system.proxy.socks", "host", host},
		{"set", "org.gnome.system.proxy.socks", "port", p},
	}
	for _, args := range cmds {
		if _, err := run("gsettings", args...); err != nil {
			return err
		}
	}
	return nil
}

func off() error {
	if err := checkGSettings(); err != nil {
		return err
	}
	_, err := run("gsettings", "set", "org.gnome.system.proxy", "mode", "none")
	return err
}

func status(host string, port int) (bool, error) {
	if err := checkGSettings(); err != nil {
		return false, err
	}
	mode, err := run("gsettings", "get", "org.gnome.system.proxy", "mode")
	if err != nil {
		return false, err
	}
	if !strings.Contains(mode, "manual") {
		return false, nil
	}
	h, _ := run("gsettings", "get", "org.gnome.system.proxy.http", "host")
	p, _ := run("gsettings", "get", "org.gnome.system.proxy.http", "port")
	return strings.Contains(h, host) && strings.TrimSpace(p) == strconv.Itoa(port), nil
}
