package config

// 本文件定义「远程桌面」模块的持久化配置。桌面账号密码不属于 proxyd 的职责，
// 因此这里只保存连接目标与非敏感用户名；认证凭据继续交给系统 RDP/VNC 客户端管理。

import (
	"fmt"
	"strings"
)

const (
	// DesktopProtocolRDP 表示 Microsoft Remote Desktop Protocol。
	DesktopProtocolRDP = "rdp"
	// DesktopProtocolVNC 表示 Virtual Network Computing / Remote Framebuffer。
	DesktopProtocolVNC = "vnc"
	// DefaultDesktopRDPPort 是系统 RDP 服务最常见的 TCP 端口。
	DefaultDesktopRDPPort = 3389
	// DefaultDesktopVNCPort 是系统屏幕共享和 VNC 服务最常见的 TCP 端口。
	DefaultDesktopVNCPort = 5900
)

// DesktopConnection 是客户端保存的一条远程桌面连接档案。
//
// Name 是用户可识别的档案名称；Remote 只引用 remote.remotes 中的远端名称，避免在
// 桌面配置中再复制一份敏感 token。Username 可以为空，且不会保存密码。
type DesktopConnection struct {
	Name       string `yaml:"name" json:"name"`
	Remote     string `yaml:"remote" json:"remote"`
	Protocol   string `yaml:"protocol" json:"protocol"`
	RemotePort int    `yaml:"remote-port" json:"remote_port"`
	Username   string `yaml:"username,omitempty" json:"username,omitempty"`
}

// DesktopConfig 是「远程桌面」模块配置段。
//
// 服务端只保存实际操作系统服务端口；是否经 tailcat 开放仍以 RemoteConfig.Serve 为
// 唯一事实来源，避免同一开关在两个配置段产生漂移。
type DesktopConfig struct {
	RDPPort     int                 `yaml:"rdp-port,omitempty" json:"rdp_port"`
	VNCPort     int                 `yaml:"vnc-port,omitempty" json:"vnc_port"`
	Connections []DesktopConnection `yaml:"connections,omitempty" json:"connections"`
}

// ApplyDefaults 为缺失的服务端口和连接端口补齐协议默认值。
//
// 参数说明：无；接收者是刚解析或直接构造的桌面配置。
//
// 返回值说明：无；方法原地修改配置，使运行态、API 和后续持久化看到相同默认值。
//
// 错误情况：无；非法协议和越界端口由 Validate 统一拒绝，避免默认化过程吞掉错误。
func (d *DesktopConfig) ApplyDefaults() {
	if d.RDPPort == 0 {
		d.RDPPort = DefaultDesktopRDPPort
	}
	if d.VNCPort == 0 {
		d.VNCPort = DefaultDesktopVNCPort
	}
	for index := range d.Connections {
		if d.Connections[index].RemotePort == 0 {
			d.Connections[index].RemotePort = DefaultDesktopPort(d.Connections[index].Protocol)
		}
	}
}

// Clone 返回可独立参与配置事务的桌面配置副本。
//
// 参数说明：无。
//
// 返回值说明：DesktopConfig，其中 Connections 切片不与原配置共享底层数组。
//
// 错误情况：无；字符串与整数均为值类型，不需要更深层复制。
func (d DesktopConfig) Clone() DesktopConfig {
	out := d
	out.Connections = append([]DesktopConnection(nil), d.Connections...)
	return out
}

// DefaultDesktopPort 返回协议约定的默认服务端口。
//
// 参数说明：protocol 为 rdp 或 vnc，比较时忽略大小写和首尾空白。
//
// 返回值说明：RDP 返回 3389，VNC 返回 5900，未知协议返回 0。
//
// 错误情况：函数不返回错误；调用方应让 0 进入 Validate，从而得到带字段位置的错误。
func DefaultDesktopPort(protocol string) int {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case DesktopProtocolRDP:
		return DefaultDesktopRDPPort
	case DesktopProtocolVNC:
		return DefaultDesktopVNCPort
	default:
		return 0
	}
}

// ServicePort 返回指定协议当前配置的本机服务端口。
//
// 参数说明：protocol 为 rdp 或 vnc。
//
// 返回值说明：返回配置端口；零值配置按协议默认端口返回，未知协议返回 0。
//
// 错误情况：函数不返回错误；未知协议由调用方的协议值校验负责处理。
func (d DesktopConfig) ServicePort(protocol string) int {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case DesktopProtocolRDP:
		if d.RDPPort == 0 {
			return DefaultDesktopRDPPort
		}
		return d.RDPPort
	case DesktopProtocolVNC:
		if d.VNCPort == 0 {
			return DefaultDesktopVNCPort
		}
		return d.VNCPort
	default:
		return 0
	}
}

// SetServicePort 修改指定协议的本机服务端口。
//
// 参数说明：protocol 为 rdp 或 vnc；port 为 1-65535 的 TCP 端口。
//
// 返回值说明：error；协议和端口合法时写入配置并返回 nil。
//
// 错误情况：协议不受支持或端口越界时返回错误，接收者保持不变。
func (d *DesktopConfig) SetServicePort(protocol string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("桌面服务端口 %d 超出 1-65535", port)
	}
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case DesktopProtocolRDP:
		d.RDPPort = port
	case DesktopProtocolVNC:
		d.VNCPort = port
	default:
		return fmt.Errorf("桌面协议 %q 不受支持（可选 rdp 或 vnc）", protocol)
	}
	return nil
}

// Validate 校验服务端口与客户端连接档案的结构约束。
//
// 参数说明：无。
//
// 返回值说明：error；全部字段合法且档案名称唯一时返回 nil。
//
// 错误情况：服务端口越界、档案名称/远端为空、直接保存疑似 token、协议非法、
// 远端端口越界或档案重名时返回包含具体字段位置的错误。远端名称是否存在由应用层
// 在启动会话时解析，允许用户先保存档案、稍后再恢复被暂时删除的远端记录。
func (d DesktopConfig) Validate() error {
	rdpPort := d.ServicePort(DesktopProtocolRDP)
	vncPort := d.ServicePort(DesktopProtocolVNC)
	if rdpPort < 1 || rdpPort > 65535 {
		return fmt.Errorf("desktop.rdp-port %d 超出 1-65535", rdpPort)
	}
	if vncPort < 1 || vncPort > 65535 {
		return fmt.Errorf("desktop.vnc-port %d 超出 1-65535", vncPort)
	}
	if rdpPort == vncPort {
		return fmt.Errorf("desktop: RDP 与 VNC 不能共用服务端口 %d", rdpPort)
	}
	seen := make(map[string]bool, len(d.Connections))
	for index, connection := range d.Connections {
		name := strings.TrimSpace(connection.Name)
		if name == "" {
			return fmt.Errorf("desktop.connections[%d]: 名称不能为空", index)
		}
		if seen[name] {
			return fmt.Errorf("desktop.connections[%d]: 名称 %q 重复", index, name)
		}
		seen[name] = true
		remote := strings.TrimSpace(connection.Remote)
		if remote == "" {
			return fmt.Errorf("desktop.connections[%d]: 远端不能为空", index)
		}
		if looksLikeTailcatToken(remote) {
			return fmt.Errorf("desktop.connections[%d]: 远端必须引用 remote.remotes 名称，不能直接保存 tailcat token", index)
		}
		protocol := strings.ToLower(strings.TrimSpace(connection.Protocol))
		if protocol != DesktopProtocolRDP && protocol != DesktopProtocolVNC {
			return fmt.Errorf("desktop.connections[%d]: 协议 %q 不受支持", index, connection.Protocol)
		}
		port := connection.RemotePort
		if port == 0 {
			port = DefaultDesktopPort(protocol)
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("desktop.connections[%d]: 远端端口 %d 超出 1-65535", index, port)
		}
		if strings.ContainsAny(connection.Username, "\r\n") {
			return fmt.Errorf("desktop.connections[%d]: 用户名不能包含换行", index)
		}
	}
	return nil
}

// looksLikeTailcatToken 判断文本是否具有 tailcat 地址的敏感凭据外形。
//
// 参数说明：value 为桌面连接的远端引用。
//
// 返回值说明：bool；`tc` 前缀、足够长度且其余字符全为 base64url 字符时返回 true。
//
// 错误情况：无；这是配置边界的防泄漏识别，不替代 remote 包中的完整 CBOR/token
// 解析。较短的普通名称不会误判，疑似 token 则要求先保存到 remote.remotes 再引用名称。
func looksLikeTailcatToken(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 34 || !strings.HasPrefix(value, "tc") {
		return false
	}
	for _, character := range value[2:] {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}
