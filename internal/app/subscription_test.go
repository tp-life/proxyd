package app

import (
	"context"
	"path/filepath"
	"testing"

	"proxyd/internal/config"
	"proxyd/internal/node"
	"proxyd/internal/pool"
)

// subscriptionTestNode 构造订阅设置事务测试使用的最小 socks5 节点。
//
// 参数：
//   - name: string，节点展示名与 mihomo 出站名。
//   - subscription: string，节点所属订阅名。
//   - server: string，构成稳定身份的测试服务器地址。
//
// 返回值：*node.Node，标记为健康且包含可被 mihomo 解析的最小映射。
//
// 错误情况：无；该函数只生成内存测试数据，不访问网络或文件系统。
func subscriptionTestNode(name, subscription, server string) *node.Node {
	return &node.Node{
		Name:         name,
		Subscription: subscription,
		Alive:        true,
		Delay:        10,
		Mapping: map[string]any{
			"name":   name,
			"type":   "socks5",
			"server": server,
			"port":   1080,
		},
	}
}

// TestUpdateSubscriptionDisableAndRenameCommitsAtomically 验证禁用并重命名订阅时，
// 配置、策略组引用、节点运行态和磁盘文件作为一次事务提交；其它订阅节点继续可用。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于隔离配置/状态目录并报告断言失败。
//
// 返回值：无。
//
// 错误情况：事务返回错误、旧订阅节点仍在运行态、策略组引用未改名，或磁盘状态
// 与内存状态不一致时测试失败。
func TestUpdateSubscriptionDisableAndRenameCommitsAtomically(t *testing.T) {
	disabledPortMapping := false
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Subscriptions: []config.Subscription{
			{Name: "old", URL: "https://old.example/sub", Type: "auto"},
			{Name: "other", URL: "https://other.example/sub", Type: "auto"},
		},
		Listen:      "127.0.0.1",
		PortRange:   [2]int{42000, 42010},
		PortMapping: &disabledPortMapping,
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
		Groups: []config.NodeGroup{{
			Name: "old-group", Port: 43000, Subscription: "old",
		}},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("保存初始配置失败: %v", err)
	}
	a, err := New(cfg, cfgPath)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	t.Cleanup(a.Shutdown)
	oldNode := subscriptionTestNode("旧节点", "old", "10.0.0.1")
	otherNode := subscriptionTestNode("其它节点", "other", "10.0.0.2")
	a.nodes = []*node.Node{oldNode, otherNode}
	a.assigns = []pool.Assignment{{Port: 42000, Node: oldNode}, {Port: 42001, Node: otherNode}}
	disabled := false

	next, err := a.UpdateSubscription(context.Background(), "old", config.Subscription{
		Name: "renamed", URL: "https://old.example/sub", Type: "auto", Enabled: &disabled,
	})
	if err != nil {
		t.Fatalf("禁用并重命名订阅失败: %v", err)
	}
	if next.Name != "renamed" || next.IsEnabled() {
		t.Fatalf("返回订阅状态异常: %+v", next)
	}
	if len(a.nodes) != 1 || a.nodes[0].Subscription != "other" {
		t.Fatalf("禁用订阅的节点未从运行态移除: %+v", a.nodes)
	}
	if len(a.cfg.Groups) != 1 || a.cfg.Groups[0].Subscription != "renamed" {
		t.Fatalf("策略组订阅引用未同步改名: %+v", a.cfg.Groups)
	}
	onDisk, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("读取提交后的配置失败: %v", err)
	}
	if len(onDisk.Subscriptions) != 2 || onDisk.Subscriptions[0].Name != "renamed" || onDisk.Subscriptions[0].IsEnabled() {
		t.Fatalf("磁盘订阅状态未原子提交: %+v", onDisk.Subscriptions)
	}
	if onDisk.Groups[0].Subscription != "renamed" {
		t.Fatalf("磁盘策略组引用未同步提交: %+v", onDisk.Groups)
	}
}

// TestUpdateSubscriptionEnableFailureKeepsDisabled 验证启用订阅必须先获得新内容或可用缓存；
// 拉取与缓存都失败时，订阅保持禁用且配置文件不发生变化。
//
// 参数：
//   - t: *testing.T，Go 测试上下文，用于创建隔离配置并报告断言失败。
//
// 返回值：无。
//
// 错误情况：无效 URL 被当作启用成功、内存 enabled 被改为 true，或磁盘配置被覆盖时测试失败。
func TestUpdateSubscriptionEnableFailureKeepsDisabled(t *testing.T) {
	disabled := false
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Subscriptions: []config.Subscription{{
			Name: "offline", URL: "http://[::1", Type: "auto", Enabled: &disabled,
		}},
		ManualNodes: []string{"socks5://127.0.0.1:1080#manual"},
		Listen:      "127.0.0.1",
		PortRange:   [2]int{42000, 42010},
		Mode:        "rule",
		LogLevel:    "silent",
		StateDir:    t.TempDir(),
		Rules:       []string{"MATCH,PROXY"},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("保存初始配置失败: %v", err)
	}
	a, err := New(cfg, cfgPath)
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	enabled := true
	_, err = a.UpdateSubscription(context.Background(), "offline", config.Subscription{
		Name: "offline", URL: "http://[::1", Type: "auto", Enabled: &enabled,
	})
	if err == nil {
		t.Fatal("拉取与缓存都失败时不应启用订阅")
	}
	if a.cfg.Subscriptions[0].IsEnabled() {
		t.Fatal("启用失败后内存订阅没有恢复禁用")
	}
	onDisk, loadErr := config.Load(cfgPath)
	if loadErr != nil {
		t.Fatalf("读取回滚后的配置失败: %v", loadErr)
	}
	if onDisk.Subscriptions[0].IsEnabled() {
		t.Fatal("启用失败后磁盘订阅没有保持禁用")
	}
}
