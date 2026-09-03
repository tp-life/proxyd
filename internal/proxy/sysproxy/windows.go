//go:build windows

package sysproxy

import (
	"fmt"
	"strconv"
	"strings"
)

// Windows 实现：HKCU\...\Internet Settings 注册表，best-effort。

const regKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`

func on(host string, port int) error {
	if _, err := run("reg", "add", regKey, "/v", "ProxyServer", "/t", "REG_SZ", "/d", host+":"+strconv.Itoa(port), "/f"); err != nil {
		return err
	}
	_, err := run("reg", "add", regKey, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "1", "/f")
	return err
}

func off() error {
	_, err := run("reg", "add", regKey, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "0", "/f")
	return err
}

func status(host string, port int) (bool, error) {
	out, err := run("reg", "query", regKey, "/v", "ProxyEnable")
	if err != nil {
		return false, err
	}
	if !strings.Contains(out, "0x1") {
		return false, nil
	}
	srv, err := run("reg", "query", regKey, "/v", "ProxyServer")
	if err != nil {
		return false, nil
	}
	return strings.Contains(srv, fmt.Sprintf("%s:%d", host, port)), nil
}
