package subscribe

// 订阅草稿差异领域测试：验证身份比较、健康变化和凭据隔离。

import (
	"encoding/json"
	"strings"
	"testing"

	"proxyd/internal/proxy/node"
)

// TestCompareDraftNodes 验证新增、删除、更新与未变化节点的分类和健康统计。
//
// 参数：t 为 Go 测试上下文。
// 返回值：无。
// 错误情况：任一计数、排序或序列化结果不符合领域规则时测试失败。
func TestCompareDraftNodes(t *testing.T) {
	before := []*node.Node{
		draftTestNode("保留", "a.example", "secret-a", true, 20),
		draftTestNode("改名之前", "b.example", "secret-b", true, 30),
		draftTestNode("将删除", "c.example", "secret-c", false, 0),
	}
	after := []*node.Node{
		draftTestNode("保留", "a.example", "secret-a", true, 20),
		draftTestNode("改名之后", "b.example", "secret-b", false, 0),
		draftTestNode("新增", "d.example", "secret-d", true, 10),
	}

	diff := CompareDraftNodes(before, after)
	if diff.BeforeTotal != 3 || diff.AfterTotal != 3 || diff.BeforeAlive != 2 || diff.AfterAlive != 2 {
		t.Fatalf("总量或健康统计错误: %+v", diff)
	}
	if diff.Added != 1 || diff.Removed != 1 || diff.Updated != 1 || diff.Unchanged != 1 {
		t.Fatalf("变化分类错误: %+v", diff)
	}
	encoded, err := json.Marshal(diff)
	if err != nil {
		t.Fatalf("序列化差异失败: %v", err)
	}
	if strings.Contains(string(encoded), "secret-") || strings.Contains(string(encoded), "example") {
		t.Fatalf("差异响应泄露稳定身份或凭据: %s", encoded)
	}
}

// draftTestNode 构造带稳定身份和健康状态的测试节点。
//
// 参数：name/server/password/alive/delay 分别为展示名、服务器、凭据、健康状态和延迟。
// 返回值：*node.Node，可直接参与差异比较。
// 错误情况：无；测试输入固定合法。
func draftTestNode(name, server, password string, alive bool, delay uint16) *node.Node {
	return &node.Node{
		Name: name, Subscription: "测试订阅", Alive: alive, Delay: delay,
		Mapping: map[string]any{"name": name, "type": "ss", "server": server, "port": 443, "password": password},
	}
}
