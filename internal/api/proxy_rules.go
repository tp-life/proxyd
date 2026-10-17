package api

// 代理域：自定义规则与远程规则源（rule-urls）接口。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"proxyd/internal/app"
	"proxyd/internal/config"
)

// registerProxyRuleRoutes 注册自定义规则与远程规则源（rule-urls）路由。
func (s *Server) registerProxyRuleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/rules", s.handleListRules)
	mux.HandleFunc("GET /api/rules/effective", s.handleListEffectiveRules)
	mux.HandleFunc("POST /api/rules", s.handleAddRule)
	mux.HandleFunc("POST /api/rules/reorder", s.handleMoveRule)
	mux.HandleFunc("PUT /api/rules/{index}", s.handleUpdateRule)
	mux.HandleFunc("DELETE /api/rules/{index}", s.handleDelRule)
	mux.HandleFunc("GET /api/rule-urls", s.handleListRuleURLs)
	mux.HandleFunc("POST /api/rule-urls", s.handleAddRuleURL)
	mux.HandleFunc("DELETE /api/rule-urls/{name}", s.handleDelRuleURL)
	mux.HandleFunc("GET /api/rule-urls/{name}/content", s.handleRuleURLContent)
}

func (s *Server) handleListRules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.CustomRules())
}

// handleListEffectiveRules 返回当前生效配置里的规则（含来源标注），支持筛选与分页。
//
// 参数：
//   - w: http.ResponseWriter，写出 app.EffectiveRulesStatus JSON 或 400 错误。
//   - r: *http.Request，查询参数 source（来源键或中文名）、q（规则文本子串）、
//     offset、limit。
//
// 返回值：无；成功返回 HTTP 200 与分页结果。
//
// 错误情况：offset/limit 非整数或为负、source 不是已知来源、规格超出上限时返回 400。
// 代理未启动或已停用时不算错误，返回 available=false 的空快照，前端据此给出引导文案。
func (s *Server) handleListEffectiveRules(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	offset, err := parseNonNegativeInt(query.Get("offset"), "offset")
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	limit, err := parseNonNegativeInt(query.Get("limit"), "limit")
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	status, err := s.app.EffectiveRules(app.EffectiveRulesQuery{
		Source:  query.Get("source"),
		Keyword: query.Get("q"),
		Offset:  offset,
		Limit:   limit,
	})
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, status)
}

// parseNonNegativeInt 解析可选的非负整数查询参数。
//
// 参数：
//   - raw: string，查询参数原值；空字符串视为未提供。
//   - name: string，参数名，仅用于错误信息。
//
// 返回值：
//   - int：解析结果；未提供时返回 0（由用例层决定默认值）。
//   - error：不是整数或为负数时返回。
//
// 错误情况：错误信息不含请求体内容，只回显参数名与要求。
func parseNonNegativeInt(raw, name string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是整数", name)
	}
	if value < 0 {
		return 0, fmt.Errorf("%s 不能为负数", name)
	}
	return value, nil
}

// handleAddRule 追加自定义规则（前置到内置规则之前），持久化 + 热更新。
func (s *Server) handleAddRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rule string `json:"rule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Rule) == "" {
		http.Error(w, "bad request: rule required", http.StatusBadRequest)
		return
	}
	if err := s.app.AddRule(req.Rule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, s.app.CustomRules())
}

func (s *Server) handleDelRule(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		http.Error(w, "bad request: index must be an integer", http.StatusBadRequest)
		return
	}
	if err := s.app.RemoveRule(index); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateRule 原位编辑一条 custom-rules 规则并保留其优先级位置。
//
// 参数：
//   - w: http.ResponseWriter，写出更新后的完整自定义规则列表或错误。
//   - r: *http.Request，路径 index 为零基下标，请求体为 `{ "rule": "..." }`。
//
// 返回值：无；成功返回 HTTP 200 与最新规则数组。
//
// 错误情况：下标/JSON/规则非法、mihomo 自检、持久化或回滚失败时返回 400。
func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		http.Error(w, "bad request: index must be an integer", http.StatusBadRequest)
		return
	}
	var req struct {
		Rule string `json:"rule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Rule) == "" {
		http.Error(w, "bad request: rule required", http.StatusBadRequest)
		return
	}
	if err := s.app.UpdateRule(index, req.Rule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.app.CustomRules())
}

// handleMoveRule 调整 custom-rules 中一条规则的优先级顺序。
//
// 参数：
//   - w: http.ResponseWriter，写出重排后的完整规则列表或错误。
//   - r: *http.Request，请求体为 `{ "from": number, "to": number }`。
//
// 返回值：无；成功返回 HTTP 200 与最新规则数组。
//
// 错误情况：JSON/下标非法、热更新、持久化或回滚失败时返回 400；远程规则源内容
// 不在该接口索引空间内，因此不能被误编辑或重排。
func (s *Server) handleMoveRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From int `json:"from"`
		To   int `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.MoveRule(req.From, req.To); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.app.CustomRules())
}

func (s *Server) handleListRuleURLs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.app.RuleURLs())
}

// handleAddRuleURL 新增规则源：持久化 + 立即拉取该源 + 热更新。
func (s *Server) handleAddRuleURL(w http.ResponseWriter, r *http.Request) {
	var ru config.RuleURL
	if err := json.NewDecoder(r.Body).Decode(&ru); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.app.AddRuleURL(ru); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, s.app.RuleURLs())
}

func (s *Server) handleDelRuleURL(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RemoveRuleURL(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRuleURLContent 返回规则源适合管理界面阅读的文本。
//
// 参数：
//   - w: http.ResponseWriter，写出 UTF-8 文本或 404 错误。
//   - r: *http.Request，路径参数 name 指定要查看的规则源。
//
// 返回值：无；普通规则源返回原文，整体 Base64 gfwlist 返回解码后的 AutoProxy 内容。
//
// 错误情况：
// 规则源不存在，或缓存缺失且现场拉取失败时返回 404。接口不在前端重复实现 Base64
// 识别，确保 Web、CLI 以及未来调用方共享同一个可读内容契约。
func (s *Server) handleRuleURLContent(w http.ResponseWriter, r *http.Request) {
	body, err := s.app.RuleURLContent(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(body)
}
