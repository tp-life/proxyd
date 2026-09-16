// Package openvpn 定义代理域内 OpenVPN 一体化接入与 .ovpn 导入规则。
//
// 本包只处理 OpenVPN 配置文本、认证材料和访问方式等纯业务语义，不依赖 HTTP、
// 配置文件或 mihomo 运行时。应用层负责把规范化结果提交为节点、策略组与 TUN 规则。
package openvpn

import (
	"bufio"
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/youmark/pkcs8"
)

// AccessMode 表示向本机应用暴露 OpenVPN 私网的方式。
type AccessMode string

const (
	// AccessModeProxy 只创建固定 mixed 代理入口，不修改系统透明路由。
	AccessModeProxy AccessMode = "proxy"
	// AccessModeTUN 启用 TUN，并仅接管用户明确填写的目标网段。
	AccessModeTUN AccessMode = "tun"
	// AccessModeBoth 同时创建代理入口和目标网段的 TUN 透明路由。
	AccessModeBoth AccessMode = "both"
)

// Setup 是一次 OpenVPN 一体化接入请求。
//
// Profile 保存用户上传的原始 .ovpn 文本；PrivateKeyPassphrase 只在本次请求中用于
// 解密私钥，规范化后的 mihomo 映射不会保存该口令。Username/Password 用于覆盖
// profile 中的 auth-user-pass，因为浏览器无法读取 .ovpn 引用的外部密码文件。
type Setup struct {
	Name                 string     `json:"name"`
	Profile              string     `json:"profile,omitempty"`
	Server               string     `json:"server,omitempty"`
	Port                 int        `json:"server_port,omitempty"`
	Proto                string     `json:"proto,omitempty"`
	CA                   string     `json:"ca,omitempty"`
	Cert                 string     `json:"cert,omitempty"`
	Key                  string     `json:"key,omitempty"`
	TLSAuth              string     `json:"tls_auth,omitempty"`
	TLSCrypt             string     `json:"tls_crypt,omitempty"`
	TLSCryptV2           string     `json:"tls_crypt_v2,omitempty"`
	KeyDirection         string     `json:"key_direction,omitempty"`
	Username             string     `json:"username,omitempty"`
	Password             string     `json:"password,omitempty"`
	PrivateKeyPassphrase string     `json:"private_key_passphrase,omitempty"`
	Cipher               string     `json:"cipher,omitempty"`
	DataCiphers          []string   `json:"data_ciphers,omitempty"`
	DataCipherFallback   string     `json:"data_cipher_fallback,omitempty"`
	Auth                 string     `json:"auth,omitempty"`
	CompLZO              string     `json:"comp_lzo,omitempty"`
	Ping                 int        `json:"ping,omitempty"`
	PingRestart          int        `json:"ping_restart,omitempty"`
	HandshakeTimeout     int        `json:"handshake_timeout,omitempty"`
	MTU                  int        `json:"mtu,omitempty"`
	UDP                  bool       `json:"udp"`
	DialerProxy          string     `json:"dialer_proxy,omitempty"`
	RemoteDNSResolve     bool       `json:"remote_dns_resolve"`
	DNS                  []string   `json:"dns,omitempty"`
	GroupName            string     `json:"group_name"`
	GroupPort            int        `json:"port,omitempty"`
	AccessMode           AccessMode `json:"access_mode"`
	Target               string     `json:"target,omitempty"`
}

// Profile 是从 .ovpn 中提取出的 mihomo 可承载配置子集。
//
// 未列出的 OpenVPN 指令不会被静默透传；ParseProfile 会把它们加入 Warnings，避免
// 用户误以为 mihomo 与原生 OpenVPN 客户端具备完全相同的配置面。
type Profile struct {
	Server             string
	Port               int
	Proto              string
	Dev                string
	CA                 string
	Cert               string
	Key                string
	TLSAuth            string
	TLSCrypt           string
	TLSCryptV2         string
	KeyDirection       string
	Username           string
	Password           string
	Cipher             string
	DataCiphers        []string
	DataCipherFallback string
	Auth               string
	CompLZO            string
	Ping               int
	PingRestart        int
	HandshakeTimeout   int
	MTU                int
	NeedsUserPass      bool
	NeedsAskPass       bool
	Warnings           []string
}

// ImportPreview 是 .ovpn 解析接口可安全返回的非敏感摘要。
//
// 证书、私钥和内联用户名密码不会出现在该结构中；前端只需据此填充地址并提示还需
// 哪些凭据，最终创建时继续提交原始 profile。
type ImportPreview struct {
	Server                 string   `json:"server"`
	Port                   int      `json:"port"`
	Proto                  string   `json:"proto"`
	HasCA                  bool     `json:"has_ca"`
	HasClientCertificate   bool     `json:"has_client_certificate"`
	RequiresUserPassword   bool     `json:"requires_user_password"`
	RequiresPrivateKeyPass bool     `json:"requires_private_key_passphrase"`
	Warnings               []string `json:"warnings,omitempty"`
}

// NormalizedSetup 是通过全部领域校验、可以交给应用层提交的接入值对象。
type NormalizedSetup struct {
	Setup
	Profile Profile
}

// PreviewProfile 解析 .ovpn，并只返回不含证书和凭据的摘要。
//
// 参数说明：raw 是浏览器读取的完整 .ovpn 文本。
//
// 返回值说明：ImportPreview 描述远端、协议和待补凭据；error 表示文本为空、缺少
// remote/ca 或引用了浏览器无法一并读取的外部密钥文件。
//
// 错误情况：解析失败不会返回半份敏感材料；调用方可安全把错误直接展示给用户。
func PreviewProfile(raw string) (ImportPreview, error) {
	profile, err := ParseProfile(raw)
	if err != nil {
		return ImportPreview{}, err
	}
	return ImportPreview{
		Server:                 profile.Server,
		Port:                   profile.Port,
		Proto:                  profile.Proto,
		HasCA:                  strings.TrimSpace(profile.CA) != "",
		HasClientCertificate:   strings.TrimSpace(profile.Cert) != "" && strings.TrimSpace(profile.Key) != "",
		RequiresUserPassword:   profile.NeedsUserPass && (strings.TrimSpace(profile.Username) == "" || profile.Password == ""),
		RequiresPrivateKeyPass: profile.NeedsAskPass || encryptedPrivateKey(profile.Key),
		Warnings:               append([]string(nil), profile.Warnings...),
	}, nil
}

// Normalize 清理请求、解析可选 profile、解密受 askpass 保护的私钥，并验证接入聚合。
//
// 参数说明：无；接收者来自 HTTP 请求，Profile 与手工字段二选一或由手工字段覆盖凭据。
//
// 返回值说明：NormalizedSetup 包含最终出站材料和规范化的组名、端口、目标网段；
// error 表示认证材料、私钥口令、端口或访问方式非法。
//
// 错误情况：私钥解密失败时不保留口令；TUN 模式没有明确目标时拒绝，避免一键接入
// 意外接管默认路由。auth-user-pass 与客户端证书可以同时存在，以兼容双重认证服务端。
func (s Setup) Normalize() (NormalizedSetup, error) {
	s.Name = strings.TrimSpace(s.Name)
	s.Profile = strings.TrimSpace(s.Profile)
	s.GroupName = strings.TrimSpace(s.GroupName)
	s.Target = strings.TrimSpace(s.Target)
	s.DialerProxy = strings.TrimSpace(s.DialerProxy)
	s.Username = strings.TrimSpace(s.Username)
	if s.Name == "" {
		return NormalizedSetup{}, fmt.Errorf("OpenVPN 节点名称不能为空")
	}

	profile := Profile{}
	var err error
	if s.Profile != "" {
		profile, err = ParseProfile(s.Profile)
		if err != nil {
			return NormalizedSetup{}, err
		}
	} else {
		profile = profileFromSetup(s)
	}
	if err := validateBaseProfile(profile); err != nil {
		return NormalizedSetup{}, err
	}
	if s.Username != "" {
		profile.Username = s.Username
		profile.Password = s.Password
	} else if s.Password != "" {
		profile.Password = s.Password
	}
	if profile.NeedsUserPass && (profile.Username == "" || profile.Password == "") {
		return NormalizedSetup{}, fmt.Errorf("该 OpenVPN 配置需要 auth-user-pass 用户名和密码")
	}
	if encryptedPrivateKey(profile.Key) {
		if s.PrivateKeyPassphrase == "" {
			return NormalizedSetup{}, fmt.Errorf("客户端私钥已加密，请填写 askpass 私钥口令")
		}
		profile.Key, err = decryptPrivateKey(profile.Key, s.PrivateKeyPassphrase)
		if err != nil {
			return NormalizedSetup{}, fmt.Errorf("askpass 私钥口令无效或加密格式不受支持: %w", err)
		}
	}
	if strings.TrimSpace(profile.Cert) == "" && profile.Username == "" {
		return NormalizedSetup{}, fmt.Errorf("OpenVPN 必须提供 cert+key 或 auth-user-pass 用户名")
	}
	if (strings.TrimSpace(profile.Cert) == "") != (strings.TrimSpace(profile.Key) == "") {
		return NormalizedSetup{}, fmt.Errorf("客户端 cert 与 key 必须同时提供")
	}
	if s.GroupName == "" {
		s.GroupName = s.Name + "-access"
	}
	if s.GroupName == s.Name {
		return NormalizedSetup{}, fmt.Errorf("策略组名称不能与 OpenVPN 节点名称相同")
	}
	if s.GroupPort < 0 || s.GroupPort > 65535 {
		return NormalizedSetup{}, fmt.Errorf("代理端口必须为 1-65535，留空时由 proxyd 自动分配")
	}
	if s.AccessMode == "" {
		s.AccessMode = AccessModeProxy
	}
	switch s.AccessMode {
	case AccessModeProxy:
		s.Target = ""
	case AccessModeTUN, AccessModeBoth:
		s.Target, err = normalizeTarget(s.Target)
		if err != nil {
			return NormalizedSetup{}, err
		}
	default:
		return NormalizedSetup{}, fmt.Errorf("访问方式 %q 无效（proxy|tun|both）", s.AccessMode)
	}
	s.Profile = ""
	s.PrivateKeyPassphrase = ""
	s.Key = ""
	return NormalizedSetup{Setup: s, Profile: profile}, nil
}

// UsesTUN 判断本次接入是否需要透明路由。
//
// 参数说明：无。
//
// 返回值说明：访问方式为 tun 或 both 时返回 true。
//
// 错误情况：无；非法模式已经在 Normalize 中拒绝。
func (s NormalizedSetup) UsesTUN() bool {
	return s.AccessMode == AccessModeTUN || s.AccessMode == AccessModeBoth
}

// RouteRule 生成把目标私网送往 OpenVPN 单成员策略组的 mihomo 规则。
//
// 参数说明：无；接收者必须来自 Normalize。
//
// 返回值说明：IPv4/IPv6 分别返回 IP-CIDR/IP-CIDR6 规则；代理端口模式返回空串。
//
// 错误情况：无；目标前缀已在 Normalize 中验证并标准化。
func (s NormalizedSetup) RouteRule() string {
	if !s.UsesTUN() || s.Target == "" {
		return ""
	}
	prefix, _ := netip.ParsePrefix(s.Target)
	ruleType := "IP-CIDR"
	if prefix.Addr().Is6() {
		ruleType = "IP-CIDR6"
	}
	return fmt.Sprintf("%s,%s,%s,no-resolve", ruleType, s.Target, s.GroupName)
}

// IsManagedRouteForGroup 判断规则是否由 OpenVPN 一体化接入为指定组生成。
//
// 参数说明：rule 是单条 mihomo 规则；groupName 是待删除接入的策略组名称。
//
// 返回值说明：严格匹配受管 IP-CIDR/IP-CIDR6 规则时返回 true。
//
// 错误情况：无；严格匹配可避免删除用户手工编写、恰好也引用该组的其它规则。
func IsManagedRouteForGroup(rule, groupName string) bool {
	parts := strings.Split(rule, ",")
	if len(parts) != 4 || strings.TrimSpace(parts[2]) != strings.TrimSpace(groupName) || strings.TrimSpace(parts[3]) != "no-resolve" {
		return false
	}
	ruleType := strings.TrimSpace(parts[0])
	prefix, err := netip.ParsePrefix(strings.TrimSpace(parts[1]))
	if err != nil {
		return false
	}
	return (ruleType == "IP-CIDR" && prefix.Addr().Is4()) || (ruleType == "IP-CIDR6" && prefix.Addr().Is6())
}

// OutboundMapping 把规范化值对象转换为 mihomo 原生 OpenVPN 出站映射。
//
// 参数说明：无；接收者必须通过 Normalize。
//
// 返回值说明：独立 map，包含 mihomo v1.19.30 支持的字段；askpass 不会写入映射。
//
// 错误情况：无；mihomo 的最终协议校验由应用层 ParseManualNode 与核心生成兜底。
func (s NormalizedSetup) OutboundMapping() map[string]any {
	p := s.Profile
	mapping := map[string]any{
		"name": s.Name, "type": "openvpn", "server": p.Server, "port": p.Port,
		"proto": p.Proto, "ca": p.CA, "udp": s.UDP,
	}
	putString(mapping, "dev", p.Dev)
	putString(mapping, "cert", p.Cert)
	putString(mapping, "key", p.Key)
	putString(mapping, "tls-auth", p.TLSAuth)
	putString(mapping, "tls-crypt", p.TLSCrypt)
	putString(mapping, "tls-crypt-v2", p.TLSCryptV2)
	putString(mapping, "key-direction", p.KeyDirection)
	putString(mapping, "username", p.Username)
	if p.Username != "" {
		mapping["password"] = p.Password
	}
	putString(mapping, "cipher", p.Cipher)
	if len(p.DataCiphers) > 0 {
		mapping["data-ciphers"] = append([]string(nil), p.DataCiphers...)
	}
	putString(mapping, "data-ciphers-fallback", p.DataCipherFallback)
	putString(mapping, "auth", p.Auth)
	putString(mapping, "comp-lzo", p.CompLZO)
	putPositive(mapping, "ping", p.Ping)
	putPositive(mapping, "ping-restart", p.PingRestart)
	putPositive(mapping, "handshake-timeout", p.HandshakeTimeout)
	putPositive(mapping, "mtu", p.MTU)
	putString(mapping, "dialer-proxy", s.DialerProxy)
	if s.RemoteDNSResolve {
		mapping["remote-dns-resolve"] = true
		if len(s.DNS) > 0 {
			mapping["dns"] = append([]string(nil), s.DNS...)
		}
	}
	return mapping
}

// ParseProfile 解析 mihomo 当前可承载的 .ovpn 子集。
//
// 参数说明：raw 是完整 OpenVPN 客户端配置文本，要求证书和密钥使用 inline block。
//
// 返回值说明：Profile 保存出站字段、认证需求与兼容性警告；error 表示缺少 remote、
// inline CA，使用 tap，或把证书/密钥放在浏览器无法访问的外部文件。
//
// 错误情况：未知但非关键指令进入 Warnings；会改变连接材料且无法读取的外部文件
// 直接拒绝，防止产生表面成功但必然无法连接的节点。
func ParseProfile(raw string) (Profile, error) {
	raw = strings.TrimPrefix(raw, "\uFEFF")
	if strings.TrimSpace(raw) == "" {
		return Profile{}, fmt.Errorf(".ovpn 文件内容不能为空")
	}
	inline, body, err := extractInlineBlocks(raw)
	if err != nil {
		return Profile{}, err
	}
	p := Profile{Proto: "udp", Dev: "tun", CA: inline["ca"], Cert: inline["cert"], Key: inline["key"], TLSAuth: inline["tls-auth"], TLSCrypt: inline["tls-crypt"], TLSCryptV2: inline["tls-crypt-v2"]}
	if credentials := strings.Split(strings.TrimSpace(inline["auth-user-pass"]), "\n"); len(credentials) >= 1 && strings.TrimSpace(credentials[0]) != "" {
		p.NeedsUserPass = true
		p.Username = strings.TrimSpace(credentials[0])
		if len(credentials) >= 2 {
			p.Password = strings.TrimSpace(credentials[1])
		}
	}

	knownIgnored := map[string]bool{"client": true, "nobind": true, "persist-key": true, "persist-tun": true, "resolv-retry": true, "verb": true, "mute": true, "auth-nocache": true, "pull": true, "explicit-exit-notify": true}
	external := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(body))
	// 内联证书通常多行，但供应商也可能把较长材料压成单行；放大 Scanner 上限与
	// API 文件边界一致，避免合法的 64 KiB 以上单行被默认限制误判为读取失败。
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		words, splitErr := splitDirective(scanner.Text())
		if splitErr != nil {
			return Profile{}, splitErr
		}
		if len(words) == 0 {
			continue
		}
		name := strings.ToLower(words[0])
		// arg 安全读取可选指令参数。
		// 参数 index 为零基字段下标；返回值为对应文本，越界返回空串；不会抛错。
		arg := func(index int) string {
			if index >= len(words) {
				return ""
			}
			return words[index]
		}
		switch name {
		case "remote":
			if p.Server == "" {
				p.Server = arg(1)
				p.Port, _ = strconv.Atoi(arg(2))
				if p.Port == 0 && arg(2) == "" {
					p.Port = 1194
				}
				if arg(3) != "" {
					p.Proto = normalizeProto(arg(3))
				}
			} else {
				p.Warnings = appendUnique(p.Warnings, "profile 包含多个 remote，mihomo 导入仅使用第一项")
			}
		case "proto":
			p.Proto = normalizeProto(arg(1))
		case "dev":
			p.Dev = strings.ToLower(arg(1))
		case "cipher":
			p.Cipher = strings.ToUpper(arg(1))
		case "data-ciphers":
			p.DataCiphers = splitCipherList(arg(1))
		case "data-ciphers-fallback":
			p.DataCipherFallback = strings.ToUpper(arg(1))
		case "auth":
			p.Auth = strings.ToUpper(arg(1))
		case "comp-lzo":
			p.CompLZO = strings.ToLower(arg(1))
			if p.CompLZO == "" {
				p.CompLZO = "adaptive"
			}
		case "key-direction":
			p.KeyDirection = arg(1)
		case "tls-auth":
			if p.TLSAuth == "" && arg(1) != "" && arg(1) != "[inline]" {
				external[name] = arg(1)
			}
			if arg(2) != "" {
				p.KeyDirection = arg(2)
			}
		case "tls-crypt", "tls-crypt-v2", "ca", "cert", "key":
			if inline[name] == "" && arg(1) != "" && arg(1) != "[inline]" {
				external[name] = arg(1)
			}
		case "auth-user-pass":
			p.NeedsUserPass = true
			if arg(1) != "" && arg(1) != "[inline]" {
				p.Warnings = append(p.Warnings, "auth-user-pass 引用了外部文件，创建时请在页面填写用户名和密码")
			}
		case "askpass":
			p.NeedsAskPass = true
		case "ping":
			p.Ping, _ = strconv.Atoi(arg(1))
		case "ping-restart":
			p.PingRestart, _ = strconv.Atoi(arg(1))
		case "keepalive":
			p.Ping, _ = strconv.Atoi(arg(1))
			p.PingRestart, _ = strconv.Atoi(arg(2))
		case "handshake-timeout":
			p.HandshakeTimeout, _ = strconv.Atoi(arg(1))
		case "tun-mtu", "mtu":
			p.MTU, _ = strconv.Atoi(arg(1))
		default:
			if !knownIgnored[name] {
				p.Warnings = appendUnique(p.Warnings, fmt.Sprintf("指令 %s 不在 mihomo OpenVPN 支持子集中，导入时已忽略", name))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Profile{}, fmt.Errorf("读取 .ovpn 失败: %w", err)
	}
	if p.Server == "" || p.Port <= 0 || p.Port > 65535 {
		return Profile{}, fmt.Errorf(".ovpn 必须包含有效的 remote 主机和端口")
	}
	if p.Dev != "tun" {
		return Profile{}, fmt.Errorf("mihomo OpenVPN 仅支持 dev tun，不支持 %q", p.Dev)
	}
	if p.Proto != "udp" && p.Proto != "tcp" {
		return Profile{}, fmt.Errorf("mihomo OpenVPN 仅支持 udp/tcp，不支持 %q", p.Proto)
	}
	if p.CA == "" {
		if path := external["ca"]; path != "" {
			return Profile{}, fmt.Errorf(".ovpn 的 ca 引用了外部文件 %q，请先把证书改为 <ca> 内联块", path)
		}
		return Profile{}, fmt.Errorf(".ovpn 缺少 <ca> 内联证书")
	}
	for _, name := range []string{"cert", "key", "tls-auth", "tls-crypt", "tls-crypt-v2"} {
		if path := external[name]; path != "" {
			return Profile{}, fmt.Errorf(".ovpn 的 %s 引用了外部文件 %q，请先改为 <%s> 内联块", name, path, name)
		}
	}
	return p, nil
}

// validateBaseProfile 校验手工表单与 .ovpn 共用的 OpenVPN 基础连接边界。
//
// 参数说明：profile 是解析或手工组装后的连接材料，尚未校验认证组合。
//
// 返回值说明：字段可交给 mihomo 时返回 nil，否则返回面向用户的具体错误。
//
// 错误情况：远端/端口/CA 缺失、协议或设备不支持，以及 tls-auth/tls-crypt 系列
// 同时配置时返回错误；这些问题不会通过补充用户名或 askpass 得到修复，必须先拒绝。
func validateBaseProfile(profile Profile) error {
	if strings.TrimSpace(profile.Server) == "" {
		return fmt.Errorf("OpenVPN 服务器地址不能为空")
	}
	if profile.Port <= 0 || profile.Port > 65535 {
		return fmt.Errorf("OpenVPN 服务器端口必须为 1-65535")
	}
	if profile.Proto != "udp" && profile.Proto != "tcp" {
		return fmt.Errorf("mihomo OpenVPN 仅支持 udp/tcp，不支持 %q", profile.Proto)
	}
	if profile.Dev != "" && profile.Dev != "tun" {
		return fmt.Errorf("mihomo OpenVPN 仅支持 dev tun，不支持 %q", profile.Dev)
	}
	if strings.TrimSpace(profile.CA) == "" {
		return fmt.Errorf("OpenVPN 必须提供 CA 证书材料")
	}
	wrapCount := 0
	for _, material := range []string{profile.TLSAuth, profile.TLSCrypt, profile.TLSCryptV2} {
		if strings.TrimSpace(material) != "" {
			wrapCount++
		}
	}
	if wrapCount > 1 {
		return fmt.Errorf("tls-auth、tls-crypt 与 tls-crypt-v2 只能配置一种")
	}
	return nil
}

// profileFromSetup 把旧式手工表单字段转换为与 .ovpn 解析相同的领域结构。
//
// 参数说明：s 是未规范化的接入请求。
//
// 返回值说明：Profile；文本字段会裁剪边缘空白，证书内部换行保持不变。
//
// 错误情况：无；必填和协议语义随后由 Normalize 与 mihomo 校验。
func profileFromSetup(s Setup) Profile {
	return Profile{Server: strings.TrimSpace(s.Server), Port: s.Port, Proto: normalizeProto(s.Proto), Dev: "tun", CA: strings.TrimSpace(s.CA), Cert: strings.TrimSpace(s.Cert), Key: strings.TrimSpace(s.Key), TLSAuth: strings.TrimSpace(s.TLSAuth), TLSCrypt: strings.TrimSpace(s.TLSCrypt), TLSCryptV2: strings.TrimSpace(s.TLSCryptV2), KeyDirection: strings.TrimSpace(s.KeyDirection), Username: strings.TrimSpace(s.Username), Password: s.Password, Cipher: strings.TrimSpace(s.Cipher), DataCiphers: append([]string(nil), s.DataCiphers...), DataCipherFallback: strings.TrimSpace(s.DataCipherFallback), Auth: strings.TrimSpace(s.Auth), CompLZO: strings.TrimSpace(s.CompLZO), Ping: s.Ping, PingRestart: s.PingRestart, HandshakeTimeout: s.HandshakeTimeout, MTU: s.MTU, NeedsUserPass: strings.TrimSpace(s.Username) != ""}
}

// extractInlineBlocks 提取 OpenVPN 的 XML 风格内联材料，并用空行替换原位置。
//
// 参数说明：raw 是完整配置文本。
//
// 返回值说明：map 按小写标签保存不含标签的内容；string 是供行指令解析的剩余文本；
// error 表示标签未闭合或出现不支持的嵌套。
//
// 错误情况：未闭合块直接拒绝，避免把后续指令误当成私钥或证书内容。
func extractInlineBlocks(raw string) (map[string]string, string, error) {
	blocks := make(map[string]string)
	var body strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(strings.ReplaceAll(raw, "\r\n", "\n")))
	// 与顶层指令扫描保持同一上限，兼容被压成单行的长证书或私钥材料。
	scanner.Buffer(make([]byte, 4096), 2<<20)
	active := ""
	var content strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if active == "" {
			if strings.HasPrefix(trimmed, "<") && strings.HasSuffix(trimmed, ">") && !strings.HasPrefix(trimmed, "</") {
				candidate := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(trimmed, "<"), ">"))
				if isInlineBlock(candidate) {
					active = candidate
					content.Reset()
					body.WriteByte('\n')
					continue
				}
			}
			body.WriteString(line)
			body.WriteByte('\n')
			continue
		}
		if strings.EqualFold(trimmed, "</"+active+">") {
			blocks[active] = strings.TrimSpace(content.String())
			active = ""
			continue
		}
		content.WriteString(line)
		content.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("读取 .ovpn 内联材料失败: %w", err)
	}
	if active != "" {
		return nil, "", fmt.Errorf(".ovpn 的 <%s> 内联块未闭合", active)
	}
	return blocks, body.String(), nil
}

// isInlineBlock 判断标签是否属于导入器支持的敏感材料类型。
//
// 参数说明：name 是已转换为小写的标签名。
//
// 返回值说明：受支持时返回 true。
//
// 错误情况：无；未知 XML 风格标签留给普通指令解析并形成兼容性警告。
func isInlineBlock(name string) bool {
	switch name {
	case "ca", "cert", "key", "tls-auth", "tls-crypt", "tls-crypt-v2", "auth-user-pass":
		return true
	default:
		return false
	}
}

// splitDirective 按 OpenVPN 配置的引号、反斜杠和注释规则拆分一行。
//
// 参数说明：line 是单行配置。
//
// 返回值说明：字段切片不含注释与引号；error 表示引号或转义未闭合。
//
// 错误情况：只有位于字段边界的 #/; 才开始注释，避免破坏口令或路径中的字符。
func splitDirective(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	quote := rune(0)
	escaped := false
	// flush 把当前已完成字段追加到结果。
	// 无参数、无返回值；空字段不会追加，也不会产生错误。
	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	for _, char := range line {
		if escaped {
			word.WriteRune(char)
			escaped = false
			continue
		}
		if char == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				word.WriteRune(char)
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if (char == '#' || char == ';') && word.Len() == 0 {
			break
		}
		if char == ' ' || char == '\t' {
			flush()
			continue
		}
		word.WriteRune(char)
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf(".ovpn 指令包含未闭合的引号或转义: %q", line)
	}
	flush()
	return words, nil
}

// decryptPrivateKey 使用 askpass 解密传统 PEM 或 PKCS#8 私钥。
//
// 参数说明：raw 是加密 PEM；passphrase 是本次请求中的 askpass 口令。
//
// 返回值说明：未加密的标准 PEM，可由 mihomo 的 tls.X509KeyPair 读取；error 表示
// PEM、口令或加密算法无效。
//
// 错误情况：传统 RFC 1423 与现代 PKCS#8 分别使用标准库和 pkcs8 实现；解密后统一
// 编码为 PRIVATE KEY，避免把旧加密头或口令继续写入 proxyd 配置。
func decryptPrivateKey(raw, passphrase string) (string, error) {
	block, rest := pem.Decode([]byte(raw))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return "", fmt.Errorf("客户端私钥不是单个有效 PEM 块")
	}
	var der []byte
	var err error
	switch {
	case x509.IsEncryptedPEMBlock(block):
		der, err = x509.DecryptPEMBlock(block, []byte(passphrase)) //nolint:staticcheck // 兼容 OpenVPN 传统 RFC 1423 私钥。
		if err == nil {
			var key any
			key, err = parsePrivateKeyDER(der)
			if err == nil {
				der, err = x509.MarshalPKCS8PrivateKey(key)
			}
		}
	case strings.Contains(block.Type, "ENCRYPTED PRIVATE KEY"):
		var key any
		key, err = pkcs8.ParsePKCS8PrivateKey(block.Bytes, []byte(passphrase))
		if err == nil {
			der, err = x509.MarshalPKCS8PrivateKey(key)
		}
	default:
		return raw, nil
	}
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// parsePrivateKeyDER 识别传统 PEM 解密后可能使用的三种常见私钥编码。
//
// 参数说明：der 是已经解密但尚未识别格式的 ASN.1 DER 数据。
//
// 返回值说明：返回 RSA/EC 或其它 PKCS#8 私钥对象，供调用方统一重编码为 PKCS#8。
//
// 错误情况：三种格式都无法解析时返回错误；这也用于识别传统 PEM 因错误 askpass
// 产生的随机明文，避免把未认证解密的垃圾数据当成成功结果写入配置。
func parsePrivateKeyDER(der []byte) (any, error) {
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("解密结果不是受支持的 PKCS#8、PKCS#1 或 EC 私钥")
}

// encryptedPrivateKey 判断 PEM 是否需要 askpass 解密。
//
// 参数说明：raw 是可选客户端私钥文本。
//
// 返回值说明：传统加密 PEM 或 ENCRYPTED PRIVATE KEY 返回 true。
//
// 错误情况：无；无法解码的文本返回 false，后续 mihomo 校验会提供具体 PEM 错误。
func encryptedPrivateKey(raw string) bool {
	block, _ := pem.Decode([]byte(raw))
	return block != nil && (x509.IsEncryptedPEMBlock(block) || strings.Contains(block.Type, "ENCRYPTED PRIVATE KEY"))
}

// normalizeProto 把 OpenVPN 常见协议别名收敛为 mihomo 支持的 udp/tcp。
//
// 参数说明：raw 是 proto 或 remote 第三个参数。
//
// 返回值说明：已识别别名返回 udp/tcp，未知值保留小写供上层报告。
//
// 错误情况：无。
func normalizeProto(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "udp", "udp4", "udp6":
		return "udp"
	case "tcp", "tcp-client", "tcp4", "tcp4-client", "tcp6", "tcp6-client":
		return "tcp"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

// splitCipherList 把 OpenVPN 冒号分隔的数据通道算法列表转换为 mihomo 切片。
//
// 参数说明：raw 是 data-ciphers 的单个参数。
//
// 返回值说明：去空、转大写且保持原始优先级的算法列表。
//
// 错误情况：无；具体算法支持范围由 mihomo 最终校验。
func splitCipherList(raw string) []string {
	var result []string
	for _, item := range strings.Split(raw, ":") {
		if item = strings.ToUpper(strings.TrimSpace(item)); item != "" {
			result = append(result, item)
		}
	}
	return result
}

// normalizeTarget 把单个私网地址或 CIDR 规范化为带掩码前缀。
//
// 参数说明：raw 是用户填写的 IPv4/IPv6 地址或 CIDR。
//
// 返回值说明：标准 CIDR 文本；error 表示目标为空或格式非法。
//
// 错误情况：单个 IP 自动转为 /32 或 /128；域名不接受，因为透明路由需要稳定边界。
func normalizeTarget(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("TUN 访问方式必须填写目标 IP 或 CIDR")
	}
	if prefix, err := netip.ParsePrefix(raw); err == nil {
		return prefix.Masked().String(), nil
	}
	address, err := netip.ParseAddr(raw)
	if err != nil {
		return "", fmt.Errorf("TUN 目标 %q 不是有效的 IP 或 CIDR", raw)
	}
	bits := 32
	if address.Is6() {
		bits = 128
	}
	return netip.PrefixFrom(address, bits).String(), nil
}

// putString 只把非空字符串写入 mihomo 映射，减少无意义配置噪声。
//
// 参数说明：mapping 是目标映射；key 是字段名；value 是候选值。
//
// 返回值说明：无。
//
// 错误情况：无；调用方保证 mapping 非 nil。
func putString(mapping map[string]any, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		mapping[key] = value
	}
}

// putPositive 只把正整数写入 mihomo 映射。
//
// 参数说明：mapping 是目标映射；key 是字段名；value 是秒数或 MTU。
//
// 返回值说明：无。
//
// 错误情况：无；负数会由上层或 mihomo 校验，零值表示采用默认行为。
func putPositive(mapping map[string]any, key string, value int) {
	if value > 0 {
		mapping[key] = value
	}
}

// appendUnique 在保持顺序的前提下追加一次兼容性警告。
//
// 参数说明：values 是已有警告；value 是候选警告。
//
// 返回值说明：原切片或追加后的新切片。
//
// 错误情况：无。
func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
