package app

// 本文件编排 SSH 公钥管理用例；所有写入复用 remote 配置事务，失败整体回滚。

import (
	"fmt"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/remote"
)

// SetRemoteSSHAuth 切换内嵌 SSH 的附加公钥认证，不修改隧道或 Web 终端的开关。
// 参数说明：required 为 bool，true 表示仅允许已登记 SSH 公钥登录。
// 返回值说明：error，内存、运行态和配置落盘全部成功时为 nil。
// 错误情况：调和或持久化失败时回滚；空授权集合保持拒绝全部，不隐式恢复免密模式。
func (a *App) SetRemoteSSHAuth(required bool) error {
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		r.SSHAuthRequired = required
		return nil
	})
}

// AddRemoteSSHKeys 将粘贴的公钥或公钥文件一次性导入授权集合。
// 参数说明：text 为 string，.pub/authorized_keys 内容；name 为 string，可选管理名称。
// 返回值说明：error，所有条目均被保存时为 nil。
// 错误情况：格式、数量、重复公钥、调和或落盘失败时不导入任何条目；
// 在事务锁内与最新集合合并，避免两个管理客户端并发添加时互相覆盖。
func (a *App) AddRemoteSSHKeys(text, name string) error {
	entries, err := remote.ParseSSHAuthorizedKeys(text, name)
	if err != nil {
		return err
	}
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		next, err := remote.NormalizeSSHKeys(append(r.SSHKeys, entries...))
		if err != nil {
			return err
		}
		r.SSHKeys = next
		return nil
	})
}

// DeleteRemoteSSHKey 按稳定指纹或唯一名称撤销 SSH 公钥授权。
// 参数说明：identifier 为 string，SHA256 指纹或管理名称；同名多项必须改用指纹。
// 返回值说明：error，匹配的一项已事务删除时为 nil。
// 错误情况：未找到、名称歧义、调和或落盘失败时返回错误且保留旧集合；
// 删除最后一项只清空公钥，不关闭 SSHAuthRequired，以免撤销权限变成放开权限。
func (a *App) DeleteRemoteSSHKey(identifier string) error {
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		index, err := remote.FindSSHKey(r.SSHKeys, identifier)
		if err != nil {
			return err
		}
		r.SSHKeys = append(r.SSHKeys[:index], r.SSHKeys[index+1:]...)
		return nil
	})
}

// UpdateRemoteSSHKey 修改单把公钥的启用状态或到期时间，保留公钥本身及其他条目。
// 参数说明：identifier 为 string；disabled 为 *bool，nil 表示不修改；expiresAt 为 *string，
// nil 表示不修改，空串表示永久，其余必须为 RFC3339 时间。
// 返回值说明：error，事务成功时为 nil。
// 错误情况：参数无效、目标缺失、歧义或落盘失败时整体回滚；过去时间允许保存并立即失效。
func (a *App) UpdateRemoteSSHKey(identifier string, disabled *bool, expiresAt *string) error {
	if disabled == nil && expiresAt == nil {
		return fmt.Errorf("请提供 disabled 或 expires_at")
	}
	var expiry *time.Time
	if expiresAt != nil && *expiresAt != "" {
		value, err := time.Parse(time.RFC3339, *expiresAt)
		if err != nil || value.IsZero() {
			return fmt.Errorf("expires_at 必须是 RFC3339 时间，或空字符串表示永久")
		}
		value = value.UTC()
		expiry = &value
	}
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		index, err := remote.FindSSHKey(r.SSHKeys, identifier)
		if err != nil {
			return err
		}
		if disabled != nil {
			r.SSHKeys[index].Disabled = *disabled
		}
		if expiresAt != nil {
			r.SSHKeys[index].ExpiresAt = expiry
		}
		return nil
	})
}

// DisconnectRemoteSSHKey 断开指定公钥已经认证的连接，独立于配置更新。
// 参数说明：fingerprint 为 string，完整 SHA256 指纹。
// 返回值说明：int 与 error，表示已请求断开的连接数。
// 错误情况：指纹无效时拒绝；此操作不撤销授权，要阻止重连需先禁用或删除公钥。
func (a *App) DisconnectRemoteSSHKey(fingerprint string) (int, error) {
	return a.remote.DisconnectSSHKey(fingerprint)
}
