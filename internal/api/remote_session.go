package api

// 远程连接模块：Web 终端会话中转端点。会话内 `proxyd ssh <远端名>` 持会话凭据
// （而非管理口令）请求本端点，守护进程以自身身份拨号已保存远端后把 HTTP 连接升级为
// 原始字节流。凭据校验在 handler 内完成，因此本路径在 requireManagementAuth 中豁免
// Basic 认证（仅当请求携带会话凭据头时）。

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"proxyd/internal/remote"
)

// sessionPipeDialTimeout 限制中转建连（DERP + 对端握手）的最长等待。
const sessionPipeDialTimeout = 30 * time.Second

// isRemoteSessionPipeRequest 判断请求是否为携带会话凭据的中转请求（豁免管理面 Basic 认证）。
func isRemoteSessionPipeRequest(r *http.Request) bool {
	return r.URL.Path == remote.SessionPipePath && r.Header.Get(remote.SessionHeader) != ""
}

// handleRemoteSessionPipe 校验会话凭据、拨号已保存远端并把连接升级为双向字节流。
//
// 参数说明：
//   - w: http.ResponseWriter，必须支持 Hijack。
//   - r: *http.Request，查询参数 name（已保存远端名）与 port（远端端口），
//     请求头 X-Proxyd-Session 携带凭据、Upgrade: proxyd-pipe 声明协议。
//
// 返回值说明：无；成功时回写 101 后桥接字节流，直至任一端关闭。
//
// 错误情况：凭据无效 401；参数或协议不符 400；名称未保存或拨号失败 502。
func (s *Server) handleRemoteSessionPipe(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), remote.SessionUpgrade) {
		http.Error(w, "需要 Upgrade: "+remote.SessionUpgrade, http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if name == "" || err != nil || port <= 0 || port > 65535 {
		http.Error(w, "参数 name/port 无效", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sessionPipeDialTimeout)
	upstream, err := s.app.DialRemoteSession(ctx, r.Header.Get(remote.SessionHeader), name, port)
	cancel()
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, remote.ErrSessionGrantInvalid) {
			status = http.StatusUnauthorized
		}
		http.Error(w, err.Error(), status)
		return
	}
	defer upstream.Close()

	// 先校验凭据并完成拨号，再检查升级能力：无效凭据不应得到除 401 以外的任何信息。
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "连接不支持升级", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	// http.Server 可能为读请求头设置过 deadline；升级后的长连接由两端关闭决定寿命。
	_ = client.SetDeadline(time.Time{})
	if _, err := io.WriteString(client, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+remote.SessionUpgrade+"\r\n\r\n"); err != nil {
		return
	}
	bridgeSessionPipe(client, buffered.Reader, upstream)
}

// bridgeSessionPipe 双向拷贝客户端与上游；任一方向结束即关闭两端，避免半开连接滞留。
// clientReader 是 Hijack 返回的缓冲读端，可能已含请求头之后的首批数据，必须优先使用。
func bridgeSessionPipe(client net.Conn, clientReader io.Reader, upstream net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, clientReader)
		closeBoth()
		close(done)
	}()
	_, _ = io.Copy(client, upstream)
	closeBoth()
	<-done
}
