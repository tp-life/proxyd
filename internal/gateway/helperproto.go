package gateway

// helper 换行分隔 JSON IPC 协议与指令分发（docs/adr/0003）。
// 本文件不含平台代码，保证协议与分发的单测在 darwin 之外也能运行；
// 平台执行能力（pfctl/sysctl/socket）经 helperOps 接口注入。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

// helperSocketPath 是 helper 的 unix socket 地址（0600 + 对端属主校验由服务端保证）。
// 不放 state-dir（root helper 不应写用户目录），路径形态与系统域服务约定一致。
const helperSocketPath = "/var/run/com.proxyd.gateway.sock"

// helperProtocolVersion 是协议主版本；握手时主版本不一致即拒绝并提示重装 helper。
const helperProtocolVersion = 1

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

// helperPFApplyParams 是 pf.apply 的参数：完整 pf anchor 文本。
type helperPFApplyParams struct {
	Anchor string `json:"anchor"`
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
		if strings.TrimSpace(params.Anchor) == "" {
			return fail("pf.apply 的 anchor 不能为空")
		}
		if err := ops.ApplyPF(params.Anchor); err != nil {
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
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)
	for {
		var req helperRequest
		if err := decoder.Decode(&req); err != nil {
			return
		}
		resp := dispatchHelperRequest(req, ops)
		if resp.OK && onActivity != nil {
			onActivity()
		}
		if err := encoder.Encode(resp); err != nil {
			return
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
