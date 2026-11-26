package main

// 远程连接模块 CLI：Web 终端会话内经守护进程中转拨号已保存远端（session-pipe）。
// 守护进程为每个 Web 终端会话注入 PROXYD_SESSION/PROXYD_API；`proxyd ssh <远端名>`
// 本地解析不到时以本命令作为 ProxyCommand，token 与客户端私钥始终留在守护进程内。

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"proxyd/internal/remote"
)

// sessionRelayAvailable 报告当前进程是否处于带中转凭据的 Web 终端会话。
func sessionRelayAvailable() bool {
	return os.Getenv(remote.SessionEnvSecret) != "" && os.Getenv(remote.SessionEnvAPI) != ""
}

// isSafeRelayName 判断远端名是否只含可直接拼入 shell 命令的字符。
func isSafeRelayName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		ok := r == '.' || r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return false
		}
	}
	return true
}

// cmdRemoteSessionPipe 实现 `proxyd remote session-pipe <远端名> <端口>`：
// 连接守护进程中转端点并把 stdio 桥接到远端端口（OpenSSH ProxyCommand 用）。
//
// 参数说明：args 为 []string，依次是已保存的远端名与远端端口。
//
// 返回值说明：error，桥接正常结束时为 nil。
//
// 错误情况：不在 Web 终端会话、参数非法、守护进程拒绝（凭据失效/远端未保存/拨号失败）
// 或网络错误时返回错误。
func cmdRemoteSessionPipe(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("用法: proxyd remote session-pipe <远端名> <端口>")
	}
	port, err := strconv.Atoi(args[1])
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("端口 %q 无效", args[1])
	}
	secret, addr := os.Getenv(remote.SessionEnvSecret), os.Getenv(remote.SessionEnvAPI)
	if secret == "" || addr == "" {
		return fmt.Errorf("当前不在 proxyd Web 终端会话中（缺少 %s/%s）", remote.SessionEnvSecret, remote.SessionEnvAPI)
	}
	conn, reader, err := openSessionPipe(addr, secret, args[0], port)
	if err != nil {
		return err
	}
	defer conn.Close()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		close(done)
	}()
	_, err = io.Copy(os.Stdout, reader)
	return err
}

// openSessionPipe 向守护进程发起中转升级请求，返回已切换为字节流的连接与其缓冲读端。
//
// 参数说明：addr 为管理 API 地址；secret 为会话凭据；name 为远端名；port 为远端端口。
//
// 返回值说明：net.Conn（调用方关闭）与 io.Reader（必须用它读取，可能含已缓冲数据）。
//
// 错误情况：连接失败、响应非 101 时返回包含守护进程错误信息的错误。
func openSessionPipe(addr, secret, name string, port int) (net.Conn, io.Reader, error) {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("连接守护进程 %s 失败: %w", addr, err)
	}
	query := url.Values{"name": {name}, "port": {strconv.Itoa(port)}}
	request := fmt.Sprintf("GET %s?%s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: %s\r\n%s: %s\r\n\r\n",
		remote.SessionPipePath, query.Encode(), addr, remote.SessionUpgrade, remote.SessionHeader, secret)
	// 建连含 DERP 与对端握手，守护进程侧上限 30 秒；这里稍宽并在升级后清除。
	_ = conn.SetDeadline(time.Now().Add(40 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("读取守护进程中转响应失败: %w", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		_ = conn.Close()
		return nil, nil, fmt.Errorf("守护进程中转失败（%s）: %s", response.Status, strings.TrimSpace(string(body)))
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reader, nil
}
