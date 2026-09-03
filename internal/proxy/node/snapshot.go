package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// SnapshotVersion 是节点快照格式的当前版本。
// 格式演进时递增；LoadSnapshot 遇到更高版本直接报错（调用方仅打日志丢弃，不致命）。
const SnapshotVersion = 1

// Snapshot 是最近一次合并后可用节点列表的持久化快照（state-dir/nodes.json）。
// 启动时先加载它立即提供服务，不必等首次订阅刷新完成；刷新成功后覆盖。
type Snapshot struct {
	Version int
	SavedAt time.Time
	Nodes   []*Node
}

// snapshotNode 是 Node 的序列化形态（Node 字段直接打 json tag 会污染其他用途，故独立定义）。
type snapshotNode struct {
	Name         string         `json:"name"`
	Subscription string         `json:"subscription"`
	Mapping      map[string]any `json:"mapping"`
	Alive        bool           `json:"alive"`
	Delay        uint16         `json:"delay"`
	FailReason   string         `json:"fail_reason,omitempty"`
}

// wire 是快照文件的 JSON 结构。
type wire struct {
	Version int            `json:"version"`
	SavedAt time.Time      `json:"saved_at"`
	Nodes   []snapshotNode `json:"nodes"`
}

// SaveSnapshot 把节点列表写入快照文件（临时文件 + rename，防止写坏）。
func SaveSnapshot(path string, nodes []*Node) error {
	sn := make([]snapshotNode, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		sn = append(sn, snapshotNode{
			Name:         n.Name,
			Subscription: n.Subscription,
			Mapping:      n.Mapping,
			Alive:        n.Alive,
			Delay:        n.Delay,
			FailReason:   n.FailReason,
		})
	}
	data, err := json.MarshalIndent(wire{SnapshotVersion, time.Now(), sn}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化节点快照失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建快照目录失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时快照失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换快照失败: %w", err)
	}
	return nil
}

// LoadSnapshot 从 JSON 文件加载节点快照。
// 文件不存在返回 (nil, nil)；解析失败/版本不兼容返回错误（调用方仅打日志丢弃）。
func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取节点快照失败: %w", err)
	}
	var raw wire
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析节点快照失败（已丢弃）: %w", err)
	}
	if raw.Version > SnapshotVersion {
		return nil, fmt.Errorf("节点快照版本 %d 高于当前支持的 %d（已丢弃）", raw.Version, SnapshotVersion)
	}
	s := &Snapshot{Version: raw.Version, SavedAt: raw.SavedAt}
	for _, sn := range raw.Nodes {
		if sn.Name == "" || sn.Mapping == nil {
			continue
		}
		sn.Mapping["name"] = sn.Name // 保持与 Node.Name 同步
		s.Nodes = append(s.Nodes, &Node{
			Name:         sn.Name,
			Subscription: sn.Subscription,
			Mapping:      sn.Mapping,
			Alive:        sn.Alive,
			Delay:        sn.Delay,
			FailReason:   sn.FailReason,
		})
	}
	return s, nil
}
