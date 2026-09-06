package remote

// 本文件适配 tailcat v0.6 的 SSH authorized_keys 语义，并将公钥转换为稳定管理标识。
// 公钥认证叠加于隧道授权；既不保存客户端私钥，也不赋予切换本机账户的权限。

import (
	"fmt"
	"strings"
	"time"

	"github.com/tailscale/tailcat"
	"golang.org/x/crypto/ssh"

	"proxyd/internal/config"
)

// SSHKeyInfo 是可公开展示的 SSH 公钥及其 SHA256 指纹，不包含任何私钥。
type SSHKeyInfo struct {
	config.RemoteSSHKey
	Fingerprint string     `json:"fingerprint"`
	Type        string     `json:"type"`
	Expired     bool       `json:"expired"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	Active      int        `json:"active"`
}

// ParseSSHAuthorizedKeys 将 .pub 或 authorized_keys 文件解析为规范化公钥条目。
// 参数说明：text 为 string，上传的公钥文件内容；name 为 string，可选管理名称。
// 返回值说明：[]config.RemoteSSHKey 和 error，多行输入作为同一次原子导入。
// 错误情况：拒绝空文件、超过 64 KiB、私钥、损坏行及 command=/from= 等未实现的限制选项；
// 校验错误不回显输入，防止误上传的私钥出现在日志或 HTTP 错误中。
func ParseSSHAuthorizedKeys(text, name string) ([]config.RemoteSSHKey, error) {
	if len(text) > 64*1024 {
		return nil, fmt.Errorf("SSH 公钥文件不得超过 64 KiB")
	}
	if err := tailcat.ValidateSSHAuthorizedKeys([]string{text}); err != nil {
		return nil, fmt.Errorf("请提供有效的 OpenSSH 公钥（.pub 或 authorized_keys）；不接受私钥或公钥限制选项")
	}
	var entries []config.RemoteSSHKey
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		public, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("SSH 公钥解析失败")
		}
		label := strings.TrimSpace(name)
		if label == "" {
			label = strings.TrimSpace(comment)
		}
		entries = append(entries, config.RemoteSSHKey{
			Name: label, PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public))),
		})
	}
	if err := config.ValidateRemoteSSHKeys(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// NormalizeSSHKeys 严格验证授权集合并拒绝同一公钥以不同注释重复登记。
// 参数说明：entries 为 []config.RemoteSSHKey，待保存或载入的完整授权集合。
// 返回值说明：[]config.RemoteSSHKey 和 error；返回值不与输入共享切片。
// 错误情况：一条配置含多把密钥、条目格式错误或重复指纹时返回错误。
func NormalizeSSHKeys(entries []config.RemoteSSHKey) ([]config.RemoteSSHKey, error) {
	if err := config.ValidateRemoteSSHKeys(entries); err != nil {
		return nil, err
	}
	out := make([]config.RemoteSSHKey, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		parsed, err := ParseSSHAuthorizedKeys(entry.PublicKey, entry.Name)
		if err != nil {
			return nil, err
		}
		if len(parsed) != 1 {
			return nil, fmt.Errorf("每个 SSH 公钥条目必须恰好包含一把公钥")
		}
		if seen[parsed[0].PublicKey] {
			return nil, fmt.Errorf("SSH 公钥已存在，不能重复添加")
		}
		seen[parsed[0].PublicKey] = true
		next := entry.Clone()
		next.Name, next.PublicKey = parsed[0].Name, parsed[0].PublicKey
		out = append(out, next)
	}
	return out, nil
}

// SSHKeyInfos 为已验证的配置条目补充公钥类型与稳定指纹。
// 参数说明：entries 为 []config.RemoteSSHKey，配置快照中的公钥列表。
// 返回值说明：[]SSHKeyInfo，空集合返回空数组供 Web/CLI 一致渲染。
// 错误情况：损坏条目防御性跳过；正式配置入口会提前拒绝此类数据。
func SSHKeyInfos(entries []config.RemoteSSHKey) []SSHKeyInfo {
	infos := make([]SSHKeyInfo, 0, len(entries))
	for _, entry := range entries {
		public, _, _, _, err := ssh.ParseAuthorizedKey([]byte(entry.PublicKey))
		if err == nil {
			infos = append(infos, SSHKeyInfo{RemoteSSHKey: entry.Clone(), Fingerprint: ssh.FingerprintSHA256(public), Type: public.Type(), Expired: entry.IsExpired(time.Now())})
		}
	}
	return infos
}

// FindSSHKey 按完整指纹优先、唯一名称其次定位原始配置中的公钥。
// 参数说明：entries 为 []config.RemoteSSHKey；identifier 为 string，指纹或管理名称。
// 返回值说明：int 与 error，返回原始切片索引。
// 错误情况：格式错误、未找到或名称歧义时返回错误；先规范化避免展示层跳过坏条目导致误删。
func FindSSHKey(entries []config.RemoteSSHKey, identifier string) (int, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return -1, fmt.Errorf("请指定 SSH 公钥指纹或名称")
	}
	normalized, err := NormalizeSSHKeys(entries)
	if err != nil {
		return -1, err
	}
	infos := SSHKeyInfos(normalized)
	for i, entry := range infos {
		if entry.Fingerprint == identifier {
			return i, nil
		}
	}
	index := -1
	for i, entry := range infos {
		if entry.Name == identifier {
			if index >= 0 {
				return -1, fmt.Errorf("存在同名 SSH 公钥，请使用指纹")
			}
			index = i
		}
	}
	if index < 0 {
		return -1, fmt.Errorf("未找到 SSH 公钥")
	}
	return index, nil
}
