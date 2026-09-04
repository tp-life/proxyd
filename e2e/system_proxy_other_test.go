//go:build !darwin

package e2e

import "testing"

// testSystemProxyLifecycle 在非 macOS 平台跳过依赖 networksetup 的真实系统代理场景。
//
// 参数：
//   - t: *testing.T，保持与 Darwin 实现一致的测试上下文。
//   - base: string，e2e API 基础 URL；非 macOS 不使用。
//   - cfgPath: string，配置文件路径；非 macOS 不使用。
//   - mixedPort: int，主端口；非 macOS 不使用。
//   - newLo: int，节点端口新区间下界；非 macOS 不使用。
//
// 返回值：无；非 macOS 不修改宿主机系统代理。
//
// 错误情况：无。Linux 和 Windows 的系统代理依赖桌面环境或注册表，不应在通用 CI
// 主机上执行真实全局设置；各平台生产适配器由其自身单元测试覆盖。
func testSystemProxyLifecycle(t *testing.T, base, cfgPath string, mixedPort, newLo int) {
	t.Helper()
}
