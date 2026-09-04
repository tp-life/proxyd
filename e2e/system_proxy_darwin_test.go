//go:build darwin

package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"proxyd/internal/config"
	"proxyd/internal/proxy/sysproxy"
)

// testSystemProxyLifecycle 验证 macOS 系统代理启停、配置持久化和主端口热重绑。
//
// 参数：
//   - t: *testing.T，记录断言并注册清理逻辑的测试上下文。
//   - base: string，当前 e2e API 服务的基础 URL。
//   - cfgPath: string，运行中配置文件路径，用于验证 system-proxy 已持久化。
//   - mixedPort: int，启用系统代理时预期绑定的主端口。
//   - newLo: int，节点端口新区间下界，用于避开端口冲突。
//
// 返回值：无；所有验证结果通过 testing.T 报告。
//
// 错误情况：无法读取系统代理快照时安全跳过该平台场景；API 请求、状态断言或持久化
// 校验失败时测试失败。函数始终通过 defer 尝试恢复原始 networksetup 状态，避免污染宿主机。
func testSystemProxyLifecycle(t *testing.T, base, cfgPath string, mixedPort, newLo int) {
	t.Helper()

	/*
		真实 system proxy 测试会修改所有活动网络服务，必须先获得完整快照。
		快照失败时不继续写系统设置；成功后立即注册恢复，覆盖后续任一断言提前退出的路径。
	*/
	snapshot, err := sysproxy.Snapshot()
	if err != nil {
		t.Logf("sysproxy snapshot 失败，跳过: %v", err)
		return
	}
	defer func() {
		if err := sysproxy.Restore(snapshot); err != nil {
			t.Errorf("恢复系统代理快照失败: %v", err)
		}
	}()

	resp, err := http.Post(base+"/api/system-proxy", "application/json", strings.NewReader(`{"enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("system-proxy on: status=%d", resp.StatusCode)
	}
	if on, _ := sysproxy.Status("127.0.0.1", mixedPort); !on {
		t.Error("系统代理未生效")
	}

	saved, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("读取系统代理持久化配置失败: %v", err)
	}
	if !saved.SystemProxy {
		t.Error("system-proxy 未持久化")
	}

	/*
		主端口变更必须同时重绑系统代理。候选端口需要避开节点映射新区间和原主端口，
		否则端口占用会让测试测到监听冲突，而不是系统代理调和规则。
	*/
	rebindPort := freePort(t)
	for rebindPort >= newLo && rebindPort <= newLo+1 || rebindPort == mixedPort {
		rebindPort = freePort(t)
	}
	resp, err = http.Post(base+"/api/main-port", "application/json",
		strings.NewReader(fmt.Sprintf(`{"port":%d}`, rebindPort)))
	if err != nil {
		t.Fatalf("系统代理开启时修改主端口失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("main-port（系统代理开启中）: status=%d", resp.StatusCode)
	}
	if on, _ := sysproxy.Status("127.0.0.1", rebindPort); !on {
		t.Error("系统代理未跟随主端口重绑")
	}

	resp, err = http.Post(base+"/api/system-proxy", "application/json", strings.NewReader(`{"enabled":false}`))
	if err != nil {
		t.Fatalf("关闭系统代理失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("system-proxy off: status=%d", resp.StatusCode)
	}
	if on, _ := sysproxy.Status("127.0.0.1", mixedPort); on {
		t.Error("系统代理未关闭")
	}
}
