package core

// 此回归在根模块验证替换后的 mihomo 日志同步；竞态检测必须覆盖依赖后台读路径。
import (
	inbound "github.com/metacubex/mihomo/adapter/inbound"
	"net"
	"net/netip"
	"sync"
	"testing"

	mihomolog "github.com/metacubex/mihomo/log"
)

// TestMihomoLogLevelConcurrent 验证级别写入与事件输出同时执行；参数 t 为测试对象；无返回，由 race 检测器报告非原子访问。
func TestMihomoLogLevelConcurrent(t *testing.T) {
	previous := mihomolog.Level()
	defer mihomolog.SetLevel(previous)
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			for range 200 {
				if index%2 == 0 {
					mihomolog.SetLevel(mihomolog.SILENT)
				} else {
					_ = mihomolog.Level()
					mihomolog.Debugln("并发日志回归")
				}
			}
		}(worker)
	}
	workers.Wait()
}

// TestMihomoInboundPolicyConcurrent 验证热更新与连接认证并发时同步，且配置快照无法绕过授权。
// 参数 t 为测试对象；无返回，策略数据竞争或读取方能修改共享授权时失败。
func TestMihomoInboundPolicyConcurrent(t *testing.T) {
	oldSkip, oldAllowed, oldDenied := inbound.SkipAuthPrefixes(), inbound.AllowedIPs(), inbound.DisAllowedIPs()
	defer inbound.SetSkipAuthPrefixes(oldSkip)
	defer inbound.SetAllowedIPs(oldAllowed)
	defer inbound.SetDisAllowedIPs(oldDenied)
	prefixes := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	inbound.SetSkipAuthPrefixes(prefixes)
	snapshot := inbound.SkipAuthPrefixes()
	snapshot[0] = netip.MustParsePrefix("10.0.0.0/8")
	if !inbound.SkipAuthRemoteAddr(addr) {
		t.Fatal("快照修改影响了共享认证策略")
	}
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			for range 100 {
				if index%2 == 0 {
					inbound.SetSkipAuthPrefixes(prefixes)
					inbound.SetAllowedIPs(prefixes)
					inbound.SetDisAllowedIPs(nil)
				} else {
					_ = inbound.SkipAuthRemoteAddr(addr)
					_ = inbound.IsRemoteAddrDisAllowed(addr)
					_ = inbound.AllowedIPs()
					_ = inbound.DisAllowedIPs()
				}
			}
		}(worker)
	}
	workers.Wait()
}
