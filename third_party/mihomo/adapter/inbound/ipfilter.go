package inbound

import (
	"net"
	"net/netip"
	"slices"
	"sync"

	C "github.com/metacubex/mihomo/constant"
)

// 监听协程与热更新共享过滤策略，读写均持锁；外部只能取得副本。
var ipFilterMu sync.RWMutex
var lanAllowedIPs []netip.Prefix
var lanDisAllowedIPs []netip.Prefix

// SetAllowedIPs 更新 IP 过滤前缀；参数为前缀切片；无返回、无错误，复制后持锁发布。
func SetAllowedIPs(prefixes []netip.Prefix) {
	ipFilterMu.Lock()
	defer ipFilterMu.Unlock()
	lanAllowedIPs = slices.Clone(prefixes)
}

// SetDisAllowedIPs 更新 IP 过滤前缀；参数为前缀切片；无返回、无错误，复制后持锁发布。
func SetDisAllowedIPs(prefixes []netip.Prefix) {
	ipFilterMu.Lock()
	defer ipFilterMu.Unlock()
	lanDisAllowedIPs = slices.Clone(prefixes)
}

// AllowedIPs 返回独立策略快照；无参数，返回前缀切片；无错误，避免外部修改共享数组。
func AllowedIPs() []netip.Prefix {
	ipFilterMu.RLock()
	defer ipFilterMu.RUnlock()
	return slices.Clone(lanAllowedIPs)
}

// DisAllowedIPs 返回独立策略快照；无参数，返回前缀切片；无错误，避免外部修改共享数组。
func DisAllowedIPs() []netip.Prefix {
	ipFilterMu.RLock()
	defer ipFilterMu.RUnlock()
	return slices.Clone(lanDisAllowedIPs)
}

func IsRemoteAddrDisAllowed(addr net.Addr) bool {
	m := C.Metadata{}
	if err := m.SetRemoteAddr(addr); err != nil {
		return false
	}
	ipAddr := m.AddrPort().Addr()
	if ipAddr.IsValid() {
		return isAllowed(ipAddr) && !isDisAllowed(ipAddr)
	}
	return false
}

// isAllowed 判断地址是否匹配前缀；参数为 IP，返回 bool；无错误，与配置写入同步。
func isAllowed(addr netip.Addr) bool {
	ipFilterMu.RLock()
	defer ipFilterMu.RUnlock()
	return prefixesContains(lanAllowedIPs, addr)
}

// isDisAllowed 判断地址是否匹配前缀；参数为 IP，返回 bool；无错误，与配置写入同步。
func isDisAllowed(addr netip.Addr) bool {
	ipFilterMu.RLock()
	defer ipFilterMu.RUnlock()
	return prefixesContains(lanDisAllowedIPs, addr)
}
