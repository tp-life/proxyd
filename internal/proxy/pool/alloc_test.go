package pool

import (
	"os"
	"path/filepath"
	"testing"

	"proxyd/internal/proxy/node"
)

// mkNode 构造一个测试节点，server 用名字代替以保证 Key 唯一。
func mkNode(name string, delay uint16) *node.Node {
	return &node.Node{
		Name: name,
		Mapping: map[string]any{
			"name":   name,
			"type":   "socks5",
			"server": name + ".example",
			"port":   443,
		},
		Delay: delay,
	}
}

func TestAllocate(t *testing.T) {
	a := mkNode("A", 30)
	b := mkNode("B", 10)
	c := mkNode("C", 20)

	tests := []struct {
		name  string
		nodes []*node.Node
		lo    int
		hi    int
		prev  *Snapshot
		// 期望的 name -> port
		want map[string]int
	}{
		{
			name:  "全新分配按延迟排序连续占位",
			nodes: []*node.Node{a, b, c},
			lo:    101,
			hi:    110,
			want:  map[string]int{"B": 101, "C": 102, "A": 103},
		},
		{
			name:  "延迟相同按 Name 稳定排序",
			nodes: []*node.Node{mkNode("Y", 10), mkNode("X", 10)},
			lo:    100,
			hi:    101,
			want:  map[string]int{"X": 100, "Y": 101},
		},
		{
			name:  "稳定映射保留老节点端口",
			nodes: []*node.Node{mkNode("B", 5), mkNode("C", 1), mkNode("A", 9)},
			lo:    101,
			hi:    110,
			prev: &Snapshot{Mapping: map[string]int{
				mkNode("A", 0).Key(): 101,
				mkNode("B", 0).Key(): 102,
			}},
			want: map[string]int{"A": 101, "B": 102, "C": 103},
		},
		{
			name:  "节点下线释放端口新节点补位",
			nodes: []*node.Node{a, c}, // B 下线
			lo:    101,
			hi:    110,
			prev: &Snapshot{Mapping: map[string]int{
				mkNode("A", 0).Key(): 101,
				mkNode("B", 0).Key(): 102,
				mkNode("C", 0).Key(): 103,
			}},
			// A、C 保留原位，102 被释放但无人补位
			want: map[string]int{"A": 101, "C": 103},
		},
		{
			name:  "超容量按延迟截断",
			nodes: []*node.Node{mkNode("n1", 50), mkNode("n2", 40), mkNode("n3", 30), mkNode("n4", 20), mkNode("n5", 10)},
			lo:    200,
			hi:    202,
			want:  map[string]int{"n5": 200, "n4": 201, "n3": 202},
		},
		{
			name:  "prev 端口越界忽略并重新分配",
			nodes: []*node.Node{a},
			lo:    101,
			hi:    110,
			prev: &Snapshot{Mapping: map[string]int{
				a.Key(): 999, // 越界
			}},
			want: map[string]int{"A": 101},
		},
		{
			name:  "prev 含不存在节点不影响分配",
			nodes: []*node.Node{a, b},
			lo:    101,
			hi:    110,
			prev: &Snapshot{Mapping: map[string]int{
				mkNode("ghost", 0).Key(): 101, // 已消失，端口应释放
				b.Key():                  102,
			}},
			want: map[string]int{"B": 102, "A": 101},
		},
		{
			name:  "prev 为 nil 等价全新分配",
			nodes: []*node.Node{a, b},
			lo:    101,
			hi:    110,
			prev:  nil,
			want:  map[string]int{"B": 101, "A": 102},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Allocate(tt.nodes, tt.lo, tt.hi, tt.prev)
			if len(got) != len(tt.want) {
				t.Fatalf("分配数量 = %d, 期望 %d (%+v)", len(got), len(tt.want), got)
			}
			for _, as := range got {
				wantPort, ok := tt.want[as.Node.Name]
				if !ok {
					t.Fatalf("节点 %s 不应被分配端口 %d", as.Node.Name, as.Port)
				}
				if as.Port != wantPort {
					t.Errorf("节点 %s 端口 = %d, 期望 %d", as.Node.Name, as.Port, wantPort)
				}
			}
			// 结果应按端口升序
			for i := 1; i < len(got); i++ {
				if got[i-1].Port > got[i].Port {
					t.Errorf("结果未按端口升序: %+v", got)
					break
				}
			}
		})
	}
}

// TestAllocateSkipsTunnelNodes 验证隧道类（VPN 语义）节点不参与每节点端口映射：
// 不占用端口、不参与容量截断计数，普通节点的分配行为不受影响。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无。
//
// 错误情况：隧道节点获得端口，或其存在导致普通节点被容量截断时测试失败。
func TestAllocateSkipsTunnelNodes(t *testing.T) {
	tunnel := &node.Node{
		Name: "vpn",
		Mapping: map[string]any{
			"name": "vpn", "type": "tailscale", "auth-key": "tskey-auth-xxx",
		},
		Delay: 1, // 延迟最低也不应挤占端口
	}
	a := mkNode("A", 30)
	b := mkNode("B", 10)

	// 区间只有 2 个端口：若隧道节点参与分配，最慢的 A 会被截断。
	got := Allocate([]*node.Node{tunnel, a, b}, 101, 102, nil)
	if len(got) != 2 {
		t.Fatalf("隧道节点不应计入端口分配: %+v", got)
	}
	for _, as := range got {
		if as.Node.Name == "vpn" {
			t.Fatalf("隧道节点不应获得端口: %+v", as)
		}
	}
	want := map[string]int{"B": 101, "A": 102}
	for _, as := range got {
		if as.Port != want[as.Node.Name] {
			t.Errorf("节点 %s 端口 = %d, 期望 %d", as.Node.Name, as.Port, want[as.Node.Name])
		}
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mapping.json")

	// 不存在的文件应返回空快照且无错误
	s, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot(不存在) 出错: %v", err)
	}
	if s == nil || len(s.Mapping) != 0 {
		t.Fatalf("LoadSnapshot(不存在) = %+v, 期望空快照", s)
	}

	want := &Snapshot{Mapping: map[string]int{
		mkNode("A", 0).Key(): 101,
		mkNode("B", 0).Key(): 102,
	}}
	if err := SaveSnapshot(path, want); err != nil {
		t.Fatalf("SaveSnapshot 出错: %v", err)
	}

	got, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot 出错: %v", err)
	}
	if len(got.Mapping) != len(want.Mapping) {
		t.Fatalf("往返后 Mapping 长度 = %d, 期望 %d", len(got.Mapping), len(want.Mapping))
	}
	for k, v := range want.Mapping {
		if got.Mapping[k] != v {
			t.Errorf("Mapping[%q] = %d, 期望 %d", k, got.Mapping[k], v)
		}
	}

	// 临时文件不应残留
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("临时文件未被 rename 清理: err=%v", err)
	}
}
