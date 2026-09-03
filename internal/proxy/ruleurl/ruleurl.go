// Package ruleurl 拉取远程规则源（rule-urls）并解析为 mihomo 规则行。
// 支持两种格式（按内容自动识别）：
//   - mihomo 规则文本：每行 类型,内容,策略（≥3 段），支持 # // 注释与空行；
//   - gfwlist / AutoProxy（base64 编码）：||domain → DOMAIN-SUFFIX,domain,PROXY，
//     @@||domain → DOMAIN-SUFFIX,domain,DIRECT，复杂正则条目跳过。
//
// 拉取成功的内容缓存到 <stateDir>/cache/，失败时降级使用缓存。
package ruleurl

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"proxyd/internal/config"
)

// userAgent 与订阅拉取保持一致（部分站点按 UA 返回内容）。
const userAgent = "clash.meta/v1.19.30"

// maxBodySize 限制规则源响应体大小。
const maxBodySize = 32 << 20

// maxConcurrentRuleFetches 是单轮刷新同时拉取规则源的最大数量。
// 每个规则源响应最多占用 32 MiB，固定 worker 数可限制异常大响应叠加造成的内存峰值；
// 任务仍会全部执行，并按输入索引写回结果，因此不会改变规则源优先级或错误隔离语义。
const maxConcurrentRuleFetches = 4

// MaxImportedRules 是全部规则源合并去重后的导入规则上限，超出截断（调用方打日志）。
const MaxImportedRules = 10000

var httpClient = &http.Client{Timeout: 30 * time.Second}

// Result 是单个规则源的拉取结果。
type Result struct {
	Name  string
	Rules []string
	Err   error // 拉取与缓存都失败时非 nil
	Warn  error // 拉取失败但降级用了缓存
}

// FetchAll 使用固定 worker 池并发拉取所有规则源；单个失败不影响其他。
//
// 参数：
//   - ctx: context.Context，控制整轮规则源请求的取消。
//   - urls: []config.RuleURL，待拉取的规则源，结果严格保持相同索引顺序。
//   - stateDir: string，规则源缓存目录根路径。
//
// 返回值：
//   - []Result：与 urls 等长且一一对应的结果；单源失败只写入自身 Result。
//
// 错误情况：网络、解析及缓存降级错误由 Fetch 写入 Result，不会中断其它任务。空输入
// 返回空切片；上下文取消后排队任务仍会快速进入 Fetch，以得到明确的取消错误或缓存结果。
func FetchAll(ctx context.Context, urls []config.RuleURL, stateDir string) []Result {
	results := make([]Result, len(urls))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(maxConcurrentRuleFetches, len(urls)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = Fetch(ctx, urls[i], stateDir)
			}
		}()
	}
	for i := range urls {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

// Fetch 拉取单个规则源并解析为规则行；失败时降级使用本地缓存。
func Fetch(ctx context.Context, ru config.RuleURL, stateDir string) Result {
	res := Result{Name: ru.Name}
	body, err := httpGet(ctx, ru.URL)
	if err == nil {
		_ = writeCache(stateDir, ru.Name, body) // 缓存写失败不影响主流程
		res.Rules = Parse(body)
		return res
	}
	cached, cerr := os.ReadFile(cachePath(stateDir, ru.Name))
	if cerr != nil {
		res.Err = fmt.Errorf("拉取规则源 %s 失败: %w", ru.Name, err)
		return res
	}
	res.Rules = Parse(cached)
	res.Warn = fmt.Errorf("规则源 %s 拉取失败，已降级使用缓存: %v", ru.Name, err)
	return res
}

// Content 返回适合管理界面阅读的规则源内容。
//
// 参数：
//   - ctx: context.Context，控制无缓存时现场拉取的取消与超时生命周期。
//   - ru: config.RuleURL，包含规则源名称和上游 URL。
//   - stateDir: string，规则源原始响应缓存所在的状态目录。
//
// 返回值：
//   - []byte：普通规则源保持上游原文；整体 Base64 编码且可识别为 AutoProxy/gfwlist
//     时返回解码后的原文，方便 Web 和 CLI 直接展示。
//   - error：缓存不存在且现场拉取失败时返回包含规则源名称的错误。
//
// 错误情况：
// 缓存读取失败会降级为现场拉取；拉取成功后的缓存写入失败不阻断展示。缓存始终保留
// 上游原始字节，避免改变 Parse 的输入契约；解码只发生在返回展示内容的最后一步。
func Content(ctx context.Context, ru config.RuleURL, stateDir string) ([]byte, error) {
	if body, err := os.ReadFile(cachePath(stateDir, ru.Name)); err == nil {
		return normalizeContentForDisplay(body), nil
	}
	body, err := httpGet(ctx, ru.URL)
	if err != nil {
		return nil, fmt.Errorf("尚未拉取过规则源 %s，现场拉取也失败: %w", ru.Name, err)
	}
	_ = writeCache(stateDir, ru.Name, body)
	return normalizeContentForDisplay(body), nil
}

// normalizeContentForDisplay 把可确认的整体 Base64 gfwlist 转换为可读原文。
//
// 参数：
//   - body: []byte，从上游或缓存读取的原始响应字节。
//
// 返回值：
//   - []byte：识别为 AutoProxy/gfwlist 时返回解码结果，否则原样返回 body。
//
// 错误情况：
// Base64 无效、解码内容不像 gfwlist 或普通规则文本时不报错并保持原文。这样既不会
// 把普通规则源误改写，也能让展示识别与 Parse 使用完全相同的格式判定规则。
func normalizeContentForDisplay(body []byte) []byte {
	text := strings.TrimSpace(string(body))
	decoded, ok := tryBase64(text)
	if !ok || !looksLikeGFWList(decoded) {
		return body
	}
	return []byte(decoded)
}

// Parse 按内容自动识别格式并解析为 mihomo 规则行（去重、保序）。
func Parse(body []byte) []string {
	text := strings.TrimSpace(string(body))
	if decoded, ok := tryBase64(text); ok && looksLikeGFWList(decoded) {
		return parseGFWList(decoded)
	}
	return parseRuleText(text)
}

// tryBase64 尝试按 base64 解码（gfwlist 整体编码，可能含换行）。
func tryBase64(s string) (string, bool) {
	compact := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	if compact == "" {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// looksLikeGFWList 判断解码后的内容是否为 AutoProxy/gfwlist 格式。
func looksLikeGFWList(s string) bool {
	return strings.Contains(s, "||") || strings.Contains(s, "@@") ||
		strings.Contains(s, "[AutoProxy")
}

// parseGFWList 解析 AutoProxy/gfwlist 规则：
//   - ||domain   → DOMAIN-SUFFIX,domain,PROXY
//   - @@||domain → DOMAIN-SUFFIX,domain,DIRECT
//   - ! 注释、[AutoProxy] 段头、含 * 或 / 的正则条目、其余前缀（|、@@| 等）跳过。
func parseGFWList(text string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(rule string) {
		if !seen[rule] {
			seen[rule] = true
			out = append(out, rule)
		}
	}
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "!") || strings.HasPrefix(line, "[") {
			continue
		}
		policy := "PROXY"
		rest := line
		if strings.HasPrefix(rest, "@@") {
			policy = "DIRECT"
			rest = rest[2:]
		}
		if !strings.HasPrefix(rest, "||") {
			continue // |https://...、@@|...、裸 URL 等不做域名规则
		}
		domain := rest[2:]
		// 含通配符/路径的复杂条目无法用 DOMAIN-SUFFIX 表达，跳过
		if strings.ContainsAny(domain, "*/") || domain == "" {
			continue
		}
		domain = strings.TrimPrefix(domain, ".")
		if !strings.ContainsAny(domain, "abcdefghijklmnopqrstuvwxyz") {
			continue
		}
		add(fmt.Sprintf("DOMAIN-SUFFIX,%s,%s", domain, policy))
	}
	return out
}

// parseRuleText 解析 mihomo 规则文本：每行 类型,内容,策略[,附加]；
// 支持 # // 注释与空行，不满足 ≥3 段的行跳过。
func parseRuleText(text string) []string {
	seen := map[string]bool{}
	var out []string
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}
		ok := true
		for _, p := range parts {
			if strings.TrimSpace(p) == "" {
				ok = false
				break
			}
		}
		if !ok || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}

// httpGet 发起 GET 请求并读取响应体。
func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
}

// cachePath 返回规则源的缓存文件路径，文件名做安全清洗。
func cachePath(stateDir, name string) string {
	return filepath.Join(stateDir, "cache", "rules-"+sanitizeFileName(name)+".cache")
}

// sanitizeFileName 把名称清洗成安全的文件名：保留字母数字和 -_.，其余替换为 _。
func sanitizeFileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" || s == "." || s == ".." {
		return "unnamed"
	}
	return s
}

// writeCache 把响应体写入缓存文件。
func writeCache(stateDir, name string, body []byte) error {
	p := cachePath(stateDir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, body, 0o644)
}
