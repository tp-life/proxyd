package openvpn

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/youmark/pkcs8"
)

// TestParseProfileExtractsMihomoSubset 验证常见 .ovpn 会被转换为 mihomo 支持字段，
// 外部 auth-user-pass 只形成凭据提示，而不会尝试读取服务端文件系统路径。
//
// 参数说明：t 是 Go 测试上下文。
//
// 返回值说明：无；通过 Profile 字段与预览认证标志断言解析结果。
//
// 错误情况：remote 默认端口、协议别名、inline block、算法列表、key-direction 或
// 未支持指令警告丢失时测试失败。
func TestParseProfileExtractsMihomoSubset(t *testing.T) {
	raw := `
client
dev tun
proto tcp-client
remote "vpn.example.com"
auth-user-pass credentials.txt
cipher AES-256-GCM
data-ciphers AES-256-GCM:AES-128-GCM
auth SHA512
tls-auth [inline] 1
unknown-option value
<ca>
-----BEGIN CERTIFICATE-----
ZmFrZQ==
-----END CERTIFICATE-----
</ca>
<tls-auth>
-----BEGIN OpenVPN Static key V1-----
00
-----END OpenVPN Static key V1-----
</tls-auth>`
	profile, err := ParseProfile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Server != "vpn.example.com" || profile.Port != 1194 || profile.Proto != "tcp" {
		t.Fatalf("远端解析异常: %+v", profile)
	}
	if !profile.NeedsUserPass || profile.KeyDirection != "1" || profile.Cipher != "AES-256-GCM" {
		t.Fatalf("认证与算法解析异常: %+v", profile)
	}
	if len(profile.DataCiphers) != 2 || len(profile.Warnings) != 2 {
		t.Fatalf("算法列表或兼容性警告异常: ciphers=%v warnings=%v", profile.DataCiphers, profile.Warnings)
	}
	preview, err := PreviewProfile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.RequiresUserPassword || !preview.HasCA || preview.HasClientCertificate {
		t.Fatalf("安全预览异常: %+v", preview)
	}
}

// TestParseProfileRejectsExternalKeyMaterial 验证浏览器只上传单个 .ovpn 时，不会把
// 相对路径误当成 proxyd 服务端可读取的文件。
//
// 参数说明：t 是 Go 测试上下文。
//
// 返回值说明：无；外部 ca 和 dev tap 两种关键不兼容配置都必须返回明确错误。
//
// 错误情况：解析器静默接受外部材料会让创建结果必然不可用，因此视为测试失败。
func TestParseProfileRejectsExternalKeyMaterial(t *testing.T) {
	if _, err := ParseProfile("remote vpn.example.com 1194\nca ca.crt\nauth-user-pass"); err == nil || !strings.Contains(err.Error(), "外部文件") {
		t.Fatalf("外部 CA 应被拒绝，得到 %v", err)
	}
	if _, err := ParseProfile("remote vpn.example.com 1194\ndev tap\n<ca>\nx\n</ca>"); err == nil || !strings.Contains(err.Error(), "dev tun") {
		t.Fatalf("dev tap 应被拒绝，得到 %v", err)
	}
}

// TestNormalizeDecryptsAskPassPKCS8 验证现代 PKCS#8 加密私钥可以由 proxyd 使用
// askpass 解密，并且最终 mihomo 映射不包含原始口令或加密 PEM。
//
// 参数说明：t 是 Go 测试上下文。
//
// 返回值说明：无；规范化映射、默认组名、TUN 路由和口令清除均通过断言表达。
//
// 错误情况：密钥生成/加密属于测试准备错误；解密失败、错误口令未拒绝、口令落盘
// 或目标网段未规范化时测试失败。
func TestNormalizeDecryptsAskPassPKCS8(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	der, err := pkcs8.MarshalPrivateKey(key, []byte("ask-secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	encrypted := string(pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: der}))
	request := Setup{
		Name: "office", Server: "vpn.example.com", Port: 1194, Proto: "udp",
		CA:   "-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----",
		Cert: "-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----",
		Key:  encrypted, PrivateKeyPassphrase: "ask-secret", AccessMode: AccessModeBoth,
		Target: "10.20.30.40", UDP: true,
	}
	normalized, err := request.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	mapping := normalized.OutboundMapping()
	plainKey, _ := mapping["key"].(string)
	if !strings.Contains(plainKey, "BEGIN PRIVATE KEY") || strings.Contains(plainKey, "ENCRYPTED") {
		t.Fatalf("私钥未转换为未加密 PKCS#8: %q", plainKey)
	}
	if _, exists := mapping["private-key-passphrase"]; exists || normalized.PrivateKeyPassphrase != "" {
		t.Fatalf("askpass 不得进入持久化映射: setup=%+v mapping=%v", normalized.Setup, mapping)
	}
	if normalized.GroupName != "office-access" || normalized.Target != "10.20.30.40/32" || normalized.RouteRule() != "IP-CIDR,10.20.30.40/32,office-access,no-resolve" {
		t.Fatalf("一体化默认值异常: %+v rule=%q", normalized.Setup, normalized.RouteRule())
	}
	request.PrivateKeyPassphrase = "wrong"
	if _, err := request.Normalize(); err == nil || !strings.Contains(err.Error(), "askpass") {
		t.Fatalf("错误 askpass 应被拒绝，得到 %v", err)
	}
}

// TestDecryptPrivateKeySupportsTraditionalPEM 验证旧式 OpenVPN profile 常见的
// RFC 1423 `RSA PRIVATE KEY` 也能使用 askpass 解密，并能识别错误口令。
//
// 参数说明：t 是 Go 测试上下文。
//
// 返回值说明：无；正确口令必须产出未加密 PKCS#8 PEM，错误口令必须失败。
//
// 错误情况：测试密钥生成/加密失败属于准备错误；错误口令产生随机 DER 却被误判
// 成功时测试失败，防止传统无认证加密格式把垃圾私钥写入持久化配置。
func TestDecryptPrivateKeySupportsTraditionalPEM(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	block, err := x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key), []byte("legacy-secret"), x509.PEMCipherAES256) //nolint:staticcheck // 测试必须构造 OpenVPN 旧式加密私钥。
	if err != nil {
		t.Fatal(err)
	}
	raw := string(pem.EncodeToMemory(block))
	plain, err := decryptPrivateKey(raw, "legacy-secret")
	if err != nil || !strings.Contains(plain, "BEGIN PRIVATE KEY") {
		t.Fatalf("传统私钥解密失败: err=%v pem=%q", err, plain)
	}
	if _, err := decryptPrivateKey(raw, "wrong"); err == nil {
		t.Fatal("传统私钥错误口令必须被 DER 校验拒绝")
	}
}

// TestNormalizePreservesInlineUserPassword 验证 profile 已内联 auth-user-pass 时，空的
// 页面覆盖字段不会意外清除文件内密码。
//
// 参数说明：t 是 Go 测试上下文。
//
// 返回值说明：无；最终映射必须保留用户名和密码。
//
// 错误情况：解析或规范化失败、密码被空覆盖时测试失败。
func TestNormalizePreservesInlineUserPassword(t *testing.T) {
	request := Setup{Name: "password-only", Profile: `remote vpn.example.com 443 tcp
<ca>
-----BEGIN CERTIFICATE-----
ZmFrZQ==
-----END CERTIFICATE-----
</ca>
<auth-user-pass>
alice
secret
</auth-user-pass>`}
	normalized, err := request.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	mapping := normalized.OutboundMapping()
	if mapping["username"] != "alice" || mapping["password"] != "secret" {
		t.Fatalf("内联 auth-user-pass 被破坏: %#v", mapping)
	}
}
