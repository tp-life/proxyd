// Package groupstate 持久化 select 类型节点分组的手动选中项
// （state-dir/group-selected.json）。选中值在 mihomo 配置生成时写入分组的
// default-selected 字段，使重启与订阅刷新后仍保持用户选择。
package groupstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FileName 是 select 分组选中项状态文件名（位于 state-dir 下）。
const FileName = "group-selected.json"

// wire 是状态文件的 JSON 结构；外层包裹一层为将来扩展（如版本字段）留余地。
type wire struct {
	Selected map[string]string `json:"selected"` // 分组名 -> 选中的节点名
}

// Load 从 JSON 文件读取分组选中项。
//
// 参数：
//   - path: string，状态文件完整路径。
//
// 返回值：
//   - map[string]string，分组名到选中节点名的映射；文件不存在时返回空 map。
//   - error，读取或解析失败时返回（调用方仅打日志忽略，不致命）。
//
// 错误情况：文件不存在不算错误；内容损坏时返回错误且映射为 nil。
func Load(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("读取分组选中状态失败: %w", err)
	}
	var raw wire
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析分组选中状态失败（已丢弃）: %w", err)
	}
	if raw.Selected == nil {
		raw.Selected = map[string]string{}
	}
	return raw.Selected, nil
}

// Save 把分组选中项原子写入 JSON 文件（临时文件 + rename，防止写坏）。
//
// 参数：
//   - path: string，状态文件完整路径；父目录不存在时自动创建。
//   - selected: map[string]string，分组名到选中节点名的完整映射（整体覆盖写）。
//
// 返回值：error，序列化、目录创建、写入或替换失败时返回。
//
// 错误情况：任一写入步骤失败时旧文件保持不变。
func Save(path string, selected map[string]string) error {
	data, err := json.MarshalIndent(wire{Selected: selected}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化分组选中状态失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建分组选中状态目录失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时分组选中状态失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换分组选中状态失败: %w", err)
	}
	return nil
}
