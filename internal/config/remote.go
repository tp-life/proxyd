package config

// 「远程连接」周边模块配置段：RemoteConfig/RemotePeer/RemoteForward 及其校验。

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

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
	Enabled    bool            `yaml:"enabled" json:"enabled"`                             // 隧道服务端总开关
	Region     string          `yaml:"region,omitempty" json:"region,omitempty"`           // 空=自动就近；数字=DERP 区域 ID；含 "."=自建 derper 主机名（逗号分隔）
	DERPMapURL string          `yaml:"derpmap-url,omitempty" json:"derpmap_url,omitempty"` // 自建 DERP map JSON 地址
	Serve      []int           `yaml:"serve,omitempty" json:"serve,omitempty"`             // 经隧道暴露的本机端口（连接转发到 127.0.0.1:port）
	Allow      []string        `yaml:"allow,omitempty" json:"allow,omitempty"`             // 允许的客户端 node 公钥白名单；空=放行所有持有 token 的客户端
	TempKey    string          `yaml:"temp-key,omitempty" json:"temp_key,omitempty"`       // 临时身份公钥（应急 nodekey，给客户端连入本机用；默认为空、只手动生成；与 allow 叠加生效，重置只替换它）
	Remotes    []RemotePeer    `yaml:"remotes,omitempty" json:"remotes,omitempty"`
	Forwards   []RemoteForward `yaml:"forwards,omitempty" json:"forwards,omitempty"`
}

// Clone 返回可独立修改的 RemoteConfig 副本，供热更新失败时回滚。
func (r RemoteConfig) Clone() RemoteConfig {
	out := r
	out.Serve = append([]int(nil), r.Serve...)
	out.Allow = append([]string(nil), r.Allow...)
	out.Remotes = append([]RemotePeer(nil), r.Remotes...)
	out.Forwards = append([]RemoteForward(nil), r.Forwards...)
	return out
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

// ValidateRemoteAllow 校验客户端公钥白名单的结构（非空、nodekey: 前缀、去重）；
// 密钥格式本身的严格解析由 remote 包（tailcat 依赖的唯一入口）兜底。
func ValidateRemoteAllow(keys []string) error {
	seen := map[string]bool{}
	for i, k := range keys {
		k = strings.TrimSpace(k)
		if !strings.HasPrefix(k, "nodekey:") {
			return fmt.Errorf("allow[%d]: 客户端公钥须为 nodekey: 开头，got %q", i, k)
		}
		if seen[k] {
			return fmt.Errorf("allow[%d]: 公钥重复", i)
		}
		seen[k] = true
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
