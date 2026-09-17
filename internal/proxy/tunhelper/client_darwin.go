//go:build darwin

package tunhelper

// macOS 客户端：经 unix socket 向 root 特权 helper 下发白名单指令并接收 utun fd。
// 协议类型与白名单分发见 helperproto.go（跨平台），fd 回传见 fdpass_unix.go。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// helperDial 拨号 helper socket；包级变量便于测试替换。
var helperDial = func() (net.Conn, error) {
	return net.DialTimeout("unix", helperSocketPath, 3*time.Second)
}

// helperHandshake 完成协议握手：先交换 ping，主版本不一致即拒绝。
//
// 参数：
//   - encoder: *json.Encoder，连接写侧编码器。
//   - decoder: *json.Decoder，连接读侧解码器。
//
// 返回值：error，发送/读取失败、被拒绝或版本不兼容时返回。
//
// 错误情况：版本不兼容提示重装 helper。
func helperHandshake(encoder *json.Encoder, decoder *json.Decoder) error {
	handshake := helperRequest{Version: helperProtocolVersion, Op: helperOpPing}
	if err := encoder.Encode(handshake); err != nil {
		return fmt.Errorf("helper 握手发送失败: %w", err)
	}
	var ack helperResponse
	if err := decoder.Decode(&ack); err != nil {
		return fmt.Errorf("helper 握手读取失败: %w", err)
	}
	if !ack.OK {
		return fmt.Errorf("helper 握手被拒绝: %s", ack.Error)
	}
	if ack.Version != helperProtocolVersion {
		return fmt.Errorf(
			"helper 协议版本不兼容（本端 %d，对端 %d）：请重新执行 proxyd tun helper install 升级 helper",
			helperProtocolVersion, ack.Version,
		)
	}
	return nil
}

// helperDialChecked 拨号并把拨号失败统一包装为 ErrHelperUnavailable（含安装指引）。
func helperDialChecked() (net.Conn, error) {
	conn, err := helperDial()
	if err != nil {
		return nil, fmt.Errorf(
			"无法连接 tun 特权 helper（%s）: %v；helper 未安装或未运行，请执行 proxyd tun helper install 安装: %w",
			helperSocketPath, err, ErrHelperUnavailable,
		)
	}
	return conn, nil
}

// helperCall 完成一次「握手 + 单指令」往返；每次调用独立建连（helper 为短连接模型）。
//
// 参数：
//   - op: string，白名单指令名（ping/tun.create）。
//   - params: any，指令参数结构（tun.create 为 CreateTUNParams）；nil 表示无参数。
//
// 返回值：
//   - helperResponse：helper 应答。
//   - error：拨号失败（包 ErrHelperUnavailable）、握手版本不兼容或指令被拒绝时返回。
//
// 错误情况：tun.create 之外的指令无 fd 回传；需要 fd 的调用方用 RequestTUN。
func helperCall(op string, params any) (helperResponse, error) {
	conn, err := helperDialChecked()
	if err != nil {
		return helperResponse{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(bufio.NewReader(conn))

	if err := helperHandshake(encoder, decoder); err != nil {
		return helperResponse{}, err
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

// Precheck 检测 helper 是否可连（ping 握手验版本）。
//
// 参数：无。
//
// 返回值：error，helper 已安装且协议兼容时返回 nil。
//
// 错误情况：拨号/握手失败返回含安装指引的中文错误（拨号失败包 ErrHelperUnavailable）。
func Precheck() error {
	_, err := helperCall(helperOpPing, nil)
	return err
}

// RequestTUN 请求 helper 创建 utun 设备并接收其文件描述符：
// 握手 → tun.create → 确认字节 → SCM_RIGHTS 收 fd → dup 高位。
//
// 参数：
//   - params: CreateTUNParams，创建参数（本地先校验一遍，避免白跑特权链路）。
//
// 返回值：
//   - fd int：dup 到 >=10 高位的 utun fd，调用方持有（注入 mihomo tun.file-descriptor）。
//   - ifName string：内核分配的接口名（如 "utun5"）。
//   - err error：拨号失败包 ErrHelperUnavailable；参数非法/执行失败/收 fd 失败返回中文错误。
//
// 错误情况：收 fd 失败时连接直接关闭，helper 侧副本由服务端自行关闭，utun 随之内核回收。
func RequestTUN(params CreateTUNParams) (int, string, error) {
	if err := params.Validate(); err != nil {
		return -1, "", err
	}
	conn, err := helperDialChecked()
	if err != nil {
		return -1, "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(bufio.NewReader(conn))

	if err := helperHandshake(encoder, decoder); err != nil {
		return -1, "", err
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return -1, "", fmt.Errorf("tun.create 参数编码失败: %w", err)
	}
	if err := encoder.Encode(helperRequest{Version: helperProtocolVersion, Op: helperOpTunCreate, Params: raw}); err != nil {
		return -1, "", fmt.Errorf("tun.create 发送失败: %w", err)
	}
	var resp helperResponse
	if err := decoder.Decode(&resp); err != nil {
		return -1, "", fmt.Errorf("tun.create 读取应答失败: %w", err)
	}
	if !resp.OK {
		return -1, "", fmt.Errorf("tun.create 执行失败: %s", resp.Error)
	}
	// 通知服务端可以发 fd 了：防止本地解码器缓冲超读吞掉带 SCM_RIGHTS 的数据字节。
	if _, err := conn.Write([]byte{0}); err != nil {
		return -1, "", fmt.Errorf("tun.create 确认字节发送失败: %w", err)
	}
	fd, err := recvHelperFD(conn)
	if err != nil {
		return -1, "", err
	}
	return fd, resp.Result, nil
}
