package subscribe

import (
	"regexp"
	"testing"

	"proxyd/internal/node"
)

// mkNode 构造测试节点。
func mkNode(name, sub, typ, server string, port int, credKey, cred string) *node.Node {
	m := map[string]any{
		"name":   name,
		"type":   typ,
		"server": server,
		"port":   port,
	}
	if credKey != "" {
		m[credKey] = cred
	}
	return &node.Node{Name: name, Subscription: sub, Mapping: m}
}

func TestMergeDedup(t *testing.T) {
	a := mkNode("节点1", "subA", "ss", "1.1.1.1", 1000, "password", "pw")
	dup := mkNode("节点1-别名", "subB", "ss", "1.1.1.1", 1000, "password", "pw") // Key 相同
	other := mkNode("节点2", "subB", "ss", "1.1.1.1", 1001, "password", "pw")

	out := Merge(map[string][]*node.Node{
		"subA": {a},
		"subB": {dup, other},
	}, nil)
	if len(out) != 2 {
		t.Fatalf("期望去重后 2 个节点，得到 %d", len(out))
	}
	if out[0].Name != "节点1" || out[0].Subscription != "subA" {
		t.Errorf("应保留先出现的节点: %+v", out[0])
	}
}

func TestMergeExclude(t *testing.T) {
	re := regexp.MustCompile(`官网|套餐|剩余流量`)
	out := Merge(map[string][]*node.Node{
		"subA": {
			mkNode("香港01", "subA", "ss", "1.1.1.1", 1000, "password", "pw"),
			mkNode("官网地址", "subA", "ss", "2.2.2.2", 2000, "password", "pw2"),
			mkNode("剩余流量 100G", "subA", "ss", "3.3.3.3", 3000, "password", "pw3"),
		},
	}, re)
	if len(out) != 1 || out[0].Name != "香港01" {
		t.Fatalf("exclude 过滤不正确: %+v", out)
	}
}

func TestMergeUniqueName(t *testing.T) {
	out := Merge(map[string][]*node.Node{
		"subA": {mkNode("同名", "subA", "ss", "1.1.1.1", 1000, "password", "pw1")},
		"subB": {
			mkNode("同名", "subB", "ss", "2.2.2.2", 2000, "password", "pw2"),
			mkNode("同名", "subB", "ss", "3.3.3.3", 3000, "password", "pw3"),
		},
	}, nil)
	if len(out) != 3 {
		t.Fatalf("期望 3 个节点，得到 %d", len(out))
	}
	want := []string{"同名", "同名 (subB)", "同名 (subB) 2"}
	for i, w := range want {
		if out[i].Name != w {
			t.Errorf("节点 %d 名字期望 %q，得到 %q", i, w, out[i].Name)
		}
		if out[i].Mapping["name"] != out[i].Name {
			t.Errorf("节点 %d Mapping[name] 未同步: %v", i, out[i].Mapping["name"])
		}
	}
	if out[1].Subscription != "subB" || out[2].Subscription != "subB" {
		t.Errorf("Subscription 未设置: %+v", out)
	}
}
