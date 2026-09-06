package remote

// DERP 回归测试通过本地 HTTP 服务模拟地图 DNS 故障，不依赖公网可用性。
import (
	"context"
	"net/http"
	"net/http/httptest"
	"proxyd/internal/config"
	"testing"
	"time"

	"tailscale.com/tailcfg"
)

// TestDERPDiscoveryFallback 验证首来源不可达时仍能从备用地图恢复完整区域。
// 参数：t 为 *testing.T；返回无；回退失败、节点丢失时测试失败。固定编号避免公网延迟探测。
func TestDERPDiscoveryFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Regions":{"1":{"RegionID":1,"Nodes":[{"Name":"local","RegionID":1,"HostName":"relay.example.com"}]}}}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	region, err := discoverDERPRegion(ctx, 1, []string{"http://derp-unavailable.invalid/map", server.URL})
	if err != nil || region == nil || region.RegionID != 1 {
		t.Fatalf("默认来源失败后未恢复: region=%v err=%v", region, err)
	}
}

// TestDERPRegionSurvivesRestart 验证重启复用完整区域且自定义来源变化不会误用旧缓存。
// 参数：t 为 *testing.T；返回无；缓存损坏或隔离规则失败则报告错误。
func TestDERPRegionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, nil)
	region := &tailcfg.DERPRegion{RegionID: 1, Nodes: []*tailcfg.DERPNode{{HostName: "relay.example.com"}}}
	if err := m.saveDERPRegion("", region); err != nil {
		t.Fatal(err)
	}
	fresh := NewManager(dir, nil)
	if got, err := fresh.autoRegionLocked(config.RemoteConfig{}); err != nil || got == nil || got.Nodes[0].HostName != region.Nodes[0].HostName {
		t.Fatal("重启后区域未恢复")
	}
	if fresh.loadDERPRegion("https://custom.example/map") != nil {
		t.Fatal("自定义地图复用了公共区域")
	}
}
