package inbound

import (
	"net"
	"net/netip"
	"slices"
	"sync"

	C "github.com/metacubex/mihomo/constant"
)

// 认证前缀由热更新写入、监听协程读取，锁覆盖全部访问，快照复制隔离外部修改。
var skipAuthPrefixes []netip.Prefix
var skipAuthMu sync.RWMutex

// SetSkipAuthPrefixes 发布认证豁免前缀；参数为前缀切片；无返回、无错误，复制后持锁替换。
func SetSkipAuthPrefixes(prefixes []netip.Prefix) {
	skipAuthMu.Lock()
	defer skipAuthMu.Unlock()
	skipAuthPrefixes = slices.Clone(prefixes)
}

// SkipAuthPrefixes 读取独立前缀快照；无参数，返回切片；无错误，调用者修改不影响授权。
func SkipAuthPrefixes() []netip.Prefix {
	skipAuthMu.RLock()
	defer skipAuthMu.RUnlock()
	return slices.Clone(skipAuthPrefixes)
}

func SkipAuthRemoteAddr(addr net.Addr) bool {
	m := C.Metadata{}
	if err := m.SetRemoteAddr(addr); err != nil {
		return false
	}
	return skipAuth(m.AddrPort().Addr())
}

func SkipAuthRemoteAddress(addr string) bool {
	m := C.Metadata{}
	if err := m.SetRemoteAddress(addr); err != nil {
		return false
	}
	return skipAuth(m.AddrPort().Addr())
}

// skipAuth 判断源地址是否豁免认证；参数为 IP 地址，返回 bool；锁保护热更新期间的读取。
func skipAuth(addr netip.Addr) bool {
	skipAuthMu.RLock()
	defer skipAuthMu.RUnlock()
	return prefixesContains(skipAuthPrefixes, addr)
}
