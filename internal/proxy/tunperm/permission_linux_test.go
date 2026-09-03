//go:build linux

package tunperm

import "testing"

// TestHasLinuxCapability 验证 CapEff 位图只在 CAP_NET_ADMIN 位生效时放行。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：位图解析或边界判断不符合预期时通过 t.Errorf/t.Error 报告。
func TestHasLinuxCapability(t *testing.T) {
	withNetAdmin := "Name:\tproxyd\nCapEff:\t0000000000001000\n"
	if !hasLinuxCapability(withNetAdmin, linuxCapNetAdmin) {
		t.Error("CAP_NET_ADMIN 位已设置但检测结果为 false")
	}
	withoutNetAdmin := "Name:\tproxyd\nCapEff:\t0000000000000000\n"
	if hasLinuxCapability(withoutNetAdmin, linuxCapNetAdmin) {
		t.Error("CAP_NET_ADMIN 位未设置但检测结果为 true")
	}
	if hasLinuxCapability("CapEff:\tnot-hex\n", linuxCapNetAdmin) {
		t.Error("损坏的 CapEff 不应被判定为可用")
	}
}
