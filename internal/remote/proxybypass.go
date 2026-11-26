package remote

// 本文件隔离 tailcat 与本机 proxyd 代理端口的自环。
//
// tailcat v0.8.0 起默认接入 tailscale.com/feature/useproxy：当环境变量或系统代理
// 指向本机 proxyd 的 mixed-port/auto-port 时，DERP 连接会先绕进本机代理再出国，
// 造成“隧道套代理（TCP-over-TCP）”与高延迟抖动。通过 tshttpproxy.SetSelfProxy
// 声明本机代理监听地址，让 tailcat 直连 DERP 中继而不回环本进程。

import (
	"net"
	"strconv"
	"strings"

	"tailscale.com/net/tshttpproxy"
)

// BypassSelfProxy 向 tailcat 底层 HTTP 代理探测器登记本进程自身的代理监听地址，
// 避免 tailcat 的 DERP 中继连接绕回 proxyd 自己的代理端口。
//
// 参数说明：
//   - listen: string，proxyd 代理监听主机（如 127.0.0.1 或 0.0.0.0）。
//   - ports: ...int，本进程开放的本地代理端口（如 mixed-port、auto-port）。
//
// 返回值说明：无；零或越界端口自动忽略。
//
// 错误情况：无；所有地址均在本地规范化，不执行网络 I/O。
func BypassSelfProxy(listen string, ports ...int) {
	host := strings.TrimSpace(listen)
	addrs := make([]string, 0, len(ports)*2)
	for _, port := range ports {
		if port <= 0 || port > 65535 {
			continue
		}
		portStr := strconv.Itoa(port)
		addrs = append(addrs, net.JoinHostPort("127.0.0.1", portStr))
		if host != "" && host != "127.0.0.1" && host != "0.0.0.0" && host != "::" && host != "::1" {
			addrs = append(addrs, net.JoinHostPort(host, portStr))
		}
	}
	if len(addrs) == 0 {
		return
	}
	tshttpproxy.SetSelfProxy(addrs...)
	tshttpproxy.InvalidateCache()
}
