package remote

// 本文件封装服务端身份密钥的导出、校验与原子写入。
// tailcat 私钥格式属于不稳定外部协议，因此解析细节只能存在于 remote 基础设施适配层。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tailscale/tailcat"

	"proxyd/internal/config"
)

// ValidateServerKeyData 校验导入内容是否为含有效私钥的 tailcat 密钥 JSON。
//
// 参数说明：
//   - data: []byte，用户上传或 CLI 读取的完整密钥文件内容。
//
// 返回值说明：error，格式与私钥均有效时返回 nil。
//
// 错误情况：空内容、非法 JSON 或缺失 Private 字段时返回可读错误；不会写磁盘。
func ValidateServerKeyData(data []byte) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("服务端密钥文件不能为空")
	}
	var saved tailcat.PrivateKey
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("不是合法的 tailcat 服务端密钥: %w", err)
	}
	if saved.Private.IsZero() {
		return fmt.Errorf("tailcat 服务端密钥不含私钥")
	}
	return nil
}

// WriteServerKeyData 以 0600 权限原子替换指定服务端密钥文件。
//
// 参数说明：
//   - path: string，目标密钥文件路径。
//   - data: []byte，已经或即将被校验的 tailcat 密钥 JSON。
//
// 返回值说明：error，落盘和权限收紧均成功时返回 nil。
//
// 错误情况：校验、目录创建、临时文件写入、rename 或 chmod 失败时返回错误。
// 临时文件与目标位于同一目录，rename 不会暴露半写入内容；失败时清理临时文件。
func WriteServerKeyData(path string, data []byte) error {
	if err := ValidateServerKeyData(data); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建服务端密钥目录失败: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".proxyd-server-key-*")
	if err != nil {
		return fmt.Errorf("创建服务端密钥临时文件失败: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("设置服务端密钥临时文件权限失败: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("写入服务端密钥临时文件失败: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("同步服务端密钥临时文件失败: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭服务端密钥临时文件失败: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("原子替换服务端密钥失败: %w", err)
	}
	// 权限在 rename 前已设置为 0600，同一文件系统内的原子重命名会保留 inode 模式。
	// 不在 rename 后追加可能失败的 chmod，避免“调用方收到失败但新身份其实已经提交”的半状态。
	return nil
}

// ManagedServerKeyPath 返回 Manager 内置托管服务端密钥的固定路径。
//
// 参数说明：无。
//
// 返回值说明：string，位于 state-dir/remote 下，不受自定义 key-file 配置影响。
//
// 错误情况：无。
func (m *Manager) ManagedServerKeyPath() string {
	return filepath.Join(m.stateDir, serverKeyRelPath)
}

// ExportServerKey 读取当前配置实际使用的服务端密钥；不存在时先生成稳定身份。
//
// 参数说明：
//   - cfg: config.RemoteConfig，用于选择自定义 key-file 或内置托管路径。
//
// 返回值说明：[]byte 和 error，成功时返回与 tailcat CLI 兼容的完整私钥 JSON 副本。
//
// 错误情况：密钥生成、读取或格式校验失败时返回错误。Manager 锁避免与服务重建并发读写。
func (m *Manager) ExportServerKey(cfg config.RemoteConfig) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	path := m.serverKeyPath(cfg)
	if _, err := loadOrCreateNodeKey(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取服务端密钥 %s 失败: %w", path, err)
	}
	if err := ValidateServerKeyData(data); err != nil {
		return nil, fmt.Errorf("导出服务端密钥失败: %w", err)
	}
	return append([]byte(nil), data...), nil
}

// ReloadServerKey 强制按给定配置重建服务端，使磁盘上的新身份立即生效。
//
// 参数说明：
//   - cfg: config.RemoteConfig，导入后要应用的完整 remote 配置快照。
//
// 返回值说明：error，服务端与转发调和成功时返回 nil。
//
// 错误情况：启用状态下密钥读取或 tailcat 启动失败时返回错误；错误会保存在状态中。
// 与 Apply 不同，本方法即使配置字段相同也必定重建服务端，专用于密钥内容变化。
func (m *Manager) ReloadServerKey(cfg config.RemoteConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopServerLocked()
	var reloadErr error
	if cfg.Enabled {
		if err := m.startServerLocked(cfg); err != nil {
			m.serveErr = err.Error()
			reloadErr = fmt.Errorf("remote 服务端加载导入密钥失败: %w", err)
		}
	}
	m.reconcileForwardsLocked(cfg)
	m.cfg = cfg.Clone()
	return reloadErr
}
