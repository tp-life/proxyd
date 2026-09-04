package main

// 本地管理子命令：全部作为运行中实例的 HTTP API 客户端实现
// （读取配置拿 api-listen 地址；实例未运行时给出明确提示）。

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"proxyd/internal/api"
	"proxyd/internal/config"
)

// parseCFlag 解析通用的 -c 配置文件 flag（flag 需放在位置参数之前）。
// Go flag 解析遇位置参数即停止，写在后面的 -c 会被静默忽略，导致误操作默认配置
// 对应的实例（可能不是用户预期的那台）；检测到后位置 -c 时直接报错并提示正确写法。
func parseCFlag(name string, args []string) (string, []string, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfgFile := fs.String("c", config.DefaultPath(), "配置文件路径")
	_ = fs.Parse(args)
	rest := fs.Args()
	for i, a := range rest {
		if a == "-c" || a == "--c" || strings.HasPrefix(a, "-c=") || strings.HasPrefix(a, "--c=") {
			return "", nil, fmt.Errorf("-c 需放在子命令参数之前，例如: proxyd %s -c <配置> %s", name, strings.Join(rest[:i], " "))
		}
	}
	return *cfgFile, rest, nil
}

// apiClient 是 proxyd 自有 API 的简易客户端。
type apiClient struct {
	base   string
	secret string
}

func newAPIClient(cfgFile string) (*apiClient, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s 失败: %w", cfgFile, err)
	}
	return &apiClient{base: "http://" + cfg.APIListen, secret: cfg.APISecret}, nil
}

// applyManagementAuth 为绕过 send 的长连接请求附加统一管理面认证。
//
// 参数说明：
//   - request: *http.Request，即将发往 proxyd 管理 API 的请求。
//
// 返回值说明：无；直接修改请求头。api-secret 为空时保持请求不变。
//
// 错误情况：无；SetBasicAuth 会按 HTTP 标准编码用户名和口令。
func (c *apiClient) applyManagementAuth(request *http.Request) {
	if c.secret != "" {
		request.SetBasicAuth("proxyd", c.secret)
	}
}

// do 发起一次 API 调用；连接失败提示先启动实例，HTTP 错误原样透传服务端报错。
func (c *apiClient) do(method, path string, body, out any) error {
	return c.doTimeout(method, path, body, out, 60*time.Second)
}

// doTimeout 与 do 相同，但允许调用方指定超时；
// 订阅刷新/测速、导入等同步接口最长可能执行 3 分钟，需要放宽默认 60s。
func (c *apiClient) doTimeout(method, path string, body, out any, timeout time.Duration) error {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(data)
	}
	resp, err := c.send(method, path, rdr, nil, timeout)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkAPIError(resp); err != nil {
		return err
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// raw 发起一次原始字节请求（配置导出/导入等非 JSON 接口），返回响应正文。
func (c *apiClient) raw(method, path string, body []byte, headers map[string]string, timeout time.Duration) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	resp, err := c.send(method, path, rdr, headers, timeout)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkAPIError(resp); err != nil {
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

// send 构造并执行一次请求；连接失败统一给出"先启动实例"的提示。
func (c *apiClient) send(method, path string, body io.Reader, headers map[string]string, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.applyManagementAuth(req)
	hc := &http.Client{Timeout: timeout}
	if timeout <= 0 {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接 proxyd（%s）：实例未在运行？请先 proxyd start（或 proxyd serve）", c.base)
	}
	return resp, nil
}

// checkAPIError 把 >=400 的响应转成原样透传的服务端错误文本。
func checkAPIError(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	b, _ := io.ReadAll(resp.Body)
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("%s", msg)
}

func (c *apiClient) overview() (*api.Overview, error) {
	var ov api.Overview
	if err := c.do(http.MethodGet, "/api/overview", nil, &ov); err != nil {
		return nil, err
	}
	return &ov, nil
}

// getText 发起一次 GET 并返回原始文本（不做 JSON 解析）；错误处理与 do 一致。
func (c *apiClient) getText(path string) (string, error) {
	b, err := c.raw(http.MethodGet, path, nil, nil, 60*time.Second)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// formatBytes 把字节数格式化为适合命令行阅读的二进制单位。
//
// 参数：
//   - n: int64，字节数。
//
// 返回值：
//   - string，格式化后的容量文本。
//
// 错误情况：
//   - 负数按 0B 展示，避免上游异常值污染输出。
func formatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}

// parseOnOff 解析 on/off 开关参数。
func parseOnOff(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	return false, fmt.Errorf("无效参数 %q（on|off）", s)
}
