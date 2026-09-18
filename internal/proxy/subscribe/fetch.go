// Package subscribe 负责拉取机场订阅并解析为节点列表。
package subscribe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	urlpkg "net/url"
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

// FetchOptions 描述一次订阅拉取的网络降级策略。
//
// FallbackProxyURL 为空时保持原有请求行为；非空时，主链路遇到网络错误、超时或
// 5xx 后，会把同一次 GET 经该 HTTP 代理重试。应用层只在 mihomo 已运行时注入
// 127.0.0.1 主端口，避免首次启动尚无代理数据面时形成无意义的自连接。
type FetchOptions struct {
	FallbackProxyURL string
}

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

// FetchedSubscription 是尚未提交缓存的订阅拉取结果。
//
// body 可能包含节点凭据，因此保持包内私有；应用层只能读取解析后的节点与用量，
// 并在用户确认草稿后调用 CommitCache。fromCache=true 表示本次已经使用旧缓存，
// 无需再次写回相同内容。
type FetchedSubscription struct {
	nodes     []*node.Node
	info      UserInfo
	body      []byte
	fromCache bool
}

// Nodes 返回本次拉取解析出的私有节点集合。
//
// 参数：无。
// 返回值：[]*node.Node，调用方可在草稿流水线中修改；不得直接暴露到 HTTP 响应。
// 错误情况：无；空订阅返回空切片。
func (f *FetchedSubscription) Nodes() []*node.Node {
	if f == nil {
		return nil
	}
	return f.nodes
}

// Info 返回 subscription-userinfo 用量值对象。
//
// 参数：无。
// 返回值：UserInfo；接收者为 nil 时返回零值。
// 错误情况：无。
func (f *FetchedSubscription) Info() UserInfo {
	if f == nil {
		return UserInfo{}
	}
	return f.info
}

// CommitCache 在草稿确认后提交订阅正文与用量缓存。
//
// 参数：
//   - stateDir: string，状态目录根路径。
//   - subName: string，确认时仍有效的订阅名称。
//
// 返回值：error，创建目录或写入正文/用量 sidecar 失败时返回。
//
// 错误情况：缓存降级结果无需重复写入并返回 nil；正文写入成功但用量写入失败时返回
// 用量错误，已写入的正文仍然有效，调用方应记录告警而不能回滚已经生效的运行态。
func (f *FetchedSubscription) CommitCache(stateDir, subName string) error {
	if f == nil || f.fromCache {
		return nil
	}
	if err := writeCache(stateDir, subName, f.body); err != nil {
		return err
	}
	if !f.info.IsZero() {
		return writeUserInfoCache(stateDir, subName, f.info)
	}
	return nil
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
	return FetchWithInfoOptions(ctx, sub, stateDir, FetchOptions{})
}

// LoadCachedWithInfo 只读取并解析已经提交的订阅缓存，不发起任何网络请求。
//
// 参数：
//   - sub: config.Subscription，提供缓存名称与解析类型；URL 不会被访问。
//   - stateDir: string，订阅正文和用量 sidecar 所在的状态目录。
//
// 返回值：
//   - []*node.Node: 缓存正文解析出的节点。
//   - UserInfo: 可选的订阅用量缓存；sidecar 不存在时返回零值。
//   - error: 正文缓存缺失、不可读或不能按当前订阅类型解析时返回。
//
// 错误情况：该函数绝不回退网络，适用于启动、健康检测和配置调和等禁止自动同步
// 的路径；用量缓存损坏不会阻止节点恢复，因为它不影响代理数据面。
func LoadCachedWithInfo(sub config.Subscription, stateDir string) ([]*node.Node, UserInfo, error) {
	body, err := os.ReadFile(cachePath(stateDir, sub.Name))
	if err != nil {
		return nil, UserInfo{}, fmt.Errorf("读取订阅 %s 缓存失败: %w", sub.Name, err)
	}
	nodes, err := parse(sub, body)
	if err != nil {
		return nil, UserInfo{}, fmt.Errorf("解析订阅 %s 缓存失败: %w", sub.Name, err)
	}
	info, _ := ReadCachedUserInfo(stateDir, sub.Name)
	return nodes, info, nil
}

// FetchWithInfoOptions 按给定网络降级策略拉取订阅并返回节点与用量。
//
// 参数说明：
//   - ctx: context.Context，用于控制请求重试、退避和取消。
//   - sub: config.Subscription，订阅名称、URL 与解析类型。
//   - stateDir: string，成功缓存与失败降级所使用的状态目录。
//   - options: FetchOptions，主链路不可达时可选的本机代理路径。
//
// 返回值说明：节点列表、订阅用量和错误；缓存降级成功时错误为 *FetchWarning。
//
// 错误情况：主链路和代理路径均失败且没有有效缓存、响应解析失败，或代理地址非法时
// 返回错误。远端正文只有在成功解析后才写缓存，防止错误页覆盖最后有效版本。
func FetchWithInfoOptions(ctx context.Context, sub config.Subscription, stateDir string, options FetchOptions) ([]*node.Node, UserInfo, error) {
	fetched, err := FetchPreviewWithInfoOptions(ctx, sub, stateDir, options)
	if fetched == nil {
		return nil, UserInfo{}, err
	}
	// 兼容旧的立即刷新入口：只有远端正文成功解析后才提交缓存。缓存写入继续保持
	// 非阻断语义，但不再让格式错误的 HTTP 200 响应覆盖最后一份有效缓存。
	if err == nil {
		_ = fetched.CommitCache(stateDir, sub.Name)
	}
	return fetched.Nodes(), fetched.Info(), err
}

// FetchPreviewWithInfo 拉取并解析订阅，但不修改任何本地缓存。
//
// 参数：
//   - ctx: context.Context，用于控制请求超时/取消。
//   - sub: config.Subscription，订阅名称、URL 与解析类型。
//   - stateDir: string，仅用于网络失败时读取最后一份已确认缓存。
//
// 返回值：
//   - *FetchedSubscription: 私有拉取结果，可在明确确认后提交缓存。
//   - error: 拉取/解析错误；缓存降级成功时为 *FetchWarning 且结果非 nil。
//
// 错误情况：HTTP 失败且无有效缓存、或响应/缓存无法解析时返回错误。远端 HTTP 200
// 但内容非法时不会覆盖或自动回退缓存，防止用户把服务端错误页误认为新订阅。
func FetchPreviewWithInfo(ctx context.Context, sub config.Subscription, stateDir string) (*FetchedSubscription, error) {
	return FetchPreviewWithInfoOptions(ctx, sub, stateDir, FetchOptions{})
}

// FetchPreviewWithInfoOptions 按给定网络策略拉取并解析订阅，但不修改本地缓存。
//
// 参数说明：
//   - ctx: context.Context，用于取消主链路、代理降级和退避等待。
//   - sub: config.Subscription，待预览的订阅值对象。
//   - stateDir: string，仅在全部网络路径失败时读取已确认缓存。
//   - options: FetchOptions，可指定 mihomo 主端口的 HTTP 代理地址。
//
// 返回值说明：未提交的订阅结果和错误；使用缓存时结果非 nil、错误为 *FetchWarning。
//
// 错误情况：所有网络路径失败且缓存不存在/损坏、远端正文无法解析，或代理配置非法时
// 返回错误。HTTP 200 的非法正文不会自动回退缓存，以免掩盖订阅源格式回归。
func FetchPreviewWithInfoOptions(ctx context.Context, sub config.Subscription, stateDir string, options FetchOptions) (*FetchedSubscription, error) {
	body, info, err := httpGetWithInfo(ctx, sub.URL, options)
	if err == nil {
		nodes, parseErr := parse(sub, body)
		if parseErr != nil {
			return nil, parseErr
		}
		return &FetchedSubscription{nodes: nodes, info: info, body: body}, nil
	}

	cached, cerr := os.ReadFile(cachePath(stateDir, sub.Name))
	if cerr != nil {
		return nil, fmt.Errorf("拉取订阅 %s 失败: %w", sub.Name, err)
	}
	nodes, perr := parse(sub, cached)
	if perr != nil {
		return nil, fmt.Errorf("拉取订阅 %s 失败: %w（缓存解析也失败: %v）", sub.Name, err, perr)
	}
	info, _ = ReadCachedUserInfo(stateDir, sub.Name)
	return &FetchedSubscription{nodes: nodes, info: info, body: cached, fromCache: true}, &FetchWarning{Sub: sub.Name, Err: err}
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
func httpGetWithInfo(ctx context.Context, url string, options FetchOptions) ([]byte, UserInfo, error) {
	var lastErr error
	var fallbackClient *http.Client
	defer func() {
		if fallbackClient != nil {
			fallbackClient.CloseIdleConnections()
		}
	}()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, UserInfo{}, lastErr
			case <-time.After(retryBackoff[attempt-1]):
			}
		}
		body, info, retryable, err := httpGetOnce(ctx, httpClient, url)
		if err == nil {
			return body, info, nil
		}
		lastErr = err
		if !retryable {
			return nil, UserInfo{}, err
		}

		// 主链路已经明确失败后才创建代理 Transport，避免成功直连为每次刷新分配
		// 额外连接池。代理也失败时保留两条路径的原因，但请求 URL 不进入错误文本，
		// 防止订阅 token 随日志泄露。
		if strings.TrimSpace(options.FallbackProxyURL) != "" {
			if fallbackClient == nil {
				fallbackClient, err = newFallbackProxyClient(options.FallbackProxyURL)
				if err != nil {
					return nil, UserInfo{}, fmt.Errorf("主链路拉取失败，备用主端口代理配置无效: %w", err)
				}
			}
			proxyBody, proxyInfo, proxyRetryable, proxyErr := httpGetOnce(ctx, fallbackClient, url)
			if proxyErr == nil {
				return proxyBody, proxyInfo, nil
			}
			lastErr = fmt.Errorf("主链路失败: %v；经主端口代理重试失败: %w", err, proxyErr)
			if !proxyRetryable {
				return nil, UserInfo{}, lastErr
			}
		}
	}
	return nil, UserInfo{}, lastErr
}

// httpGetOnce 使用指定客户端执行单次请求。
//
// 参数说明：ctx 控制取消；client 决定直连或代理 Transport；url 是含凭据的订阅地址。
// 返回值说明：正文、用量信息、是否值得重试和错误。
// 错误情况：网络错误、超时和 5xx 标记为可重试；4xx 标记为不可重试；响应过大或
// 读取中断也标记为可重试。底层 *url.Error 会移除原始 URL 后再返回，避免日志泄密。
func httpGetOnce(ctx context.Context, client *http.Client, url string) (body []byte, info UserInfo, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, UserInfo{}, false, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, UserInfo{}, true, redactHTTPRequestError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, UserInfo{}, resp.StatusCode >= 500, fmt.Errorf("HTTP %s", resp.Status)
	}
	body, err = readLimitedResponseBody(resp.Body, maxBodySize)
	return body, ParseUserInfo(resp.Header.Get("subscription-userinfo")), err != nil, err
}

// newFallbackProxyClient 为 mihomo mixed-port 创建独立 HTTP 客户端。
//
// 参数说明：rawProxyURL 是应用层生成的 http://127.0.0.1:<mixed-port> 地址。
// 返回值说明：具有独立连接池和与主客户端相同总超时的 *http.Client。
// 错误情况：地址为空、缺少主机或不是 http/https 协议时返回错误。Transport 显式
// 使用 ProxyURL，因此不受进程 NO_PROXY 对回环地址的影响；调用方必须关闭空闲连接。
func newFallbackProxyClient(rawProxyURL string) (*http.Client, error) {
	proxyURL, err := urlpkg.Parse(strings.TrimSpace(rawProxyURL))
	if err != nil {
		return nil, fmt.Errorf("解析代理地址失败: %w", err)
	}
	if proxyURL.Host == "" || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") {
		return nil, fmt.Errorf("代理地址必须使用 http/https 且包含主机")
	}
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("默认 HTTP Transport 不支持代理克隆")
	}
	transport := baseTransport.Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	return &http.Client{Transport: transport, Timeout: httpClient.Timeout}, nil
}

// redactHTTPRequestError 移除 net/http 错误中可能携带 token 的完整订阅 URL。
//
// 参数说明：err 是 http.Client.Do 返回的错误。
// 返回值说明：若错误链包含 *url.Error，则只保留操作名和底层原因；否则原样返回。
// 错误情况：本函数自身不产生错误，只重建安全的错误链供日志和 API 展示。
func redactHTTPRequestError(err error) error {
	var requestError *urlpkg.Error
	if errors.As(err, &requestError) {
		return fmt.Errorf("%s: %w", requestError.Op, requestError.Err)
	}
	return err
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

// InvalidateCache 删除订阅正文缓存，使 URL/解析类型变更后旧正文不能被本地调和误用。
//
// 参数：
//   - stateDir: string，缓存目录根路径。
//   - subName: string，变更前的订阅名称。
//
// 返回值：error，正文缓存存在但无法删除时返回；缓存原本不存在视为成功。
//
// 错误情况：正文删除失败必须由应用层纳入配置事务回滚。正文删除成功后，用量 sidecar
// 删除失败不影响安全性，因此按尽力清理处理，避免为非数据面信息回滚已提交设置。
func InvalidateCache(stateDir, subName string) error {
	if err := os.Remove(cachePath(stateDir, subName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除订阅 %s 旧缓存失败: %w", subName, err)
	}
	_ = os.Remove(userInfoCachePath(stateDir, subName))
	return nil
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
// 临时文件 + rename 原子替换，避免崩溃留下截断缓存；缓存含节点凭据，
// 权限与配置文件一致使用 0600，不向同机其他用户暴露。
func writeCache(stateDir, subName string, body []byte) error {
	p := cachePath(stateDir, subName)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(p, body)
}

// writeFileAtomic 先写同目录临时文件再 rename，保证目标文件要么完整要么不存在。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
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
	return writeFileAtomic(p, data)
}
