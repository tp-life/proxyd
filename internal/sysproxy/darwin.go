//go:build darwin

package sysproxy

import (
	"fmt"
	"strconv"
	"strings"
)

// macOS 实现：networksetup。对每个活动网络服务设置 web/secureweb/socks 代理。

// ProxyState 是单个网络服务单个协议的代理配置快照（供备份/恢复）。
type ProxyState struct {
	Service  string
	Protocol string // "web" | "secureweb" | "socks"
	Enabled  bool
	Server   string
	Port     int
}

// 协议名到 networksetup 子命令基名的映射。
var protoCmd = map[string]string{
	"web":       "webproxy",
	"secureweb": "securewebproxy",
	"socks":     "socksfirewallproxy",
}

// activeServices 返回活动的网络服务列表（跳过标题行与 * 标记的停用服务）。
func activeServices() ([]string, error) {
	out, err := run("networksetup", "-listallnetworkservices")
	if err != nil {
		return nil, err
	}
	var svcs []string
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || i == 0 || strings.HasPrefix(line, "*") {
			continue // 首行是说明文字
		}
		svcs = append(svcs, line)
	}
	return svcs, nil
}

func on(host string, port int) error {
	svcs, err := activeServices()
	if err != nil {
		return err
	}
	p := strconv.Itoa(port)
	var errs []string
	for _, s := range svcs {
		for _, proto := range []string{"web", "secureweb", "socks"} {
			if _, err := run("networksetup", "-set"+protoCmd[proto], s, host, p); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("部分服务设置失败: %s", strings.Join(errs, "; "))
	}
	return nil
}

func off() error {
	svcs, err := activeServices()
	if err != nil {
		return err
	}
	var errs []string
	for _, s := range svcs {
		for _, proto := range []string{"web", "secureweb", "socks"} {
			if _, err := run("networksetup", "-set"+protoCmd[proto]+"state", s, "off"); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("部分服务关闭失败: %s", strings.Join(errs, "; "))
	}
	return nil
}

func status(host string, port int) (bool, error) {
	svcs, err := activeServices()
	if err != nil {
		return false, err
	}
	for _, s := range svcs {
		st, err := getProxy(s, "web")
		if err != nil {
			continue
		}
		if st.Enabled && st.Server == host && st.Port == port {
			return true, nil
		}
	}
	return false, nil
}

// getProxy 读取单个服务单个协议的代理配置（解析 -getXXXproxy 输出）。
func getProxy(service, proto string) (ProxyState, error) {
	st := ProxyState{Service: service, Protocol: proto}
	out, err := run("networksetup", "-get"+protoCmd[proto], service)
	if err != nil {
		return st, err
	}
	for line := range strings.Lines(out) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Enabled":
			st.Enabled = strings.EqualFold(strings.TrimSpace(v), "yes")
		case "Server":
			st.Server = strings.TrimSpace(v)
		case "Port":
			st.Port, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	return st, nil
}

// Snapshot 备份所有活动服务的 web/secureweb/socks 代理配置（供测试/恢复用）。
func Snapshot() ([]ProxyState, error) {
	svcs, err := activeServices()
	if err != nil {
		return nil, err
	}
	var out []ProxyState
	for _, s := range svcs {
		for _, proto := range []string{"web", "secureweb", "socks"} {
			st, err := getProxy(s, proto)
			if err != nil {
				return out, err
			}
			out = append(out, st)
		}
	}
	return out, nil
}

// Restore 按 Snapshot 的结果恢复各服务代理配置。
func Restore(snaps []ProxyState) error {
	var errs []string
	for _, st := range snaps {
		base := protoCmd[st.Protocol]
		if st.Enabled {
			if _, err := run("networksetup", "-set"+base, st.Service, st.Server, strconv.Itoa(st.Port)); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			if _, err := run("networksetup", "-set"+base+"state", st.Service, "on"); err != nil {
				errs = append(errs, err.Error())
			}
		} else {
			if _, err := run("networksetup", "-set"+base+"state", st.Service, "off"); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("部分服务恢复失败: %s", strings.Join(errs, "; "))
	}
	return nil
}
