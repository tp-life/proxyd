package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
)

// TestNeedsMihomoRuntimeProbe 验证二阶段探测边界：普通链式节点与 Tailscale Exit Node
// 必须通过正式 mihomo 代理表测速，仅访问 Tailnet/子网路由的 Tailscale 不得用公网
// health-url 判死。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无；通过表驱动断言表达结果。
//
// 错误情况：nil/失效节点被探测、链式节点漏测，或无 Exit Node 的 Tailscale 被误测时失败。
func TestNeedsMihomoRuntimeProbe(t *testing.T) {
	tests := []struct {
		name string
		node *node.Node
		want bool
	}{
		{name: "nil", node: nil, want: false},
		{name: "普通直连节点", node: &node.Node{Alive: true, Mapping: map[string]any{"type": "socks5"}}, want: false},
		{name: "普通链式节点", node: &node.Node{Alive: true, Mapping: map[string]any{"type": "socks5", "dialer-proxy": "上游"}}, want: true},
		{name: "失效链式节点", node: &node.Node{Alive: false, Mapping: map[string]any{"type": "socks5", "dialer-proxy": "上游"}}, want: false},
		{name: "Tailnet 节点", node: &node.Node{Alive: true, Mapping: map[string]any{"type": "tailscale", "auth-key": "k"}}, want: false},
		{name: "Tailnet 链式节点", node: &node.Node{Alive: true, Mapping: map[string]any{"type": "tailscale", "auth-key": "k", "dialer-proxy": "上游"}}, want: false},
		{name: "Exit Node", node: &node.Node{Alive: true, Mapping: map[string]any{"type": "tailscale", "auth-key": "k", "exit-node": "auto:any"}}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := needsMihomoRuntimeProbe(test.node); got != test.want {
				t.Fatalf("needsMihomoRuntimeProbe() = %t, want %t", got, test.want)
			}
		})
	}
}

// TestApplyConfigSkipsUnchangedReload 验证配置字节未变化时不会重复热更新 mihomo。
//
// 功能说明：
// 后台健康检查每 5 分钟触发一轮刷新，节点可用性没有变化时生成的配置与当前运行态完全
// 一致。此前的实现会让每一轮都重建整棵 tunnel（重新加载 geo 与规则集），既造成周期性
// CPU 尖峰，也是进程 RSS 高水位的主要来源。这里用 Runner 的成功应用计数验证稳态快路径
// 确实跳过了热更新，同时确认配置真的变化时仍然必须重新应用。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，负责创建临时配置路径并报告断言失败。
//
// 返回值：无。
//
// 错误情况：相同配置重复应用仍触发热更新，或配置变化后未热更新时测试失败。
func TestApplyConfigSkipsUnchangedReload(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("rules:\n  - MATCH,PROXY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newLANShareTestApp(t, "127.0.0.1", cfgPath)

	if err := a.Regenerate(); err != nil {
		t.Fatalf("首次生成并应用配置失败: %v", err)
	}
	first := a.runner.Reloads()
	if first == 0 {
		t.Fatal("首次应用必须真正热更新 mihomo")
	}
	applied := a.appliedConfig
	if len(applied) == 0 {
		t.Fatal("成功应用后必须记录已生效配置")
	}
	if !a.configApplied(applied) {
		t.Fatal("记录的已生效配置必须能通过等值比较")
	}

	if err := a.Regenerate(); err != nil {
		t.Fatalf("重复应用相同配置失败: %v", err)
	}
	if got := a.runner.Reloads(); got != first {
		t.Fatalf("配置未变化时不应再热更新: reloads %d -> %d", first, got)
	}

	// 清空记录模拟核心被停用/替换：之后必须重新热更新，而不是继续跳过。
	a.clearAppliedConfig()
	if a.configApplied(applied) {
		t.Fatal("clearAppliedConfig 之后不应再认为配置已生效")
	}
	if err := a.Regenerate(); err != nil {
		t.Fatalf("清空记录后重新应用失败: %v", err)
	}
	if got := a.runner.Reloads(); got <= first {
		t.Fatalf("清空记录后必须重新热更新: reloads %d -> %d", first, got)
	}

	before := a.runner.Reloads()
	if err := a.SetLANShare(true); err != nil {
		t.Fatalf("开启局域网共享失败: %v", err)
	}
	if got := a.runner.Reloads(); got <= before {
		t.Fatalf("配置真实变化后必须热更新: reloads %d -> %d", before, got)
	}
}

// TestRefreshSettlesDisplayOnFailure 验证一轮刷新以失败收场时不会留下「测速中」：
// 节点在检测阶段被标记，随后因为全部失效而提前返回，展示行必须已经复位成最终结果，
// 否则控制台的延迟列会永久停在「测速中…」。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无；通过展示行断言表达结果。
//
// 错误情况：刷新失败后节点仍带测速标记，或权威字段与展示行不一致时测试失败。
func TestRefreshSettlesDisplayOnFailure(t *testing.T) {
	portMapping := false
	a, err := New(&config.Config{
		// 非下载路径只接受启用订阅名下的内存节点，这里给节点配一个占位订阅名。
		Subscriptions: []config.Subscription{{Name: "sub", URL: "http://127.0.0.1:1/sub"}},
		Listen:        "127.0.0.1",
		PortRange:     [2]int{42100, 42110},
		PortMapping:   &portMapping, // 关闭一对一 listener，避免测试绑定真实端口
		Mode:          "rule",
		LogLevel:      "silent",
		StateDir:      t.TempDir(),
		Rules:         []string{"MATCH,PROXY"},
		HealthURL:     "http://127.0.0.1:1/never",
		HealthTimeout: config.Duration(500 * time.Millisecond),
	}, "")
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)

	dead := &node.Node{
		Name:         "失效节点",
		Subscription: "sub",
		Mapping:      map[string]any{"name": "失效节点", "type": "socks5", "server": "127.0.0.1", "port": 1},
	}
	dead.PublishResult()
	a.nodes = []*node.Node{dead}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Refresh(ctx, false); err == nil {
		t.Fatal("全部节点失效时刷新应返回错误")
	}

	display := dead.Display()
	if display.Testing {
		t.Fatalf("刷新失败后不应残留「测速中」: %+v", display)
	}
	if display.Alive || display.FailReason == "" {
		t.Fatalf("展示行应反映失败原因: %+v", display)
	}
	if display.Alive != dead.Alive || display.Delay != dead.Delay || display.FailReason != dead.FailReason {
		t.Fatalf("展示行应与权威字段一致: display=%+v node=%+v", display, dead)
	}
	if a.Testing() {
		t.Fatal("刷新结束后不应仍有轮次在跑")
	}
}

// TestInheritDisplayKeepsValuesAcrossReparse 验证重新解析出来的节点按身份（Key）继承
// 上一轮的展示值，而服务器或凭据变化的节点不继承——它已经是另一个出口，必须重新测速。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无；通过展示行断言表达结果。
//
// 错误情况：同一身份未继承（控制台会在检测期间整列闪成「失效 / —」），或身份变化的
// 节点继承了旧结果（把未经测速的新出口显示成可用）时失败。
func TestInheritDisplayKeepsValuesAcrossReparse(t *testing.T) {
	a, err := New(&config.Config{StateDir: t.TempDir(), LogLevel: "silent"}, "")
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)

	mapping := func(password string) map[string]any {
		return map[string]any{
			"name": "手动节点", "type": "socks5",
			"server": "127.0.0.1", "port": 1080, "password": password,
		}
	}
	previous := &node.Node{Name: "手动节点", Subscription: "manual", Mapping: mapping("pw")}
	previous.Alive = true
	previous.Delay = 88
	previous.PublishResult()
	a.nodes = []*node.Node{previous}

	// 同一服务器与凭据重新解析：Key 相同，继承上一轮的存活与延迟。
	same := &node.Node{Name: "手动节点", Subscription: "manual", Mapping: mapping("pw")}
	same.PublishResult()
	a.inheritDisplay([]*node.Node{same})
	if d := same.Display(); !d.Alive || d.Delay != 88 || d.Testing {
		t.Fatalf("同一身份的节点应继承上一轮展示值: %+v", d)
	}

	// 凭据变化：Key 不同，属于另一个出口，不得继承旧结果。
	rotated := &node.Node{Name: "手动节点", Subscription: "manual", Mapping: mapping("new-pw")}
	rotated.PublishResult()
	a.inheritDisplay([]*node.Node{rotated})
	if d := rotated.Display(); d.Alive || d.Delay != 0 {
		t.Fatalf("身份变化的节点不得继承旧结果: %+v", d)
	}
}

// TestInheritDisplayKeepsTailscaleIdentitiesApart 验证没有 auth-key 的多个 Tailscale
// 审批节点（共用同一个 Key）不会互相继承展示行：它们共用控制面与空凭据，只有领域身份
// （DedupKey，含出口名称）能区分彼此。
func TestInheritDisplayKeepsTailscaleIdentitiesApart(t *testing.T) {
	a, err := New(&config.Config{StateDir: t.TempDir(), LogLevel: "silent"}, "")
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)

	tsMapping := func(name string) map[string]any {
		return map[string]any{"name": name, "type": "tailscale", "control-url": "https://hs.example.com", "hostname": name}
	}
	home := &node.Node{Name: "home", Subscription: "manual", Mapping: tsMapping("home")}
	home.Alive = true
	home.Delay = 33
	home.PublishResult()
	a.nodes = []*node.Node{home}

	// 同一身份的 home 重新解析后继承；另一个审批节点 test 不得继承 home 的结果。
	freshHome := &node.Node{Name: "home", Subscription: "manual", Mapping: tsMapping("home")}
	freshHome.PublishResult()
	freshTest := &node.Node{Name: "test", Subscription: "manual", Mapping: tsMapping("test")}
	freshTest.PublishResult()
	a.inheritDisplay([]*node.Node{freshHome, freshTest})
	if d := freshHome.Display(); !d.Alive || d.Delay != 33 {
		t.Fatalf("同一 Tailscale 身份应继承展示值: %+v", d)
	}
	if d := freshTest.Display(); d.Alive || d.Delay != 0 {
		t.Fatalf("不同 Tailscale 审批节点不得继承彼此结果: %+v", d)
	}
}
