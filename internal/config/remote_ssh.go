package config

// 本文件定义 remote 上下文中的 SSH 公钥授权值对象；它不依赖 SSH SDK 或文件系统。

import (
	"fmt"
	"strings"
	"time"
)

// RemoteSSHKey 表示一个获准登录内嵌 SSH 的公钥；私钥始终由客户端自行保管。
// Name 仅用于管理展示，PublicKey 是一行 OpenSSH authorized_keys 文本。
type RemoteSSHKey struct {
	Name      string     `yaml:"name,omitempty" json:"name,omitempty"`
	PublicKey string     `yaml:"public-key" json:"public_key"`
	Disabled  bool       `yaml:"disabled,omitempty" json:"disabled"`
	ExpiresAt *time.Time `yaml:"expires-at,omitempty" json:"expires_at,omitempty"`
}

// Clone 复制公钥值对象，隔离可变的到期时间指针。
// 参数说明：无；接收者为 RemoteSSHKey。
// 返回值说明：RemoteSSHKey，内容相同且不共享时间指针。
// 错误情况：无，永久授权保留 nil。
func (k RemoteSSHKey) Clone() RemoteSSHKey {
	if k.ExpiresAt != nil {
		expiry := *k.ExpiresAt
		k.ExpiresAt = &expiry
	}
	return k
}

// IsExpired 判断当前时间是否已到授权边界。
// 参数说明：now 为 time.Time，认证发生的时间。
// 返回值说明：bool，到期时刻本身也视为失效。
// 错误情况：无，未设置时间表示永久有效。
func (k RemoteSSHKey) IsExpired(now time.Time) bool {
	return k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)
}

// ValidateRemoteSSHKeys 校验授权集合的资源边界，密码学格式由 remote 适配层校验。
// 参数说明：entries 为 []RemoteSSHKey，配置中保存的公钥条目。
// 返回值说明：error，满足结构限制时为 nil。
// 错误情况：超过 128 项、名称含控制字符或过长、公钥为空或超过 16 KiB 时返回错误。
func ValidateRemoteSSHKeys(entries []RemoteSSHKey) error {
	if len(entries) > 128 {
		return fmt.Errorf("SSH 公钥最多允许 128 个")
	}
	for i, entry := range entries {
		if entry.ExpiresAt != nil && entry.ExpiresAt.IsZero() {
			return fmt.Errorf("SSH 公钥[%d] 到期时间无效", i+1)
		}
		if len(entry.Name) > 128 || strings.ContainsAny(entry.Name, "\r\n\t\x00") {
			return fmt.Errorf("SSH 公钥[%d] 名称过长或含控制字符", i+1)
		}
		if strings.TrimSpace(entry.PublicKey) == "" || len(entry.PublicKey) > 16*1024 {
			return fmt.Errorf("SSH 公钥[%d] 不能为空且不得超过 16 KiB", i+1)
		}
	}
	return nil
}
