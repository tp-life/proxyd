// Package updatecheck 实现 GitHub Releases 版本查询基础设施适配器。
package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"proxyd/internal/app"
)

// LatestReleaseURL 是 proxyd 官方仓库的 latest release API。
//
// 发布后的单二进制通常不带 .git 目录，因此仓库地址必须是构建内稳定事实，
// 不能在运行时从 git remote 猜测。
const LatestReleaseURL = "https://api.github.com/repos/tp-life/proxyd/releases/latest"

// maxReleaseResponseBytes 限制 GitHub JSON 响应体为 1 MiB，避免异常代理或服务端返回大文件。
const maxReleaseResponseBytes int64 = 1 << 20

// HTTPDoer 是 Checker 所需的最小 HTTP 客户端接口，便于测试替换网络边界。
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Checker 通过 GitHub latest release API 查询最新稳定版本。
type Checker struct {
	endpoint string
	client   HTTPDoer
}

// New 创建使用官方仓库地址和八秒 HTTP 超时的版本检查器。
//
// 参数：无。
//
// 返回值：
//   - *Checker：可注入 app.ReleaseChecker 端口的 GitHub 适配器。
//
// 错误情况：无；网络与响应错误在 Latest 调用时返回。
func New() *Checker {
	return NewWithClient(LatestReleaseURL, &http.Client{Timeout: 8 * time.Second})
}

// NewWithClient 使用指定端点和 HTTP 客户端创建检查器，供测试或受控镜像使用。
//
// 参数：
//   - endpoint: string，GitHub Releases compatible 的 latest API 地址。
//   - client: HTTPDoer，负责实际请求；调用方必须提供非 nil 实现。
//
// 返回值：
//   - *Checker：使用给定依赖的检查器。
//
// 错误情况：本构造函数不发请求；空端点或 nil client 会在 Latest 返回明确错误。
func NewWithClient(endpoint string, client HTTPDoer) *Checker {
	return &Checker{endpoint: endpoint, client: client}
}

// Latest 获取 GitHub 标记为 latest 的稳定 release 元数据。
//
// 参数：
//   - ctx: context.Context，用于取消请求或遵守应用层十秒总超时。
//
// 返回值：
//   - app.LatestRelease：tag、下载页面和发布时间。
//   - error：请求构造、网络、非 200、超限/JSON 解析或字段缺失时返回错误。
//
// 错误情况：响应体最多读取 1 MiB；错误响应只读取少量正文用于日志，不暴露鉴权信息。
func (c *Checker) Latest(ctx context.Context) (app.LatestRelease, error) {
	if strings.TrimSpace(c.endpoint) == "" || c.client == nil {
		return app.LatestRelease{}, fmt.Errorf("版本检查器缺少 endpoint 或 HTTP client")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return app.LatestRelease{}, fmt.Errorf("创建 GitHub Releases 请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "proxyd-update-check")
	resp, err := c.client.Do(req)
	if err != nil {
		return app.LatestRelease{}, fmt.Errorf("请求 GitHub Releases 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return app.LatestRelease{}, fmt.Errorf("GitHub Releases 返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	var payload struct {
		TagName     string    `json:"tag_name"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseResponseBytes))
	if err := decoder.Decode(&payload); err != nil {
		return app.LatestRelease{}, fmt.Errorf("解析 GitHub Releases 响应失败: %w", err)
	}
	if strings.TrimSpace(payload.TagName) == "" || strings.TrimSpace(payload.HTMLURL) == "" {
		return app.LatestRelease{}, fmt.Errorf("GitHub Releases 响应缺少 tag_name 或 html_url")
	}
	return app.LatestRelease{
		Version:     payload.TagName,
		URL:         payload.HTMLURL,
		PublishedAt: payload.PublishedAt,
	}, nil
}
