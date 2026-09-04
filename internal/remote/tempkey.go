package remote

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

// tempKeyRelPath 是临时身份（应急 nodekey）私钥在 stateDir 下的相对路径。
// 公钥登记在配置 remote.temp-key 里（与白名单叠加生效），私钥只落在这里；
// 重置即重新生成并覆盖该文件，旧私钥立即失效，不影响手动添加的白名单条目。
const tempKeyRelPath = "remote/tempkey.private.json"

// LoadTempKey 读取临时身份密钥，返回文本形式（私钥, 公钥）。
// 文件不存在或内容非法时返回错误，调用方据此提示「尚未生成」。
func LoadTempKey(stateDir string) (privText, pubText string, err error) {
	path := filepath.Join(stateDir, tempKeyRelPath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", fmt.Errorf("临时身份尚未生成")
		}
		return "", "", fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	var saved tailcat.PrivateKey
	if err := json.Unmarshal(data, &saved); err != nil {
		return "", "", fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	privRaw, _ := saved.Private.MarshalText()
	return string(privRaw), saved.Private.Public().String(), nil
}

// ResetTempKey 生成全新的临时身份并覆盖落盘（0600），返回新公钥文本。
func ResetTempKey(stateDir string) (pubText string, err error) {
	path := filepath.Join(stateDir, tempKeyRelPath)
	fresh := tailcat.NewPrivateKey()
	data, err := json.MarshalIndent(fresh, "", "\t")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return fresh.Private.Public().String(), nil
}

// tcAddrForKey 复刻 tailcat 未导出的节点隧道地址推导算法。
//
// 参数 k 是已经完成格式校验的 Tailscale 节点公钥；函数取公钥原始编码的前 10 字节，
// 与 fd7a:115c:a1e0::/48 前缀组合为节点地址，用于把入站连接归属到白名单身份。
// 返回值始终是有效 IPv6 地址，不会返回错误。这里使用 AppendTo 获取无类型前缀的原始
// 32 字节，避免依赖已弃用的 Raw32；tailcat 依赖升级时仍需核对地址算法是否发生变化。
func tcAddrForKey(k key.NodePublic) netip.Addr {
	var a [16]byte
	r := k.AppendTo(nil)
	a[0] = 0xfd
	a[1] = 0x7a
	a[2] = 0x11
	a[3] = 0x5c
	a[4] = 0xa1
	a[5] = 0xe0
	copy(a[6:], r[:10])
	return netip.AddrFrom16(a)
}

// knownClientAddrs 把白名单与临时身份公钥映射为隧道地址，供入站连接归属统计。
// 非法公钥跳过（启动时已经过 ValidateClientKey 校验，这里只是防御）。
func knownClientAddrs(allow []string, tempKey string) map[netip.Addr]string {
	out := map[netip.Addr]string{}
	for _, text := range append(append([]string(nil), allow...), tempKey) {
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		pub, err := ValidateClientKey(text)
		if err != nil {
			continue
		}
		out[tcAddrForKey(pub)] = pub.String()
	}
	return out
}
