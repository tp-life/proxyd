package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
	"proxyd/internal/proxy/subscribe"
)

// TestUpdateSubscriptionRenameOnlySkipsFetch 验证启用中的订阅在 URL/类型未变化时改名
// 不触发网络拉取与健康检查：节点存活状态原样保留并迁移到新名称，用量缓存跟随迁移，
// 即使节点此刻全部失效也能改名成功。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于隔离配置/状态目录并报告断言失败。
//
// 返回值：无。
//
// 错误情况：订阅 URL 指向不可达地址，若实现仍尝试拉取则本测试会因超时或报错而失败；
// 节点未改名、健康状态丢失、用量信息未迁移或磁盘配置未提交时同样失败。
func TestUpdateSubscriptionRenameOnlySkipsFetch(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Subscriptions: []config.Subscription{
			// 故意使用不可达地址：改名路径不允许发生任何真实拉取；
			// Type 留空模拟旧配置缺省（等价 auto），覆盖归一化比较
			{Name: "old", URL: "http://127.0.0.1:1/unreachable"},
		},
		Listen:    "127.0.0.1",
		PortRange: [2]int{42000, 42010},
		Mode:      "rule",
		LogLevel:  "silent",
		StateDir:  t.TempDir(),
		Rules:     []string{"MATCH,PROXY"},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("保存初始配置失败: %v", err)
	}
	a, err := New(cfg, cfgPath)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)
	deadNode := subscriptionTestNode("失效节点", "old", "10.0.0.1")
	deadNode.Alive = false
	deadNode.Delay = 0
	a.nodes = []*node.Node{deadNode}
	a.subInfos["old"] = subscribe.UserInfo{Total: 1024, Expire: 1893456000}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	next, err := a.UpdateSubscription(ctx, "old", config.Subscription{
		Name: "new", URL: "http://127.0.0.1:1/unreachable", Type: "auto",
	})
	if err != nil {
		t.Fatalf("纯改名不应触发拉取，应直接成功: %v", err)
	}
	if next.Name != "new" {
		t.Fatalf("返回订阅名异常: %+v", next)
	}
	if len(a.nodes) != 1 || a.nodes[0].Subscription != "new" {
		t.Fatalf("节点未迁移到新订阅名: %+v", a.nodes)
	}
	if a.nodes[0].Alive {
		t.Fatal("复用路径不应重新测速，节点存活状态应保持原值")
	}
	if a.subInfos["new"].Total != 1024 {
		t.Fatalf("用量缓存未跟随改名迁移: %+v", a.subInfos)
	}
	if _, ok := a.subInfos["old"]; ok {
		t.Fatal("旧名称的用量缓存未清理")
	}
	onDisk, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("读取提交后的配置失败: %v", err)
	}
	if len(onDisk.Subscriptions) != 1 || onDisk.Subscriptions[0].Name != "new" {
		t.Fatalf("磁盘订阅名未提交: %+v", onDisk.Subscriptions)
	}
}
