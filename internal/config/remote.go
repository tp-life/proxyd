package config

// 「远程连接」周边模块配置段：RemoteConfig/RemotePeer/RemoteForward 及其校验。

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RemoteAllowEntry 是一条客户端白名单授权：Key 是客户端 node 公钥，Name 是管理别名，
// ExpiresAt 与 Ports 分别收紧授权时间和目标端口；两者为空时保持永久、全部暴露端口可用。
type RemoteAllowEntry struct {
	Name      string     `yaml:"name,omitempty" json:"name,omitempty"`
	Key       string     `yaml:"key" json:"key"`
	ExpiresAt *time.Time `yaml:"expires-at,omitempty" json:"expires_at,omitempty"`
	Ports     []int      `yaml:"ports,omitempty" json:"ports,omitempty"`
}

// Clone 返回与原授权条目互不共享可变切片和时间指针的副本。
//
// 参数说明：无。
//
// 返回值说明：RemoteAllowEntry，可由配置事务独立修改的深拷贝。
//
// 错误情况：无；nil 的 ExpiresAt 与 Ports 会保持 nil。
func (e RemoteAllowEntry) Clone() RemoteAllowEntry {
	out := e
	out.Ports = append([]int(nil), e.Ports...)
	if e.ExpiresAt != nil {
		expiresAt := *e.ExpiresAt
		out.ExpiresAt = &expiresAt
	}
	return out
}

// IsExpired 判断授权在指定时刻是否已经到期。
//
// 参数说明：
//   - now: time.Time，调用方提供的当前时间，便于连接校验与测试使用同一时间基准。
//
// 返回值说明：bool，永久授权返回 false；到期时刻等于 now 也视为已过期。
//
// 错误情况：无；零值时间由配置校验拒绝，运行时仍会按已过期保守处理。
func (e RemoteAllowEntry) IsExpired(now time.Time) bool {
	return e.ExpiresAt != nil && !now.Before(*e.ExpiresAt)
}

// AllowsPort 判断授权是否允许访问一个目标端口。
//
// 参数说明：
//   - port: int，隧道连接请求的目标 TCP 端口。
//
// 返回值说明：bool，Ports 为空表示允许全部服务端暴露端口，否则仅精确匹配列表成员。
//
// 错误情况：无；非法端口不会命中，配置入口另行负责范围校验。
func (e RemoteAllowEntry) AllowsPort(port int) bool {
	if len(e.Ports) == 0 {
		return true
	}
	for _, allowedPort := range e.Ports {
		if allowedPort == port {
			return true
		}
	}
	return false
}

// UnmarshalYAML 兼容旧的纯字符串写法（- nodekey:...，无别名）。
func (e *RemoteAllowEntry) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		e.Name = ""
		e.Key = value.Value
		return nil
	}
	type plain RemoteAllowEntry
	return value.Decode((*plain)(e))
}

// UnmarshalJSON 同样兼容旧的纯字符串 JSON 形式。
func (e *RemoteAllowEntry) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		e.Name = ""
		return json.Unmarshal(data, &e.Key)
	}
	type plain RemoteAllowEntry
	return json.Unmarshal(data, (*plain)(e))
}

// RemotePeer 是一个保存的远程隧道端点（tailcat token 的命名别名），
// 供 forwards 与 CLI 按名称引用。
type RemotePeer struct {
	Name  string `yaml:"name" json:"name"`
	Token string `yaml:"token" json:"token"`
}

// RemoteForward 是把本地监听端口常驻转发到远端隧道端口的配置。
type RemoteForward struct {
	Name       string `yaml:"name" json:"name"`
	Listen     string `yaml:"listen" json:"listen"`                       // 本地监听地址（host:port；省略 host 时默认 127.0.0.1）
	Remote     string `yaml:"remote" json:"remote"`                       // remotes 中的名称，或直接的 tc... token
	RemotePort int    `yaml:"remote-port" json:"remote_port"`             // 远端隧道端口
	Enabled    *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"` // 缺失时按启用处理
}

// IsEnabled 返回该转发是否参与监听；enabled 缺失时保持默认启用行为。
func (f RemoteForward) IsEnabled() bool {
	return f.Enabled == nil || *f.Enabled
}

// RemoteConfig 是「远程连接」周边模块的配置段，基于 tailcat 数据面隧道，
// 与代理功能完全独立（不经过 mihomo）。
type RemoteConfig struct {
	Enabled    bool               `yaml:"enabled" json:"enabled"`                             // 隧道服务端总开关
	Region     string             `yaml:"region,omitempty" json:"region,omitempty"`           // 空=自动就近；数字=DERP 区域 ID；含 "."=自建 derper 主机名（逗号分隔）
	DERPMapURL string             `yaml:"derpmap-url,omitempty" json:"derpmap_url,omitempty"` // 自建 DERP map JSON 地址
	Serve      []int              `yaml:"serve,omitempty" json:"serve,omitempty"`             // 经隧道暴露的本机端口（连接转发到 127.0.0.1:port）
	Allow      []RemoteAllowEntry `yaml:"allow,omitempty" json:"allow,omitempty"`             // 允许的客户端公钥白名单（name 为可选管理别名）；空=放行所有持有 token 的客户端
	// AllowRestricted 区分“用户明确开启过白名单但授权已全部过期”和“从未配置白名单”。
	// 它避免清扫最后一条过期授权后意外回到开放模式；普通用户删除最后一条授权时会重置为 false。
	AllowRestricted bool            `yaml:"allow-restricted,omitempty" json:"allow_restricted,omitempty"`
	TempKey         string          `yaml:"temp-key,omitempty" json:"temp_key,omitempty"`         // 临时身份公钥（应急 nodekey，给客户端连入本机用；默认为空、只手动生成；与 allow 叠加生效，重置只替换它）
	KeyFile         string          `yaml:"key-file,omitempty" json:"key_file,omitempty"`         // 自定义服务端密钥文件（tailcat *.private.json，支持 ~/ 开头）；空=内置托管密钥 <state-dir>/remote/server.private.json
	BuiltinSSH      bool            `yaml:"builtin-ssh,omitempty" json:"builtin_ssh,omitempty"`   // 内嵌免密 SSH 服务：隧道 22 端口由进程内 SSH 服务器直接处理（隧道即认证），不再转发 127.0.0.1:22，无需系统 sshd
	WebTerminal     bool            `yaml:"web-terminal,omitempty" json:"web_terminal,omitempty"` // 浏览器终端总开关；默认关闭，且非回环 api-listen 开启时必须显式确认暴露风险
	Remotes         []RemotePeer    `yaml:"remotes,omitempty" json:"remotes,omitempty"`
	Forwards        []RemoteForward `yaml:"forwards,omitempty" json:"forwards,omitempty"`
}

// Clone 返回可独立修改的 RemoteConfig 副本，供热更新失败时回滚或跨锁读取。
//
// 参数说明：无。
//
// 返回值说明：RemoteConfig，切片、时间指针、端口切片和启用开关均不与原值共享。
//
// 错误情况：无；nil 指针保持 nil，兼容“字段缺失使用默认值”的配置语义。
func (r RemoteConfig) Clone() RemoteConfig {
	out := r
	out.Serve = append([]int(nil), r.Serve...)
	out.Allow = make([]RemoteAllowEntry, len(r.Allow))
	for index, entry := range r.Allow {
		out.Allow[index] = entry.Clone()
	}
	out.Remotes = append([]RemotePeer(nil), r.Remotes...)
	out.Forwards = make([]RemoteForward, len(r.Forwards))
	for index, forward := range r.Forwards {
		out.Forwards[index] = forward
		if forward.Enabled != nil {
			enabled := *forward.Enabled
			out.Forwards[index].Enabled = &enabled
		}
	}
	return out
}

// ClientWhitelistEnabled 返回服务端是否必须限制客户端身份。
//
// 参数说明：无。
//
// 返回值说明：bool；显式限制、现存 allow 条目或临时身份任一存在时返回 true。
//
// 错误情况：无；兼容旧配置时即使尚无 allow-restricted 字段，只要 allow 非空仍保持白名单模式。
func (r RemoteConfig) ClientWhitelistEnabled() bool {
	return r.AllowRestricted || len(r.Allow) > 0 || strings.TrimSpace(r.TempKey) != ""
}

// IsLoopbackAPIListen 判断管理 API 是否仅绑定本机回环地址。
//
// 参数说明：
//   - listen: string，配置中的 api-listen，通常为 host:port。
//
// 返回值说明：bool，仅 127.0.0.0/8、::1 或 localhost 返回 true；空 host、通配地址、
// 普通网卡 IP 与无法解析的文本均返回 false。
//
// 错误情况：无；格式非法时采取保守策略返回 false，具体地址合法性由配置总校验负责。
func IsLoopbackAPIListen(listen string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return false
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	// IPv6 zone 只对链路本地等地址有意义；先移除 zone 再解析，::1%zone 仍可按
	// 回环处理，而任何不可解析主机名都不会被乐观视为安全。
	if address, _, found := strings.Cut(host, "%"); found {
		host = address
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ValidateRemoteServe 校验经隧道暴露的本机端口列表（范围与去重），供 API/CLI 入口复用。
func ValidateRemoteServe(ports []int) error {
	seen := map[int]bool{}
	for i, p := range ports {
		if p <= 0 || p > 65535 {
			return fmt.Errorf("serve[%d]: 端口 %d 超出 1-65535", i, p)
		}
		if seen[p] {
			return fmt.Errorf("serve[%d]: 端口 %d 重复", i, p)
		}
		seen[p] = true
	}
	return nil
}

// ValidateRemoteAllow 校验客户端公钥白名单的结构（公钥 nodekey: 前缀、按公钥去重、
// 非空别名不重复）；密钥格式本身的严格解析由 remote 包（tailcat 依赖的唯一入口）兜底。
func ValidateRemoteAllow(entries []RemoteAllowEntry) error {
	seenKey := map[string]bool{}
	seenName := map[string]bool{}
	for i, e := range entries {
		k := strings.TrimSpace(e.Key)
		if !strings.HasPrefix(k, "nodekey:") {
			return fmt.Errorf("allow[%d]: 客户端公钥须为 nodekey: 开头，got %q", i, k)
		}
		if seenKey[k] {
			return fmt.Errorf("allow[%d]: 公钥重复", i)
		}
		seenKey[k] = true
		if e.ExpiresAt != nil && e.ExpiresAt.IsZero() {
			return fmt.Errorf("allow[%d]: expires-at 不能为零值时间", i)
		}
		if err := ValidateRemoteServe(e.Ports); err != nil {
			return fmt.Errorf("allow[%d].ports: %w", i, err)
		}
		if n := strings.TrimSpace(e.Name); n != "" {
			if seenName[n] {
				return fmt.Errorf("allow[%d]: 别名 %q 重复", i, n)
			}
			seenName[n] = true
		}
	}
	return nil
}

// checkRemote 校验 remote 配置段整体：端口范围、名称唯一性与转发字段。
func (c *Config) checkRemote() error {
	r := c.Remote
	if err := ValidateRemoteServe(r.Serve); err != nil {
		return fmt.Errorf("remote: %w", err)
	}
	if err := ValidateRemoteAllow(r.Allow); err != nil {
		return fmt.Errorf("remote: %w", err)
	}
	if t := strings.TrimSpace(r.TempKey); t != "" && !strings.HasPrefix(t, "nodekey:") {
		return fmt.Errorf("remote: temp-key 须为 nodekey: 形式的客户端公钥，got %q", t)
	}
	seenPeer := map[string]bool{}
	for i, p := range r.Remotes {
		if err := checkRemotePeer(p); err != nil {
			return fmt.Errorf("remote.remotes[%d]: %w", i, err)
		}
		if seenPeer[p.Name] {
			return fmt.Errorf("remote.remotes[%d]: 名称 %q 重复", i, p.Name)
		}
		seenPeer[p.Name] = true
	}
	seenFwd := map[string]bool{}
	seenListen := map[string]bool{}
	for i, f := range r.Forwards {
		if err := checkRemoteForward(f); err != nil {
			return fmt.Errorf("remote.forwards[%d]: %w", i, err)
		}
		if seenFwd[f.Name] {
			return fmt.Errorf("remote.forwards[%d]: 名称 %q 重复", i, f.Name)
		}
		listen, err := NormalizeRemoteListen(f.Listen)
		if err != nil {
			return fmt.Errorf("remote.forwards[%d]: %w", i, err)
		}
		if seenListen[listen] {
			return fmt.Errorf("remote.forwards[%d]: 监听地址 %s 重复", i, listen)
		}
		seenFwd[f.Name] = true
		seenListen[listen] = true
	}
	return nil
}

// checkRemotePeer 校验单个远端条目的必填字段；token 合法性由 remote 包解析兜底。
func checkRemotePeer(p RemotePeer) error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("名称不能为空")
	}
	if strings.TrimSpace(p.Token) == "" {
		return fmt.Errorf("token 不能为空")
	}
	return nil
}

// checkRemoteForward 校验单个转发条目的必填字段。
func checkRemoteForward(f RemoteForward) error {
	if strings.TrimSpace(f.Name) == "" {
		return fmt.Errorf("名称不能为空")
	}
	if strings.TrimSpace(f.Remote) == "" {
		return fmt.Errorf("remote 不能为空（remotes 名称或 tc... token）")
	}
	if f.RemotePort <= 0 || f.RemotePort > 65535 {
		return fmt.Errorf("remote-port %d 超出 1-65535", f.RemotePort)
	}
	if _, err := NormalizeRemoteListen(f.Listen); err != nil {
		return err
	}
	return nil
}

// CheckRemotePeer 校验新增远端：字段合法且不与现有远端重名。
func (c *Config) CheckRemotePeer(p RemotePeer) error {
	if err := checkRemotePeer(p); err != nil {
		return err
	}
	for _, e := range c.Remote.Remotes {
		if e.Name == p.Name {
			return fmt.Errorf("远端 %q 已存在", p.Name)
		}
	}
	return nil
}

// CheckRemoteForward 校验新增转发：字段合法，且不与现有转发重名/重复监听。
func (c *Config) CheckRemoteForward(f RemoteForward) error {
	if err := checkRemoteForward(f); err != nil {
		return err
	}
	listen, _ := NormalizeRemoteListen(f.Listen)
	for _, e := range c.Remote.Forwards {
		if e.Name == f.Name {
			return fmt.Errorf("转发 %q 已存在", f.Name)
		}
		if el, err := NormalizeRemoteListen(e.Listen); err == nil && el == listen {
			return fmt.Errorf("监听地址 %s 已被转发 %q 占用", listen, e.Name)
		}
	}
	return nil
}

// NormalizeRemoteListen 把转发监听地址规范化为 host:port；省略 host 时补 127.0.0.1。
func NormalizeRemoteListen(listen string) (string, error) {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return "", fmt.Errorf("listen 不能为空")
	}
	host, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		// 允许只写端口号（如 "2222"），统一落到回环地址。
		if p, perr := strconv.Atoi(listen); perr == nil && p > 0 && p <= 65535 {
			return net.JoinHostPort(defaultListen, strconv.Itoa(p)), nil
		}
		return "", fmt.Errorf("listen %q 不是合法的 host:port", listen)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 || p > 65535 {
		return "", fmt.Errorf("listen %q 端口超出 1-65535", listen)
	}
	if host == "" {
		listen = net.JoinHostPort(defaultListen, portStr)
	}
	return listen, nil
}
