package api

// 代理域：OpenVPN 一体化接入与 .ovpn 安全预览接口。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	proxyopenvpn "proxyd/internal/proxy/openvpn"
)

// openVPNProfileLimit 是单个 .ovpn 请求允许占用的最大 JSON body。
//
// 证书链可能显著大于普通 API 请求，因此允许 2 MiB；仍需硬限制以防管理端误上传
// 大文件导致内存峰值。限制包含 JSON 转义开销，不只是原始文件大小。
const openVPNProfileLimit = 2 << 20

// registerProxyOpenVPNRoutes 注册 profile 预览、一体化创建和聚合删除路由。
//
// 参数说明：mux 是 API Start 创建的私有 ServeMux。
//
// 返回值说明：无。
//
// 错误情况：无；业务错误由各 handler 转换为明确 HTTP 状态。
func (s *Server) registerProxyOpenVPNRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/openvpn/import", s.handlePreviewOpenVPNProfile)
	mux.HandleFunc("POST /api/openvpn/setups", s.handleSetupOpenVPN)
	mux.HandleFunc("DELETE /api/openvpn/setups/{name}", s.handleTerminateOpenVPNSetup)
}

// handlePreviewOpenVPNProfile 解析上传配置并只返回非敏感摘要。
//
// 参数说明：w 写出预览或错误；r.Body 是 `{profile:string}` JSON。
//
// 返回值说明：成功返回 HTTP 200，内容只包含地址、协议、认证需求和兼容性警告。
//
// 错误情况：请求超过 2 MiB、JSON 非法、缺少 inline CA/remote 或包含关键不兼容
// 指令时返回 400/413；证书、私钥和内联密码永远不会回显。
func (s *Server) handlePreviewOpenVPNProfile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, openVPNProfileLimit)
	var request struct {
		Profile string `json:"profile"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "OpenVPN 配置请求无效或文件过大", status)
		return
	}
	preview, err := s.app.PreviewOpenVPNProfile(request.Profile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, preview)
}

// handleSetupOpenVPN 接收单页向导配置，并委托应用层原子创建完整接入聚合。
//
// 参数说明：w 写出创建结果；r.Body 是 openvpn.Setup JSON，可包含原始 .ovpn。
//
// 返回值说明：成功返回 HTTP 201 和实际分配的策略组端口、路由及兼容性警告。
//
// 错误情况：请求过大、profile/askpass/auth-user-pass 无效、名称或端口冲突、VPN
// 握手失败、mihomo 热更新或持久化失败返回 400，应用层会整体回滚。
func (s *Server) handleSetupOpenVPN(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, openVPNProfileLimit)
	var request proxyopenvpn.Setup
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "OpenVPN 配置请求无效或文件过大", status)
		return
	}
	result, err := s.app.SetupOpenVPN(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, result)
}

// handleTerminateOpenVPNSetup 删除 OpenVPN 聚合接入及其已清空策略组和受管路由。
//
// 参数说明：w 写出删除摘要；r.PathValue("name") 是 URL 解码后的节点名称。
//
// 返回值说明：成功返回 HTTP 200；不存在返回 404；冲突或回滚错误返回 409。
//
// 错误情况：空名称返回 400；目标不是 OpenVPN、运行态或持久化失败返回 409。
func (s *Server) handleTerminateOpenVPNSetup(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "OpenVPN 节点名称不能为空", http.StatusBadRequest)
		return
	}
	result, err := s.app.TerminateOpenVPNSetup(r.Context(), name)
	if err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "不存在") {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, result)
}
