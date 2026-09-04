// Package subscribe 负责拉取机场订阅并解析为节点列表。
package subscribe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"proxyd/internal/config"
	"proxyd/internal/proxy/node"
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

// UserInfo 是机场订阅在 subscription-userinfo 响应头里返回的用量信息。
//
// 字段单位遵循通用 Clash 订阅约定：
//   - Upload/Download/Total: 字节数。
//   - Expire: Unix 秒时间戳；0 表示服务端未提供到期时间。
type UserInfo struct {
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
	Total    int64 `json:"total"`
	Expire   int64 `json:"expire"`
}

// IsZero 判断订阅用量信息是否为空。
//
// 返回值为 true 表示服务端没有提供任何有效字段，此时不应覆盖旧缓存。
func (u UserInfo) IsZero() bool {
	return u.Upload == 0 && u.Download == 0 && u.Total == 0 && u.Expire == 0
}

// Used 返回已用流量字节数。
//
// upload/download 都是订阅服务端累计值，UI 展示“已用”时应把两者相加；
// 如果服务端没有提供字段，返回 0。
func (u UserInfo) Used() int64 {
	return u.Upload + u.Download
}

// Fetch 拉取单个订阅并解析为节点列表。
// 拉取成功时把响应体缓存到 <stateDir>/cache/<订阅名>.cache；
// 拉取失败时若缓存存在则降级使用缓存，并返回 *FetchWarning 包装的错误。
func Fetch(ctx context.Context, sub config.Subscription, stateDir string) ([]*node.Node, error) {
	nodes, _, err := FetchWithInfo(ctx, sub, stateDir)
	return nodes, err
}

// FetchWithInfo 拉取单个订阅并同时返回 subscription-userinfo 用量信息。
//
// 参数：
//   - ctx: context.Context，用于控制请求超时/取消。
//   - sub: config.Subscription，订阅名称、URL 与解析类型。
//   - stateDir: string，缓存目录根路径。
//
// 返回值：
//   - []*node.Node: 解析出的节点。
//   - UserInfo: 服务端响应头或本地缓存中的用量信息。
//   - error: 拉取/解析错误；缓存降级成功时为 *FetchWarning。
//
// 错误情况：
//   - HTTP 失败且无缓存时返回拉取错误。
//   - 缓存存在但节点解析失败时返回“拉取失败 + 缓存解析失败”。
func FetchWithInfo(ctx context.Context, sub config.Subscription, stateDir string) ([]*node.Node, UserInfo, error) {
	body, info, err := httpGetWithInfo(ctx, sub.URL)
	if err == nil {
		// 缓存写失败不影响主流程；订阅缓存与用量缓存分开，避免破坏旧 body 缓存格式。
		_ = writeCache(stateDir, sub.Name, body)
		if !info.IsZero() {
			_ = writeUserInfoCache(stateDir, sub.Name, info)
		}
		nodes, err := parse(sub, body)
		return nodes, info, err
	}

	cached, cerr := os.ReadFile(cachePath(stateDir, sub.Name))
	if cerr != nil {
		return nil, UserInfo{}, fmt.Errorf("拉取订阅 %s 失败: %w", sub.Name, err)
	}
	nodes, perr := parse(sub, cached)
	if perr != nil {
		return nil, UserInfo{}, fmt.Errorf("拉取订阅 %s 失败: %w（缓存解析也失败: %v）", sub.Name, err, perr)
	}
	info, _ = ReadCachedUserInfo(stateDir, sub.Name)
	return nodes, info, &FetchWarning{Sub: sub.Name, Err: err}
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

// maxAttempts 拉取订阅的最大尝试次数。部分机场订阅后端不稳定
// （间歇性 502/503/超时），单次失败直接报错体验差，做有限重试。
const maxAttempts = 3

// retryBackoff 第 i 次失败后的等待时间。
var retryBackoff = []time.Duration{1 * time.Second, 3 * time.Second}

// httpGetWithInfo 发起 GET 请求并读取响应体与订阅用量响应头。
//
// retry 只针对网络错误、超时和 5xx：这些通常是上游临时问题；
// 4xx 多数是订阅 URL/token 本身错误，重试只会延迟反馈。
func httpGetWithInfo(ctx context.Context, url string) ([]byte, UserInfo, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, UserInfo{}, lastErr
			case <-time.After(retryBackoff[attempt-1]):
			}
		}
		body, info, retryable, err := httpGetOnce(ctx, url)
		if err == nil {
			return body, info, nil
		}
		lastErr = err
		if !retryable {
			return nil, UserInfo{}, err
		}
	}
	return nil, UserInfo{}, lastErr
}

// httpGetOnce 执行单次请求；retryable 表示该错误值得重试（网络错误、超时或 5xx）。
func httpGetOnce(ctx context.Context, url string) (body []byte, info UserInfo, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, UserInfo{}, false, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, UserInfo{}, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, UserInfo{}, resp.StatusCode >= 500, fmt.Errorf("HTTP %s", resp.Status)
	}
	body, err = readLimitedResponseBody(resp.Body, maxBodySize)
	return body, ParseUserInfo(resp.Header.Get("subscription-userinfo")), err != nil, err
}

// readLimitedResponseBody 在明确的内存上限内完整读取 HTTP 响应体。
//
// 参数说明：
//   - reader: io.Reader，上游订阅响应体。
//   - limit: int64，允许返回的最大字节数，必须大于等于零。
//
// 返回值说明：[]byte 为完整正文；error 表示读取失败或正文超过限制。
//
// 错误情况：读取 limit+1 字节用于识别溢出，因为 io.LimitReader 自身在到达
// 上限时不会报错；超过限制时绝不返回截断订阅，避免部分节点被静默接受并覆盖缓存。
func readLimitedResponseBody(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("订阅响应体超过 %d 字节限制", limit)
	}
	return body, nil
}

// ParseUserInfo 解析 subscription-userinfo 响应头。
//
// 参数：
//   - header: string，形如 `upload=1; download=2; total=3; expire=1700000000`。
//
// 返回值：
//   - UserInfo: 成功解析出的字段；缺失或非法字段保持 0。
//
// 错误情况：
//   - 本函数不返回错误。机场实现经常返回部分字段或大小写不一致字段，
//     宽松解析能避免单个坏字段影响订阅节点拉取。
func ParseUserInfo(header string) UserInfo {
	var info UserInfo
	for _, part := range strings.Split(header, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || n < 0 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "upload":
			info.Upload = n
		case "download":
			info.Download = n
		case "total":
			info.Total = n
		case "expire":
			info.Expire = n
		}
	}
	return info
}

// cachePath 返回订阅的缓存文件路径，文件名做安全清洗。
func cachePath(stateDir, subName string) string {
	return filepath.Join(stateDir, "cache", sanitizeFileName(subName)+".cache")
}

// userInfoCachePath 返回订阅用量缓存文件路径。
//
// 用单独 JSON sidecar 而不是把信息塞进订阅 body 缓存，是为了保持旧缓存仍可直接按
// Clash/share 内容解析，避免引入迁移风险。
func userInfoCachePath(stateDir, subName string) string {
	return filepath.Join(stateDir, "cache", sanitizeFileName(subName)+".userinfo.json")
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

// ReadCachedUserInfo 读取订阅用量缓存。
//
// 参数：
//   - stateDir: string，状态目录根路径。
//   - subName: string，订阅名称。
//
// 返回值：
//   - UserInfo: 缓存中的用量信息。
//   - error: 文件不存在或 JSON 损坏时返回错误。
//
// 错误情况：
//   - 缓存缺失、权限不足或 JSON 非法时返回错误，调用方可安全忽略。
func ReadCachedUserInfo(stateDir, subName string) (UserInfo, error) {
	data, err := os.ReadFile(userInfoCachePath(stateDir, subName))
	if err != nil {
		return UserInfo{}, err
	}
	var info UserInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return UserInfo{}, err
	}
	return info, nil
}

// writeUserInfoCache 写入订阅用量缓存。
//
// 参数：
//   - stateDir: string，状态目录根路径。
//   - subName: string，订阅名称。
//   - info: UserInfo，要持久化的用量信息。
//
// 返回值：
//   - error: 创建目录、JSON 序列化或写文件失败时返回错误。
//
// 错误情况：
//   - 状态目录不可写时返回错误；调用方通常只记录或忽略，不影响代理主体运行。
func writeUserInfoCache(stateDir, subName string, info UserInfo) error {
	p := userInfoCachePath(stateDir, subName)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}
