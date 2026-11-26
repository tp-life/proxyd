package gateway

// helper 换行分隔 JSON IPC 协议与指令分发（docs/adr/0003）。
// 本文件不含平台代码，保证协议与分发的单测在 darwin 之外也能运行；
// 平台执行能力（pfctl/sysctl/socket）经 helperOps 接口注入。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"proxyd/internal/config"
)

// helperSocketPath 是 helper 的 unix socket 地址（0600 + 对端属主校验由服务端保证）。
// 不放 state-dir（root helper 不应写用户目录），路径形态与系统域服务约定一致。
const helperSocketPath = "/var/run/com.proxyd.gateway.sock"

// helperProtocolVersion 是协议主版本；握手时主版本不一致即拒绝并提示重装 helper。
// v2 起 pf.apply 改为结构化参数（设备 IPv4 列表 + 端口 + DNS 开关），由 root helper
// 内部渲染 pf anchor，彻底杜绝任意 pf 规则注入与 include 读文件风险。
const helperProtocolVersion = 2

const (
	// maxHelperRequestBytes 限制单条 helper 请求体积（64 KiB），防止内存/磁盘耗尽。
	maxHelperRequestBytes = 64 << 10
	// helperReadTimeout 限制单条 helper 请求读取超时，避免空闲或慢速连接永久挂起。
	helperReadTimeout = 10 * time.Second
	// maxHelperPFDevices 限制单次下发的下游 IPv4 设备数量上限。
	maxHelperPFDevices = 1024
)

// 阶段一白名单指令集；服务端按名分发，白名单外一律拒绝。
const (
	helperOpPing       = "ping"
	helperOpPFApply    = "pf.apply"
	helperOpPFClear    = "pf.clear"
	helperOpForwardGet = "forward.get"
	helperOpForwardSet = "forward.set"
)

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
	Result  string `json:"result,omitempty"` // forward.get 的转发状态（"on"/"off"）
	Error   string `json:"error,omitempty"`
}

// helperPFApplyParams 是 pf.apply 的结构化参数；root helper 校验全部字段后在内部
// 调用 RenderPFAnchor 生成规则文本，绝不接受客户端拼装的原始 pf.conf 片段。
type helperPFApplyParams struct {
	Devices       []string `json:"devices"`
	RedirPort     int      `json:"redir_port"`
	DNSRedirect   bool     `json:"dns_redirect,omitempty"`
	DNSListenPort int      `json:"dns_listen_port,omitempty"`
}

// buildHelperPFParams 从网关配置提取 pf.apply 所需的最小结构化参数。
func buildHelperPFParams(cfg config.GatewayConfig, dnsListenPort int) helperPFApplyParams {
	return helperPFApplyParams{
		Devices:       deviceIPList(cfg),
		RedirPort:     cfg.EffectiveRedirPort(),
		DNSRedirect:   cfg.DNSRedirect,
		DNSListenPort: dnsListenPort,
	}
}

// validateAndRenderHelperPF 严格校验结构化 pf 参数并在特权边界内渲染 pf anchor 文本。
func validateAndRenderHelperPF(params helperPFApplyParams) (string, error) {
	if params.RedirPort < 1 || params.RedirPort > 65535 {
		return "", fmt.Errorf("redir_port %d 超出 1-65535", params.RedirPort)
	}
	if params.DNSRedirect && (params.DNSListenPort < 1 || params.DNSListenPort > 65535) {
		return "", fmt.Errorf("dns_listen_port %d 超出 1-65535", params.DNSListenPort)
	}
	if len(params.Devices) > maxHelperPFDevices {
		return "", fmt.Errorf("设备数量 %d 超过上限 %d", len(params.Devices), maxHelperPFDevices)
	}
	devices := make([]config.GatewayDevice, 0, len(params.Devices))
	seen := make(map[string]struct{}, len(params.Devices))
	for _, raw := range params.Devices {
		ipText := strings.TrimSpace(raw)
		addr, err := netip.ParseAddr(ipText)
		if err != nil || !addr.Is4() || addr.Zone() != "" {
			return "", fmt.Errorf("设备 IP %q 不是合法 IPv4 地址", raw)
		}
		canonical := addr.String()
		if _, dup := seen[canonical]; dup {
			continue
		}
		seen[canonical] = struct{}{}
		devices = append(devices, config.GatewayDevice{IP: canonical})
	}
	cfg := config.GatewayConfig{
		RedirPort:   params.RedirPort,
		DNSRedirect: params.DNSRedirect,
		Devices:     devices,
	}
	return RenderPFAnchor(cfg, params.DNSListenPort), nil
}

// helperForwardSetParams 是 forward.set 的参数。
type helperForwardSetParams struct {
	Enable bool `json:"enable"`
}

// helperOps 是 helper 服务端执行白名单指令的平台能力接口（darwin 实现见
// helper_server_darwin.go；测试注入 fake）。
type helperOps interface {
	// ApplyPF 整体载入 pf anchor 文本（幂等替换）。
	ApplyPF(anchor string) error
	// ClearPF 清除 anchor 规则并恢复被本模块改动的 IPv4 转发原值。
	ClearPF() error
	// GetForward 返回当前 IPv4 转发状态。
	GetForward() (bool, error)
	// SetForward 开关 IPv4 转发；首次开启时记忆原值供 ClearPF 恢复。
	SetForward(enable bool) error
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
//
// 错误情况：版本不兼容、未知指令、params 非法或执行失败均返回 OK=false。
func dispatchHelperRequest(req helperRequest, ops helperOps) helperResponse {
	resp := helperResponse{Version: helperProtocolVersion}
	fail := func(format string, args ...any) helperResponse {
		resp.Error = fmt.Sprintf(format, args...)
		return resp
	}
	if req.Version != helperProtocolVersion {
		return fail("协议主版本不兼容（helper %d，客户端 %d）：请重新安装 helper 或升级 proxyd", helperProtocolVersion, req.Version)
	}
	switch req.Op {
	case helperOpPing:
		resp.OK = true
		resp.Result = "pong"
	case helperOpPFApply:
		var params helperPFApplyParams
		if err := decodeHelperParams(req.Params, &params); err != nil {
			return fail("pf.apply %v", err)
		}
		anchor, err := validateAndRenderHelperPF(params)
		if err != nil {
			return fail("pf.apply 参数非法: %v", err)
		}
		if err := ops.ApplyPF(anchor); err != nil {
			return fail("pf.apply 执行失败: %v", err)
		}
		resp.OK = true
	case helperOpPFClear:
		if err := ops.ClearPF(); err != nil {
			return fail("pf.clear 执行失败: %v", err)
		}
		resp.OK = true
	case helperOpForwardGet:
		on, err := ops.GetForward()
		if err != nil {
			return fail("forward.get 执行失败: %v", err)
		}
		resp.OK = true
		resp.Result = "off"
		if on {
			resp.Result = "on"
		}
	case helperOpForwardSet:
		var params helperForwardSetParams
		if err := decodeHelperParams(req.Params, &params); err != nil {
			return fail("forward.set %v", err)
		}
		if err := ops.SetForward(params.Enable); err != nil {
			return fail("forward.set 执行失败: %v", err)
		}
		resp.OK = true
	default:
		return fail("未知指令 %q（不在白名单）", req.Op)
	}
	return resp
}

// serveHelperConn 处理一条已授权的 helper 连接：循环读取换行 JSON 请求并回写应答，
// 直到对端关闭或编解码出错。每条成功执行的指令记一次看门狗存活信号。
//
// 参数：
//   - conn: net.Conn，已通过属主校验的 unix socket 连接。
//   - ops: helperOps，平台执行能力。
//   - onActivity: func()，看门狗心跳回调（可为 nil）。
//
// 返回值：无；连接生命周期随对端关闭结束。
//
// 错误情况：解码失败（含 EOF）静默结束连接；单条指令的失败以应答返回，不断开连接。
func serveHelperConn(conn net.Conn, ops helperOps, onActivity func()) {
	reader := bufio.NewReaderSize(conn, 4096)
	encoder := json.NewEncoder(conn)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(helperReadTimeout))
		line, err := readBoundedLine(reader, maxHelperRequestBytes)
		if err != nil {
			return
		}
		var req helperRequest
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}
		resp := dispatchHelperRequest(req, ops)
		if resp.OK && onActivity != nil {
			onActivity()
		}
		_ = conn.SetWriteDeadline(time.Now().Add(helperReadTimeout))
		if err := encoder.Encode(resp); err != nil {
			return
		}
	}
}

// readBoundedLine 读取单行 JSON 请求并强制不超过 maxBytes，避免超大请求耗尽 root helper 内存。
func readBoundedLine(r *bufio.Reader, maxBytes int) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				return buf, nil
			}
			return nil, err
		}
		if len(buf)+len(chunk) > maxBytes {
			return nil, fmt.Errorf("请求超过 %d 字节上限", maxBytes)
		}
		buf = append(buf, chunk...)
		if !isPrefix {
			return buf, nil
		}
	}
}

// HelperStatus 是 helper 安装链路的自查结果（本地读取，不经 socket 鉴权）。
type HelperStatus struct {
	Installed bool   `json:"installed"` // plist 已安装
	Running   bool   `json:"running"`   // launchd 已加载该服务
	Reachable bool   `json:"reachable"` // socket 握手可达（协议版本兼容）
	Detail    string `json:"detail,omitempty"`
}
