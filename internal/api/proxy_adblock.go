package api

// 代理域：广告拦截（adblock）开关与规则集地址接口。

import (
	"encoding/json"
	"net/http"
)

// registerProxyAdBlockRoutes 注册广告拦截路由。
func (s *Server) registerProxyAdBlockRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/adblock", s.handleGetAdBlock)
	mux.HandleFunc("PUT /api/adblock", s.handleSetAdBlock)
}

// adBlockState 是 GET/PUT /api/adblock 的响应体。
type adBlockState struct {
	Enable    bool   `json:"enable"`
	RuleURL   string `json:"rule_url"`
	Effective bool   `json:"effective"` // 规则是否实际参与匹配：主端口恒为规则模式，开启即生效
}

// currentAdBlockState 汇总广告拦截配置与运行态生效情况。
func (s *Server) currentAdBlockState() adBlockState {
	cfg := s.app.AdBlock()
	return adBlockState{
		Enable:    cfg.Enable,
		RuleURL:   cfg.RuleURL,
		Effective: s.app.AdBlockEffective(),
	}
}

// handleGetAdBlock 返回广告拦截开关、规则集地址与当前是否实际生效。
func (s *Server) handleGetAdBlock(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.currentAdBlockState())
}

// handleSetAdBlock 开关广告拦截并可选更新规则集地址，返回提交后的最新状态。
//
// 参数：
//   - w: http.ResponseWriter，写出 JSON 响应或事务失败信息。
//   - r: *http.Request，请求体为 `{ "enable": boolean, "rule_url": "可选，空=保持原值" }`。
//
// 返回值：无；成功返回 HTTP 200 与最新 adBlockState。
//
// 错误情况：JSON 无效返回 400；校验、热更新、持久化或回滚失败也返回 400，
// 正文保留应用层合并错误，便于 UI 明确提示用户检查运行态。
func (s *Server) handleSetAdBlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enable  bool   `json:"enable"`
		RuleURL string `json:"rule_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.SetAdBlock(req.Enable, req.RuleURL); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.currentAdBlockState())
}
