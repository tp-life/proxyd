// Package subscribe 负责拉取机场订阅并解析为节点列表。
package subscribe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"proxyd/internal/config"
	"proxyd/internal/node"
)

// userAgent 很多机场按 UA 决定返回格式，伪装成 mihomo 客户端。
const userAgent = "clash.meta/v1.19.30"

// httpClient 拉取订阅用的全局客户端，30s 超时。
var httpClient = &http.Client{Timeout: 30 * time.Second}

// maxBodySize 限制订阅响应体大小，防止异常响应耗尽内存。
const maxBodySize = 32 << 20

// FetchWarning 表示拉取失败但已成功降级使用本地缓存，
// 可通过 errors.As 识别；此时 Fetch 仍会返回缓存解析出的节点。
type FetchWarning struct {
	Sub string // 订阅名
	Err error  // 原始拉取错误
}

func (w *FetchWarning) Error() string {
	return fmt.Sprintf("订阅 %s 拉取失败，已降级使用缓存: %v", w.Sub, w.Err)
}

func (w *FetchWarning) Unwrap() error { return w.Err }

// Fetch 拉取单个订阅并解析为节点列表。
// 拉取成功时把响应体缓存到 <stateDir>/cache/<订阅名>.cache；
// 拉取失败时若缓存存在则降级使用缓存，并返回 *FetchWarning 包装的错误。
func Fetch(ctx context.Context, sub config.Subscription, stateDir string) ([]*node.Node, error) {
	body, err := httpGet(ctx, sub.URL)
	if err == nil {
		// 缓存写失败不影响主流程
		_ = writeCache(stateDir, sub.Name, body)
		return parse(sub, body)
	}

	cached, cerr := os.ReadFile(cachePath(stateDir, sub.Name))
	if cerr != nil {
		return nil, fmt.Errorf("拉取订阅 %s 失败: %w", sub.Name, err)
	}
	nodes, perr := parse(sub, cached)
	if perr != nil {
		return nil, fmt.Errorf("拉取订阅 %s 失败: %w（缓存解析也失败: %v）", sub.Name, err, perr)
	}
	return nodes, &FetchWarning{Sub: sub.Name, Err: err}
}

// parse 按订阅类型分发解析；auto 时先嗅探 Clash YAML，失败再按分享链接解析。
func parse(sub config.Subscription, body []byte) ([]*node.Node, error) {
	switch sub.Type {
	case "clash":
		return ParseClash(body, sub.Name)
	case "share":
		return ParseShare(body, sub.Name)
	default: // auto 或空
		if nodes, err := ParseClash(body, sub.Name); err == nil {
			return nodes, nil
		}
		return ParseShare(body, sub.Name)
	}
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

// cachePath 返回订阅的缓存文件路径，文件名做安全清洗。
func cachePath(stateDir, subName string) string {
	return filepath.Join(stateDir, "cache", sanitizeFileName(subName)+".cache")
}

// sanitizeFileName 把订阅名清洗成安全的文件名：保留字母数字和 -_.，其余替换为 _。
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
func writeCache(stateDir, subName string, body []byte) error {
	p := cachePath(stateDir, subName)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, body, 0o644)
}
