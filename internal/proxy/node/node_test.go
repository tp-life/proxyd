// Package node 提供代理节点领域模型的单元测试。
package node

import "testing"

// TestNodeKeyIncludesDialerProxy 验证链式拨号目标属于节点稳定身份，同时普通节点
// 继续使用历史 Key 格式，避免升级后丢失已有端口快照。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：不同链路被生成相同 Key，或普通节点 Key 发生兼容性变化时测试失败。
func TestNodeKeyIncludesDialerProxy(t *testing.T) {
	mapping := map[string]any{
		"type": "socks5", "server": "127.0.0.1", "port": 1080, "password": "secret",
	}
	plain := &Node{Mapping: mapping}
	if got := plain.Key(); got != "socks5|127.0.0.1|1080|secret" {
		t.Fatalf("普通节点 Key 兼容性被破坏: %q", got)
	}

	viaA := &Node{Mapping: map[string]any{
		"type": "socks5", "server": "127.0.0.1", "port": 1080,
		"password": "secret", "dialer-proxy": "入口 A",
	}}
	viaB := &Node{Mapping: map[string]any{
		"type": "socks5", "server": "127.0.0.1", "port": 1080,
		"password": "secret", "dialer-proxy": "入口 B",
	}}
	if viaA.Key() == viaB.Key() {
		t.Fatalf("不同 dialer-proxy 的节点不应被去重: %q", viaA.Key())
	}
}

// TestNodeDisplayRoundTrip 验证展示行的发布语义：标记保留轮前稳定值，
// 结果发布反映权威字段，未发布过的节点返回零值行。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无；通过展示行字段断言表达结果。
//
// 错误情况：标记后丢失轮前值、结果发布没有跟随权威字段，或未发布节点回落到直接
// 读取权威字段（会与健康检测 worker 的写回竞争）时测试失败。
func TestNodeDisplayRoundTrip(t *testing.T) {
	n := &Node{Name: "n", Alive: true, Delay: 42}
	if d := n.Display(); d.Alive || d.Delay != 0 || d.Testing {
		t.Fatalf("未发布展示行的节点应返回零值行: %+v", d)
	}

	n.MarkTesting()
	if d := n.Display(); !d.Testing || !d.Alive || d.Delay != 42 {
		t.Fatalf("标记应保留本轮开始前的稳定值: %+v", d)
	}

	// 权威字段被探测改写后，展示行必须仍停在轮前值，直到显式发布结果。
	n.Alive = false
	n.Delay = 0
	if d := n.Display(); !d.Testing || !d.Alive || d.Delay != 42 {
		t.Fatalf("结果发布前展示行不应跟随权威字段: %+v", d)
	}

	n.Alive = true
	n.Delay = 88
	n.PublishResult()
	if d := n.Display(); d.Testing || !d.Alive || d.Delay != 88 {
		t.Fatalf("结果发布应反映权威字段并清除测速标记: %+v", d)
	}

	// PublishDisplay 只改展示行，不动权威字段：单订阅测速的增量转发依赖这一语义。
	n.PublishDisplay(Display{Alive: true, Delay: 7, Testing: true})
	if d := n.Display(); !d.Testing || d.Delay != 7 {
		t.Fatalf("PublishDisplay 应原样发布给定行: %+v", d)
	}
	if n.Delay != 88 {
		t.Fatalf("PublishDisplay 不应修改权威字段: delay=%d", n.Delay)
	}
}

// TestNodeCloneIsIndependent 验证 Clone 的字段独立性与展示行继承。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无；通过副本与源的字段对比断言表达结果。
//
// 错误情况：副本修改影响源节点、展示行未继承或 nil 接收者 panic 时测试失败。
func TestNodeCloneIsIndependent(t *testing.T) {
	source := &Node{
		Name:         "源",
		Subscription: "sub",
		Mapping:      map[string]any{"name": "源", "type": "socks5"},
		Alive:        true,
		Delay:        21,
	}
	source.MarkTesting()

	cloned := source.Clone()
	if cloned.Name != source.Name || cloned.Subscription != source.Subscription || cloned.Delay != source.Delay {
		t.Fatalf("副本字段应与源一致: %+v", cloned)
	}
	if d := cloned.Display(); !d.Testing || !d.Alive || d.Delay != 21 {
		t.Fatalf("副本应继承展示行: %+v", d)
	}

	cloned.Alive = false
	cloned.Delay = 0
	cloned.PublishResult()
	if source.Alive != true || source.Delay != 21 {
		t.Fatalf("修改副本不应影响源节点: alive=%v delay=%d", source.Alive, source.Delay)
	}
	if d := source.Display(); !d.Testing || d.Delay != 21 {
		t.Fatalf("源的展示行不应被副本发布覆盖: %+v", d)
	}

	var nilNode *Node
	if nilNode.Clone() != nil {
		t.Fatal("nil 节点克隆应返回 nil")
	}
	if d := nilNode.Display(); d != (Display{}) {
		t.Fatalf("nil 节点展示行应为零值: %+v", d)
	}
}
