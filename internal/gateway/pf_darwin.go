//go:build darwin

package gateway

// macOS 执行层：经 unix socket 向 root 特权 helper 下发白名单指令
//（docs/adr/0003）。协议类型与白名单分发见 helperproto.go（跨平台），
// 本文件只含客户端拨号/握手与 Runner 实现。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"proxyd/internal/config"
)

// helperDial 拨号 helper socket；包级变量便于测试替换。
var helperDial = func() (net.Conn, error) {
	return net.DialTimeout("unix", helperSocketPath, 3*time.Second)
}

// helperCall 完成一次「握手 + 单指令」往返；每次调用独立建连（helper 为短连接模型）。
//
// 参数：
//   - op: string，白名单指令名（ping/pf.apply/pf.clear/forward.get/forward.set）。
//   - params: any，指令参数结构（pf.apply 为 helperPFApplyParams 等）；nil 表示无参数。
//
// 返回值：
//   - helperResponse：helper 应答。
//   - error：拨号失败（helper 未安装，包 ErrHelperUnavailable）、握手版本不兼容
//     或指令被拒绝时返回。
//
// 错误情况：拨号失败返回带安装指引的中文错误；版本不兼容提示重装 helper。
func helperCall(op string, params any) (helperResponse, error) {
	conn, err := helperDial()
	if err != nil {
		return helperResponse{}, fmt.Errorf(
			"无法连接 gateway 特权 helper（%s）: %v；helper 未安装或未运行，请执行 sudo proxyd gateway helper install 安装（安装链路见 docs/adr/0003）: %w",
			helperSocketPath, err, ErrHelperUnavailable,
		)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(bufio.NewReader(conn))

	// 握手：先交换协议版本，主版本不一致即拒绝。
	handshake := helperRequest{Version: helperProtocolVersion, Op: helperOpPing}
	if err := encoder.Encode(handshake); err != nil {
		return helperResponse{}, fmt.Errorf("helper 握手发送失败: %w", err)
	}
	var ack helperResponse
	if err := decoder.Decode(&ack); err != nil {
		return helperResponse{}, fmt.Errorf("helper 握手读取失败: %w", err)
	}
	if !ack.OK {
		return helperResponse{}, fmt.Errorf("helper 握手被拒绝: %s", ack.Error)
	}
	if ack.Version != helperProtocolVersion {
		return helperResponse{}, fmt.Errorf(
			"helper 协议版本不兼容（本端 %d，对端 %d）：请重新执行 sudo proxyd gateway helper install 升级 helper",
			helperProtocolVersion, ack.Version,
		)
	}

	req := helperRequest{Version: helperProtocolVersion, Op: op}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return helperResponse{}, fmt.Errorf("helper 指令 %q 参数编码失败: %w", op, err)
		}
		req.Params = raw
	}
	if err := encoder.Encode(req); err != nil {
		return helperResponse{}, fmt.Errorf("helper 指令 %q 发送失败: %w", op, err)
	}
	var resp helperResponse
	if err := decoder.Decode(&resp); err != nil {
		return helperResponse{}, fmt.Errorf("helper 指令 %q 读取应答失败: %w", op, err)
	}
	if !resp.OK {
		return helperResponse{}, fmt.Errorf("helper 指令 %q 执行失败: %s", op, resp.Error)
	}
	return resp, nil
}

// helperRunner 是 macOS 的 Runner 实现：全部特权操作经 helper 白名单指令完成。
type helperRunner struct{}

// newRunner 选定 macOS 执行层。
func newRunner() Runner { return helperRunner{} }

// isUnsupported 报告当前平台不受支持；macOS 恒为 false。
func isUnsupported() bool { return false }

// renderRules 生成 macOS 的 pf anchor 文本。
func renderRules(cfg config.GatewayConfig, dnsListenPort int) string {
	return RenderPFAnchor(cfg, dnsListenPort)
}

// Precheck 检测 macOS 网关前置条件：特权 helper 是否可连（ping 握手）。
//
// 参数：无。
//
// 返回值：
//   - PrecheckResult：Ready 表示 helper 已安装且协议兼容；未就绪时 Detail 含安装指引。
//
// 错误情况：不返回 error；拨号/握手失败折叠进 Detail 文本。
func Precheck() PrecheckResult {
	result := PrecheckResult{Platform: "darwin", Supported: true}
	if _, err := helperCall(helperOpPing, nil); err != nil {
		result.Detail = err.Error()
		return result
	}
	result.Ready = true
	result.Detail = "helper 已连接（协议版本兼容）"
	return result
}

// Apply 经 helper 应用 pf anchor 文本。
func (helperRunner) Apply(text string) error {
	_, err := helperCall(helperOpPFApply, helperPFApplyParams{Anchor: text})
	return err
}

// Clear 经 helper 清除 pf anchor；helper 看门狗在主进程失联时也会自动清除。
func (helperRunner) Clear() error {
	_, err := helperCall(helperOpPFClear, nil)
	return err
}

// Forwarding 经 helper 开关 macOS 的 IPv4 转发（sysctl net.inet.ip.forwarding）。
func (helperRunner) Forwarding(enable bool) error {
	_, err := helperCall(helperOpForwardSet, helperForwardSetParams{Enable: enable})
	return err
}

// Ping 实现 heartbeatPinger：作为看门狗存活信号（任何成功指令均算心跳）。
func (helperRunner) Ping() error {
	_, err := helperCall(helperOpPing, nil)
	return err
}

// Status 返回 helper 上报的转发状态；连不上时返回未安装提示。
func (helperRunner) Status() string {
	resp, err := helperCall(helperOpForwardGet, nil)
	if err != nil {
		return err.Error()
	}
	return "forwarding=" + resp.Result
}

// DiagnoseGateway 执行 macOS 专属检查：helper 握手/版本与 IPv4 转发实际值。
// Detail 只用固定文案，不回传原始拨号错误。
func (helperRunner) DiagnoseGateway(_ context.Context) []DiagnosticCheck {
	steps := []DiagnosticCheck{}
	start := time.Now()
	_, err := helperCall(helperOpPing, nil)
	switch {
	case err == nil:
		steps = append(steps, DiagnosticCheck{ID: "gateway_helper", Status: "passed", Detail: "helper 握手正常，协议版本兼容", DurationMS: time.Since(start).Milliseconds()})
	case strings.Contains(err.Error(), "版本不兼容"):
		steps = append(steps, DiagnosticCheck{ID: "gateway_helper", Status: "failed", Detail: "helper 协议版本不兼容，请重新执行 proxyd gateway helper install", DurationMS: time.Since(start).Milliseconds()})
	default:
		steps = append(steps, DiagnosticCheck{ID: "gateway_helper", Status: "failed", Detail: "helper 未安装或未运行，请执行 proxyd gateway helper install", DurationMS: time.Since(start).Milliseconds()})
	}
	resp, err := helperCall(helperOpForwardGet, nil)
	if err != nil {
		steps = append(steps, DiagnosticCheck{ID: "gateway_forward", Status: "failed", Detail: "无法读取 IPv4 转发状态（helper 不可达）"})
	} else {
		steps = append(steps, DiagnosticCheck{ID: "gateway_forward", Status: "passed", Detail: "IPv4 转发当前为 " + resp.Result})
	}
	return steps
}
