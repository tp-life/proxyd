// Package tunhelper 是 proxy 子域的 root 特权 helper：macOS LaunchDaemon 常驻，
// 替主进程创建 utun 设备、配置 MTU/地址/路由，并经 SCM_RIGHTS 把 utun 文件描述符
// 回传给主进程（主进程随后注入内嵌 mihomo 的 tun.file-descriptor）。
// helper 发出 fd 后即关闭自己的副本：主进程关闭 fd 时内核自动销毁 utun 与路由，
// 因此协议无 destroy 指令，服务端也无看门狗。
package tunhelper

// helper 换行分隔 JSON IPC 协议与指令分发（协议骨架照搬 internal/gateway/helperproto.go）。
// 本文件不含平台代码，保证协议与分发的单测在 darwin 之外也能运行；
// fd 回传机制见 fdpass_unix.go（darwin+linux）与 fdpass_other.go 存根，
// 平台执行能力（utun 创建/路由）经 helperOps 接口注入。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// helperSocketPath 是 helper 的 unix socket 地址（0600 + 对端属主校验由服务端保证）。
// 不放 state-dir（root helper 不应写用户目录），路径形态与系统域服务约定一致。
const helperSocketPath = "/var/run/com.proxyd.tun.sock"

// helperProtocolVersion 是协议主版本；握手时主版本不一致即拒绝并提示重装 helper。
const helperProtocolVersion = 1

// 白名单指令集；服务端按名分发，白名单外一律拒绝。
// 无 destroy：主进程关闭 fd 即销毁 utun。
const (
	helperOpPing      = "ping"
	helperOpTunCreate = "tun.create"
)

// ErrUnsupported 表示当前平台不支持 tun 特权 helper（非 macOS 平台）。
var ErrUnsupported = errors.New("当前平台不支持 tun 特权 helper")

// ErrHelperUnavailable 表示 macOS tun 特权 helper 不可达（未安装或未运行）。
// 编排层用 errors.Is 识别该哨兵，把 TUN 模式映射为 degraded + 安装指引而非 failed。
var ErrHelperUnavailable = errors.New("tun 特权 helper 不可用")

// helperRequest 是一条下发给 helper 的指令；params 按 op 严格反序列化到固定结构。
type helperRequest struct {
	Version int             `json:"version"`
	Op      string          `json:"op"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// helperResponse 是 helper 的应答；version 用于握手兼容性校验。
type helperResponse struct {
	Version int    `json:"version"`
	OK      bool   `json:"ok"`
	Result  string `json:"result,omitempty"` // tun.create 的接口名（如 "utun5"）
	Error   string `json:"error,omitempty"`
}

// CreateTUNParams 是 tun.create 的参数（跨平台定义，darwin 实现见 server_darwin.go）。
type CreateTUNParams struct {
	MTU             int      `json:"mtu"`                         // 必填，>0
	Inet4Address    string   `json:"inet4_address"`               // 必填，IPv4 CIDR（如 "198.18.0.1/30"）
	Inet6Address    string   `json:"inet6_address,omitempty"`     // 可空，IPv6 CIDR
	AutoRoute       bool     `json:"auto_route"`                  // 是否写入系统路由表
	AutoRouteRanges []string `json:"auto_route_ranges,omitempty"` // v4 CIDR 子段（app 层预算好）
	ExcludeRanges   []string `json:"exclude_ranges,omitempty"`    // 可空，从 AutoRouteRanges 中剔除
}

// tunConfig 是 CreateTUNParams 解析后的类型化形式（routes 已剔除 exclude_ranges）。
type tunConfig struct {
	mtu       int
	inet4     netip.Prefix
	inet6     netip.Prefix // 无效值表示未配置
	autoRoute bool
	routes    []netip.Prefix
}

// Validate 校验 tun.create 参数合法性。
//
// 参数：无（接收者即待校验参数）。
//
// 返回值：error，任一字段非法时返回中文原因。
//
// 错误情况：MTU 越界、CIDR 解析失败、地址族不符、auto_route 开启但无路由段。
func (p CreateTUNParams) Validate() error {
	_, err := p.parse()
	return err
}

// parse 校验并解析为类型化配置；auto_route 时对 auto_route_ranges 剔除 exclude_ranges。
// app 层已把排除段预算成对齐子段，这里按「exclude 完整覆盖 range」剔除即可。
func (p CreateTUNParams) parse() (tunConfig, error) {
	cfg := tunConfig{mtu: p.MTU, autoRoute: p.AutoRoute}
	if p.MTU <= 0 || p.MTU > 65535 {
		return cfg, fmt.Errorf("mtu %d 非法（须在 1-65535）", p.MTU)
	}
	inet4, err := netip.ParsePrefix(strings.TrimSpace(p.Inet4Address))
	if err != nil || !inet4.Addr().Is4() {
		return cfg, fmt.Errorf("inet4_address %q 非法（须为 IPv4 CIDR）", p.Inet4Address)
	}
	cfg.inet4 = inet4
	if raw := strings.TrimSpace(p.Inet6Address); raw != "" {
		inet6, err := netip.ParsePrefix(raw)
		if err != nil || !inet6.Addr().Is6() || inet6.Addr().Is4In6() {
			return cfg, fmt.Errorf("inet6_address %q 非法（须为 IPv6 CIDR）", p.Inet6Address)
		}
		cfg.inet6 = inet6
	}
	var excludes []netip.Prefix
	for _, raw := range p.ExcludeRanges {
		exclude, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil || !exclude.Addr().Is4() {
			return cfg, fmt.Errorf("exclude_ranges 项 %q 非法（须为 IPv4 CIDR）", raw)
		}
		excludes = append(excludes, exclude)
	}
	if p.AutoRoute {
		if len(p.AutoRouteRanges) == 0 {
			return cfg, fmt.Errorf("auto_route 开启时 auto_route_ranges 不能为空")
		}
		for _, raw := range p.AutoRouteRanges {
			routeRange, err := netip.ParsePrefix(strings.TrimSpace(raw))
			if err != nil || !routeRange.Addr().Is4() {
				return cfg, fmt.Errorf("auto_route_ranges 项 %q 非法（须为 IPv4 CIDR）", raw)
			}
			if coveredByAny(routeRange, excludes) {
				continue
			}
			cfg.routes = append(cfg.routes, routeRange)
		}
	}
	return cfg, nil
}

// coveredByAny 报告 prefix 是否被某个 exclude 完整覆盖。
func coveredByAny(prefix netip.Prefix, excludes []netip.Prefix) bool {
	for _, exclude := range excludes {
		if exclude.Bits() <= prefix.Bits() && exclude.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

// helperOps 是 helper 服务端执行白名单指令的平台能力接口（darwin 实现见
// server_darwin.go；测试注入 fake）。
type helperOps interface {
	// Ping 是握手探活。
	Ping() error
	// CreateTUN 创建 utun 设备并配置 MTU/地址/路由，返回设备 fd 与接口名。
	// 调用方（serve 层）负责把 fd 回传给客户端并关闭 helper 侧副本。
	CreateTUN(params CreateTUNParams) (fd int, ifName string, err error)
}

// decodeHelperParams 把 params 严格反序列化到固定结构：未知字段拒绝，
// 防止客户端借额外字段注入服务端未审阅的语义。
//
// 参数：
//   - raw: json.RawMessage，请求携带的 params；空值视为无参数。
//   - out: any，目标固定结构指针。
//
// 返回值：error，params 缺失、JSON 非法或含未知字段时返回原因。
//
// 错误情况：无参数要求的指令不应调用本函数。
func decodeHelperParams(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return fmt.Errorf("缺少 params")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("params 非法: %w", err)
	}
	return nil
}

// dispatchHelperRequest 按白名单分发一条指令。
//
// 参数：
//   - req: helperRequest，已解码的指令。
//   - ops: helperOps，平台执行能力。
//
// 返回值：
//   - helperResponse：任何失败都收敛为 OK=false + 中文错误，不向客户端回传 panic。
//   - int：tun.create 成功时待回传的设备 fd（>=0），否则 -1；调用方负责发送后关闭。
//
// 错误情况：版本不兼容、未知指令、params 非法或执行失败均返回 OK=false。
func dispatchHelperRequest(req helperRequest, ops helperOps) (helperResponse, int) {
	resp := helperResponse{Version: helperProtocolVersion}
	fail := func(format string, args ...any) (helperResponse, int) {
		resp.Error = fmt.Sprintf(format, args...)
		return resp, -1
	}
	if req.Version != helperProtocolVersion {
		return fail("协议主版本不兼容（helper %d，客户端 %d）：请重新安装 helper 或升级 proxyd", helperProtocolVersion, req.Version)
	}
	switch req.Op {
	case helperOpPing:
		if err := ops.Ping(); err != nil {
			return fail("ping 执行失败: %v", err)
		}
		resp.OK = true
		resp.Result = "pong"
		return resp, -1
	case helperOpTunCreate:
		var params CreateTUNParams
		if err := decodeHelperParams(req.Params, &params); err != nil {
			return fail("tun.create %v", err)
		}
		if err := params.Validate(); err != nil {
			return fail("tun.create 参数非法: %v", err)
		}
		fd, ifName, err := ops.CreateTUN(params)
		if err != nil {
			return fail("tun.create 执行失败: %v", err)
		}
		resp.OK = true
		resp.Result = ifName
		return resp, fd
	default:
		return fail("未知指令 %q（不在白名单）", req.Op)
	}
}

// serveHelperConn 处理一条已授权的 helper 连接：循环读取换行 JSON 请求并回写应答；
// tun.create 成功时先写应答，等客户端回一个确认字节后再经 SCM_RIGHTS 回传 fd。
//
// 参数：
//   - conn: net.Conn，已通过属主校验的 unix socket 连接。
//   - ops: helperOps，平台执行能力。
//
// 返回值：无；连接生命周期随对端关闭结束。
//
// 错误情况：解码失败（含 EOF）静默结束连接；fd 发送失败即断开连接；
// 任何路径下 helper 侧 fd 副本都会被关闭。
func serveHelperConn(conn net.Conn, ops helperOps) {
	reader := bufio.NewReader(conn)
	decoder := json.NewDecoder(reader)
	encoder := json.NewEncoder(conn)
	for {
		var req helperRequest
		if err := decoder.Decode(&req); err != nil {
			return
		}
		resp, fd := dispatchHelperRequest(req, ops)
		if err := encoder.Encode(resp); err != nil {
			if fd >= 0 {
				closeHelperFD(fd)
			}
			return
		}
		if fd < 0 {
			continue
		}
		// 客户端解码器可能缓冲超读：等其确认应答已读完再发 fd，
		// 避免带 SCM_RIGHTS 的数据字节被缓冲吞掉导致 fd 丢失。
		if _, err := reader.ReadByte(); err != nil {
			closeHelperFD(fd)
			return
		}
		if err := sendHelperFD(conn, fd); err != nil {
			closeHelperFD(fd)
			return
		}
		// helper 不持有副本：主进程退出/关闭 fd 时内核自动销毁 utun 与路由。
		closeHelperFD(fd)
	}
}

// HelperStatus 是 helper 安装链路的自查结果（本地读取，不经 socket 鉴权）。
type HelperStatus struct {
	Installed bool   `json:"installed"` // plist 已安装
	Running   bool   `json:"running"`   // launchd 已加载该服务
	Reachable bool   `json:"reachable"` // socket 握手可达（协议版本兼容）
	Detail    string `json:"detail,omitempty"`
}
