package remote

import (
	"fmt"
	"path/filepath"
	"strings"

	"tailscale.com/types/key"
)

// clientKeyRelPath 是客户端身份密钥在 stateDir 下的相对路径。
// 密钥决定本机作为隧道客户端的 node 公钥：对端服务端用 --allow 白名单
// 按公钥放行客户端，文件在则公钥稳定，删除后下次使用生成全新身份。
const clientKeyRelPath = "remote/client.private.json"

// LoadOrCreateClientKey 读取持久化的客户端身份密钥；不存在时生成新密钥并落盘。
// 供无 Manager 的纯客户端路径（pipe/ssh/scp CLI）使用；Manager 内部走
// clientKeyLocked 惰性加载。返回零值 key.NodePrivate 表示调用方应回退为
// 每次连接生成临时身份。
func LoadOrCreateClientKey(stateDir string) (key.NodePrivate, error) {
	return loadOrCreateNodeKey(filepath.Join(stateDir, clientKeyRelPath))
}

// ValidateClientKey 严格解析 nodekey: 形式的客户端公钥（供服务端白名单使用）。
// 仅本地解析，不发起网络连接。
func ValidateClientKey(text string) (pub key.NodePublic, err error) {
	if err := pub.UnmarshalText([]byte(strings.TrimSpace(text))); err != nil {
		return pub, fmt.Errorf("客户端公钥无效（须为 nodekey: 形式）: %w", err)
	}
	return pub, nil
}

// ParseClientKey 解析 privkey: 形式的客户端私钥文本（临时身份/应急身份场景，
// 如 PROXYD_CLIENT_KEY 环境变量或 ssh --client-key 传入）。
func ParseClientKey(text string) (priv key.NodePrivate, err error) {
	if err := priv.UnmarshalText([]byte(strings.TrimSpace(text))); err != nil {
		return priv, fmt.Errorf("客户端私钥无效（须为 privkey: 形式的私钥）: %w", err)
	}
	return priv, nil
}

// GenerateClientKey 生成一对全新的客户端身份，返回文本形式（私钥, 公钥）。
// 供「proxyd remote genkey」生成应急身份：公钥录入对端白名单，私钥自行妥善保存。
func GenerateClientKey() (privText, pubText string) {
	priv := key.NewNode()
	privRaw, _ := priv.MarshalText()
	return string(privRaw), priv.Public().String()
}
