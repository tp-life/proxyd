package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"proxyd/internal/proxy/node"
)

// Assignment 是一个节点到本地端口的映射结果。
type Assignment struct {
	Port int
	Node *node.Node
}

// Snapshot 是端口分配的持久化快照（stateDir/mapping.json）。
type Snapshot struct {
	// Mapping 记录 node.Key() -> port。
	Mapping map[string]int `json:"mapping"`
}

// Allocate 为可用节点分配 [lo, hi] 范围内的端口：
//   - 内部按 Delay 升序排序（相同按 Name 稳定排序），超出容量的节点按延迟截断；
//   - 上一轮快照中已占用端口的节点（按 Node.Key() 匹配）保留原端口；
//   - 新节点按延迟顺序从小到大填入空闲端口；
//   - 消失节点的端口自然释放；prev 中端口越界的条目被忽略。
//
// 返回的 Assignment 按端口升序排列。
func Allocate(nodes []*node.Node, lo, hi int, prev *Snapshot) []Assignment {
	sorted := make([]*node.Node, len(nodes))
	copy(sorted, nodes)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Delay != sorted[j].Delay {
			return sorted[i].Delay < sorted[j].Delay
		}
		return sorted[i].Name < sorted[j].Name
	})

	// 容量不足时按延迟截断，只保留最快的 N 个
	if capacity := hi - lo + 1; len(sorted) > capacity {
		if capacity < 0 {
			capacity = 0
		}
		sorted = sorted[:capacity]
	}

	ports := make(map[*node.Node]int, len(sorted))
	used := make(map[int]bool, len(sorted))

	// 先为老节点保留原端口
	if prev != nil {
		for _, n := range sorted {
			p, ok := prev.Mapping[n.Key()]
			if !ok || p < lo || p > hi || used[p] {
				continue
			}
			ports[n] = p
			used[p] = true
		}
	}

	// 再为剩余节点按延迟顺序填入空闲端口
	next := lo
	for _, n := range sorted {
		if _, ok := ports[n]; ok {
			continue
		}
		for used[next] {
			next++
		}
		ports[n] = next
		used[next] = true
	}

	out := make([]Assignment, 0, len(ports))
	for n, p := range ports {
		out = append(out, Assignment{Port: p, Node: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// LoadSnapshot 从 JSON 文件加载快照；文件不存在时返回空快照和 nil 错误。
func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Snapshot{Mapping: map[string]int{}}, nil
		}
		return nil, fmt.Errorf("读取快照失败: %w", err)
	}
	s := &Snapshot{}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("解析快照失败: %w", err)
	}
	if s.Mapping == nil {
		s.Mapping = map[string]int{}
	}
	return s, nil
}

// SaveSnapshot 将快照写入 JSON 文件：先写临时文件再 rename，避免写坏已有文件。
func SaveSnapshot(path string, s *Snapshot) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化快照失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建快照目录失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写入临时快照失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换快照失败: %w", err)
	}
	return nil
}
