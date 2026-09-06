package remote

// 本文件验证公钥导入边界，以及 tailcat v0.6 升级前后的持久身份兼容性。

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tailscale/tailcat"
	"golang.org/x/crypto/ssh"

	"proxyd/internal/config"
)

// newSSHTestSigner 生成仅用于测试的 ed25519 客户端私钥及其公钥文本。
// 参数说明：t 为 *testing.T，用于报告随机数或密钥转换错误。
// 返回值说明：ssh.Signer 和 string，不读写任何用户密钥。
// 错误情况：随机数或 signer 创建失败时终止测试。
func newSSHTestSigner(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

// TestSSHKeyImportValidation 验证公钥文件导入与 tailcat 的严格 authorized_keys 语义一致。
// 参数说明：t 为 *testing.T，构造有效、公钥重复、受限选项和误上传私钥的输入。
// 返回值说明：无。
// 错误情况：格式损坏被忽略、重复授权被接受或错误包含私钥内容时失败。
func TestSSHKeyImportValidation(t *testing.T) {
	_, first := newSSHTestSigner(t)
	_, second := newSSHTestSigner(t)
	entries, err := ParseSSHAuthorizedKeys("# 注释\r\n\n"+first+" laptop\r\n"+second+" desktop\n", "")
	if err != nil || len(entries) != 2 || entries[0].Name != "laptop" {
		t.Fatalf("多行公钥解析错误：%v，%+v", err, entries)
	}
	if _, err := NormalizeSSHKeys(append(entries, config.RemoteSSHKey{Name: "different", PublicKey: first + " other"})); err == nil {
		t.Fatal("同一公钥更改注释后不应绕过去重")
	}
	for _, input := range []string{"", "# 只有注释", "invalid", first + "\ninvalid", `command="id" ` + first, "-----BEGIN OPENSSH PRIVATE KEY-----\nDO_NOT_LEAK_PRIVATE\n", strings.Repeat("x", 64*1024+1)} {
		if _, err := ParseSSHAuthorizedKeys(input, ""); err == nil {
			t.Fatal("非法公钥输入被接受")
		} else if strings.Contains(err.Error(), "DO_NOT_LEAK_PRIVATE") {
			t.Fatal("校验错误泄露了输入中的私钥")
		}
	}
	if _, err := NormalizeSSHKeys([]config.RemoteSSHKey{{PublicKey: first + "\n" + second}}); err == nil {
		t.Fatal("配置中一个条目不得隐含多个无法逐项撤销的公钥")
	}
}

// TestTailcatIdentityPSKPersistence 验证新版 PSK 跨加载稳定，旧文件不被自动迁移失效。
// 参数说明：t 为 *testing.T，提供临时密钥文件目录。
// 返回值说明：无。
// 错误情况：新文件丢失 PSK、重载 token 变化、旧文件被重写或零私钥被接受时失败。
func TestTailcatIdentityPSKPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.private.json")
	first, err := loadOrCreateTailcatKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Public.PresharedKey.IsZero() {
		t.Fatal("v0.6 新身份应持久化 PSK")
	}
	second, err := loadOrCreateTailcatKey(path)
	if err != nil {
		t.Fatal(err)
	}
	first.Public.RegionID, second.Public.RegionID = 1, 1
	if first.Public.ConnBlob() != second.Public.ConnBlob() || !first.Private.Equal(second.Private) {
		t.Fatal("新身份在重新加载后发生变化")
	}
	// 旧文件缺少 PSK，保持字节与身份不变，不能让升级成为隐式密钥轮换。
	first.Public.PresharedKey = tailcat.PresharedKey{}
	data, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteServerKeyData(path, data); err != nil {
		t.Fatal(err)
	}
	legacy, err := loadOrCreateTailcatKey(path)
	if err != nil || !legacy.Public.PresharedKey.IsZero() || legacy.Public.ConnBlob() != first.Public.ConnBlob() {
		t.Fatalf("旧身份未保持兼容：%v", err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != string(data) {
		t.Fatal("读取旧身份不应修改文件")
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateTailcatKey(path); err == nil {
		t.Fatal("缺少节点私钥不能隐式生成临时服务身份")
	}
}
