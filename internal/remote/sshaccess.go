package remote

// 本文件管理 SSH 授权策略与已认证连接；独立锁使公钥更新不需要重启 tailcat 隧道。
// 最近使用时间与连接审计仅在当前进程保留，不把高频登录写入用户配置文件。

import (
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"proxyd/internal/config"
)

// sshAccess 是 remote 上下文的 SSH 策略服务，所有集合均由 mu 保护。
// 策略只决定后续认证；已建立连接单独索引，只有显式断开才结束现有会话。
type sshAccess struct {
	mu          sync.Mutex
	required    bool
	keys        map[string]config.RemoteSSHKey
	lastUsed    map[string]time.Time
	connections map[net.Conn]string
	audit       *auditLog
}

// newSSHAccess 构造独立的公钥策略运行态。
// 参数说明：audit 为 *auditLog，复用 remote 的有界审计环。
// 返回值说明：*sshAccess，初始为兼容免密模式。
// 错误情况：无；不打开文件或网络连接。
func newSSHAccess(audit *auditLog) *sshAccess {
	return &sshAccess{keys: map[string]config.RemoteSSHKey{}, lastUsed: map[string]time.Time{}, connections: map[net.Conn]string{}, audit: audit}
}

// update 原子替换已验证的配置策略，保留已有会话。
// 参数说明：required 为 bool；entries 为 []config.RemoteSSHKey，已通过 NormalizeSSHKeys。
// 返回值说明：无。
// 错误情况：无；未登记密钥的历史使用时间会清理，限制内存随删除/添加而无限增长。
func (a *sshAccess) update(required bool, entries []config.RemoteSSHKey) {
	keys := make(map[string]config.RemoteSSHKey, len(entries))
	for _, info := range SSHKeyInfos(entries) {
		keys[info.Fingerprint] = info.RemoteSSHKey.Clone()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.required, a.keys = required, keys
	for fp := range a.lastUsed {
		if _, ok := keys[fp]; !ok {
			delete(a.lastUsed, fp)
		}
	}
}

// checkLocked 在认证时判断公钥、禁用状态和到期时间。
// 参数说明：fingerprint 为 string，空串表示免密认证；now 为 time.Time；调用方持有 mu。
// 返回值说明：error，获准认证时为 nil。
// 错误情况：未知、禁用或过期公钥被拒绝；未启用附加认证时只允许空指纹的兼容路径。
func (a *sshAccess) checkLocked(fingerprint string, now time.Time) error {
	if fingerprint == "" {
		if !a.required {
			return nil
		}
		return fmt.Errorf("SSH public key is required")
	}
	entry, ok := a.keys[fingerprint]
	if !ok {
		return fmt.Errorf("SSH public key is not authorized")
	}
	if entry.Disabled {
		return fmt.Errorf("SSH public key is disabled")
	}
	if entry.IsExpired(now) {
		return fmt.Errorf("SSH public key has expired")
	}
	return nil
}

// check 执行公钥探测阶段的授权检查，不更新最近登录时间。
// 参数说明：fingerprint 为 string，客户端提供公钥的 SHA256 指纹。
// 返回值说明：error，当前有效时为 nil。
// 错误情况：任何未获授权的公钥返回错误；公钥探测本身不能证明持有私钥。
func (a *sshAccess) check(fingerprint string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.checkLocked(fingerprint, time.Now())
}

// authenticated 在签名验证后重新检查实时策略并登记成功认证。
// 参数说明：conn 为 net.Conn，当前 SSH 传输；fingerprint 为 string，免密时为空。
// 返回值说明：error，登记成功时为 nil。
// 错误情况：探测与签名之间密钥失效时拒绝；登记与显式断开共用锁，避免漏掉已成功认证的连接。
func (a *sshAccess) authenticated(conn net.Conn, fingerprint string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now().UTC()
	if err := a.checkLocked(fingerprint, now); err != nil {
		return err
	}
	a.connections[conn] = fingerprint
	name := "隧道免密"
	if fingerprint != "" {
		a.lastUsed[fingerprint] = now
		name = a.keys[fingerprint].Name
	}
	a.audit.Append(AuditEntry{Time: now, Action: "ssh_authenticated", TargetPort: 22, SSHFingerprint: fingerprint, SSHKeyName: name})
	return nil
}

// finished 清理传输索引并记录认证结果或会话结束。
// 参数说明：conn 为 net.Conn，已经退出处理的传输；fingerprint 为 string，最后尝试的公钥。
// 返回值说明：无。
// 错误情况：未完成认证的断连记录为失败，不把公钥探测记录成登录成功。
func (a *sshAccess) finished(conn net.Conn, fingerprint string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fp, ok := a.connections[conn]
	action, reason := "ssh_failed", "SSH 握手或认证未完成"
	if ok {
		fingerprint = fp
		action = "ssh_disconnected"
		reason = ""
		delete(a.connections, conn)
	}
	a.audit.Append(AuditEntry{Action: action, TargetPort: 22, SSHFingerprint: fingerprint, SSHKeyName: a.keys[fingerprint].Name, Reason: reason})
}

// infos 补充当前进程内的最近认证时间与活动 SSH 传输数。
// 参数说明：entries 为 []config.RemoteSSHKey，已取得的配置快照。
// 返回值说明：[]SSHKeyInfo，时间指针和配置均为副本。
// 错误情况：无；重启后最近使用时间为空，活动数从零开始。
func (a *sshAccess) infos(entries []config.RemoteSSHKey) []SSHKeyInfo {
	out := SSHKeyInfos(entries)
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range out {
		if when, ok := a.lastUsed[out[i].Fingerprint]; ok {
			out[i].LastUsedAt = &when
		}
		for _, fp := range a.connections {
			if fp == out[i].Fingerprint {
				out[i].Active++
			}
		}
	}
	return out
}

// disconnect 断开调用时已经以指定公钥完成认证的传输。
// 参数说明：fingerprint 为 string，完整 SHA256 指纹；即使公钥已删除也可按指纹断开。
// 返回值说明：int，本次选中的连接数。
// 错误情况：网络 Close 错误忽略；先在锁内取得快照，锁外关闭，避免回调清理死锁。
// 此操作不改变授权；若希望禁止重连，管理员应先禁用或撤销公钥。
func (a *sshAccess) disconnect(fingerprint string) int {
	a.mu.Lock()
	var conns []net.Conn
	for conn, fp := range a.connections {
		if fp == fingerprint {
			conns = append(conns, conn)
		}
	}
	a.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	a.audit.Append(AuditEntry{Action: "ssh_disconnect_requested", TargetPort: 22, SSHFingerprint: fingerprint, Reason: fmt.Sprintf("管理员请求断开 %d 条连接", len(conns))})
	return len(conns)
}

// DisconnectSSHKey 暴露按密钥断开已有传输的管理用例。
// 参数说明：fingerprint 为 string，必须来自公钥列表或审计记录的完整 SHA256 指纹。
// 返回值说明：int 与 error，返回已请求断开的连接数。
// 错误情况：指纹格式不正确时拒绝；撤销操作本身由应用层另行事务保存。
func (m *Manager) DisconnectSSHKey(fingerprint string) (int, error) {
	raw, ok := strings.CutPrefix(fingerprint, "SHA256:")
	decoded, err := base64.RawStdEncoding.DecodeString(raw)
	if !ok || err != nil || len(decoded) != 32 {
		return 0, fmt.Errorf("请提供完整的 SSH 公钥 SHA256 指纹")
	}
	return m.sshAccess.disconnect(fingerprint), nil
}
