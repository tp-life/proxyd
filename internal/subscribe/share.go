package subscribe

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"proxyd/internal/node"
)

// ParseShare 解析 base64 编码（也兼容明文）的多行分享链接。
// 解析失败或不支持的协议行会被跳过；仅当一行都解析不出来时才返回错误。
// 生成的 map 键名与 mihomo v1.19.30 adapter/outbound 中各 XxxOption 的 proxy tag 一致。
func ParseShare(body []byte, subName string) ([]*node.Node, error) {
	text := decodeShareBody(body)
	var nodes []*node.Node
	skipped := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m, name, err := parseShareLink(line)
		if err != nil {
			skipped++
			continue
		}
		nodes = append(nodes, newNode(m, name, subName))
	}
	if len(nodes) == 0 && skipped > 0 {
		return nil, fmt.Errorf("分享链接全部解析失败（跳过 %d 行）", skipped)
	}
	return nodes, nil
}

// decodeShareBody 尝试整体 base64 解码订阅内容；解不出来则按明文处理。
func decodeShareBody(body []byte) string {
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, string(body))
	if dec, err := b64decode(compact); err == nil && strings.Contains(string(dec), "://") {
		return string(dec)
	}
	return string(body)
}

// parseShareLink 按协议前缀分发解析单行分享链接，返回 mihomo outbound map 和节点名。
func parseShareLink(line string) (map[string]any, string, error) {
	switch {
	case strings.HasPrefix(line, "ss://"):
		return parseSS(line)
	case strings.HasPrefix(line, "ssr://"):
		return parseSSR(line)
	case strings.HasPrefix(line, "vmess://"):
		return parseVMess(line)
	case strings.HasPrefix(line, "vless://"):
		return parseVLESS(line)
	case strings.HasPrefix(line, "trojan://"):
		return parseTrojan(line)
	case strings.HasPrefix(line, "hysteria2://"), strings.HasPrefix(line, "hy2://"):
		return parseHysteria2(line)
	case strings.HasPrefix(line, "tuic://"):
		return parseTUIC(line)
	default:
		return nil, "", errors.New("不支持的协议")
	}
}

// b64decode 依次尝试 RawURLEncoding / URLEncoding / RawStdEncoding / StdEncoding，
// 并在必要时补齐 padding。
func b64decode(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, withPad := range []string{s, padBase64(s)} {
		for _, enc := range []*base64.Encoding{
			base64.RawURLEncoding, base64.URLEncoding,
			base64.RawStdEncoding, base64.StdEncoding,
		} {
			if b, err := enc.DecodeString(withPad); err == nil {
				return b, nil
			}
		}
	}
	return nil, errors.New("base64 解码失败")
}

// padBase64 补齐 base64 padding。
func padBase64(s string) string {
	if r := len(s) % 4; r != 0 {
		return s + strings.Repeat("=", 4-r)
	}
	return s
}

// b64decodeString 是 b64decode 的字符串便捷封装，失败时返回空串。
func b64decodeString(s string) string {
	b, err := b64decode(s)
	if err != nil {
		return ""
	}
	return string(b)
}

// splitHostPort 拆分 host:port 并校验端口范围。
func splitHostPort(hostport string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", 0, fmt.Errorf("无效的 host:port %q: %w", hostport, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("无效的端口 %q", portStr)
	}
	return host, port, nil
}

// fragmentName 取分享链接 #fragment 作为节点名（URL-decoded）。
func fragmentName(u *url.URL) string {
	name, err := url.PathUnescape(u.Fragment)
	if err != nil {
		return u.Fragment
	}
	return name
}

// fallbackName 节点名为空时用 host:port 兜底。
func fallbackName(name, host string, port int) string {
	if name != "" {
		return name
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// queryBool 解析 query 中的布尔值（"1"/"true" 等）。
func queryBool(q url.Values, keys ...string) bool {
	for _, k := range keys {
		switch strings.ToLower(q.Get(k)) {
		case "1", "true", "yes":
			return true
		}
	}
	return false
}

// queryALPN 解析 alpn 参数（逗号分隔）。
func queryALPN(q url.Values) []string {
	v := q.Get("alpn")
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// setTransportOpts 按 network 类型填充 ws-opts / grpc-opts（vless/trojan 链接通用）。
func setTransportOpts(m map[string]any, network string, q url.Values) {
	switch network {
	case "ws":
		ws := map[string]any{}
		if p := q.Get("path"); p != "" {
			ws["path"] = p
		}
		if h := q.Get("host"); h != "" {
			ws["headers"] = map[string]string{"Host": h}
		}
		if len(ws) > 0 {
			m["ws-opts"] = ws
		}
	case "grpc":
		if sn := q.Get("serviceName"); sn != "" {
			m["grpc-opts"] = map[string]any{"grpc-service-name": sn}
		}
	}
}

// parseSS 解析 ss:// 链接，兼容三种格式：
//   - SIP002: ss://base64(method:password)@host:port?plugin=...#name
//   - SIP002 明文 userinfo: ss://method:password@host:port#name
//   - 旧格式: ss://base64(method:password@host:port)#name
func parseSS(link string) (map[string]any, string, error) {
	rest := strings.TrimPrefix(link, "ss://")

	// 先剥离 fragment 和 query（plugin 参数）
	name := ""
	if i := strings.Index(rest, "#"); i >= 0 {
		if v, err := url.PathUnescape(rest[i+1:]); err == nil {
			name = v
		} else {
			name = rest[i+1:]
		}
		rest = rest[:i]
	}
	rawQuery := ""
	if i := strings.Index(rest, "?"); i >= 0 {
		rawQuery = rest[i+1:]
		rest = rest[:i]
	}

	var userinfo, hostport string
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		userinfo, hostport = rest[:i], rest[i+1:]
	} else {
		// 旧格式：整体 base64 后再拆
		dec, err := b64decode(rest)
		if err != nil {
			return nil, "", fmt.Errorf("ss: %w", err)
		}
		s := string(dec)
		i := strings.LastIndex(s, "@")
		if i < 0 {
			return nil, "", errors.New("ss: 缺少 @")
		}
		userinfo, hostport = s[:i], s[i+1:]
	}

	// userinfo 可能是 base64(method:password)，也可能是明文（可能含 percent 编码）
	if dec, err := b64decode(userinfo); err == nil && strings.Contains(string(dec), ":") {
		userinfo = string(dec)
	} else if v, err := url.PathUnescape(userinfo); err == nil {
		userinfo = v
	}
	ci := strings.Index(userinfo, ":")
	if ci <= 0 {
		return nil, "", errors.New("ss: 缺少 method:password")
	}
	method, password := userinfo[:ci], userinfo[ci+1:]

	host, port, err := splitHostPort(hostport)
	if err != nil {
		return nil, "", err
	}

	m := map[string]any{
		"type":     "ss",
		"server":   host,
		"port":     port,
		"cipher":   method,
		"password": password,
	}
	parseSSPlugin(m, rawQuery)
	return m, fallbackName(name, host, port), nil
}

// parseSSPlugin 尽力解析 plugin 参数（obfs-local / v2ray-plugin），解析不了就忽略。
func parseSSPlugin(m map[string]any, rawQuery string) {
	if rawQuery == "" {
		return
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return
	}
	p := q.Get("plugin")
	if p == "" {
		return
	}
	parts := strings.Split(p, ";")
	pluginName := parts[0]
	opts := map[string]string{}
	flags := map[string]bool{}
	for _, kv := range parts[1:] {
		if i := strings.Index(kv, "="); i > 0 {
			opts[kv[:i]] = kv[i+1:]
		} else if kv != "" {
			flags[kv] = true
		}
	}
	switch pluginName {
	case "obfs-local", "simple-obfs", "obfs":
		po := map[string]any{}
		if mode := opts["obfs"]; mode != "" {
			po["mode"] = mode
		}
		if host := opts["obfs-host"]; host != "" {
			po["host"] = host
		}
		if len(po) == 0 {
			return
		}
		m["plugin"] = "obfs"
		m["plugin-opts"] = po
	case "v2ray-plugin":
		po := map[string]any{"mode": "websocket"}
		if host := opts["host"]; host != "" {
			po["host"] = host
		}
		if path := opts["path"]; path != "" {
			po["path"] = path
		}
		if flags["tls"] {
			po["tls"] = true
		}
		if flags["mux"] {
			po["mux"] = true
		}
		m["plugin"] = "v2ray-plugin"
		m["plugin-opts"] = po
	default:
		// 未知 plugin，忽略
	}
}

// parseSSR 解析 ssr:// 链接：
// ssr://base64(host:port:protocol:method:obfs:base64pass/?obfsparam=&protoparam=&remarks=&group=)
func parseSSR(link string) (map[string]any, string, error) {
	rest := strings.TrimPrefix(link, "ssr://")
	dec, err := b64decode(rest)
	if err != nil {
		return nil, "", fmt.Errorf("ssr: %w", err)
	}
	s := string(dec)

	paramStr := ""
	if i := strings.Index(s, "/?"); i >= 0 {
		paramStr = s[i+2:]
		s = s[:i]
	}
	parts := strings.SplitN(s, ":", 6)
	if len(parts) != 6 {
		return nil, "", fmt.Errorf("ssr: 字段数不足（%d/6）", len(parts))
	}
	host, protocol, method, obfs := parts[0], parts[2], parts[3], parts[4]
	port, err := strconv.Atoi(parts[1])
	if err != nil || port <= 0 || port > 65535 {
		return nil, "", fmt.Errorf("ssr: 无效的端口 %q", parts[1])
	}
	password := b64decodeString(parts[5])
	if password == "" {
		return nil, "", errors.New("ssr: 密码解码失败")
	}

	// query 参数值各自也是 base64url 编码
	params := map[string]string{}
	for _, kv := range strings.Split(paramStr, "&") {
		if i := strings.Index(kv, "="); i > 0 {
			params[kv[:i]] = b64decodeString(kv[i+1:])
		}
	}

	m := map[string]any{
		"type":     "ssr",
		"server":   host,
		"port":     port,
		"cipher":   method,
		"password": password,
		"protocol": protocol,
		"obfs":     obfs,
	}
	if v := params["obfsparam"]; v != "" {
		m["obfs-param"] = v
	}
	if v := params["protoparam"]; v != "" {
		m["protocol-param"] = v
	}
	name := params["remarks"]
	return m, fallbackName(name, host, port), nil
}

// vmessJSON 是 vmess:// 链接 base64 解码后的 JSON 结构（字段对常见客户端容错）。
type vmessJSON struct {
	PS   string `json:"ps"`
	Add  string `json:"add"`
	Port any    `json:"port"`
	ID   string `json:"id"`
	Aid  any    `json:"aid"`
	Scy  string `json:"scy"`
	Net  string `json:"net"`
	Type string `json:"type"`
	Host string `json:"host"`
	Path string `json:"path"`
	TLS  string `json:"tls"`
	SNI  string `json:"sni"`
	Alpn string `json:"alpn"`
}

// parseVMess 解析 vmess://base64(JSON) 链接。
func parseVMess(link string) (map[string]any, string, error) {
	rest := strings.TrimPrefix(link, "vmess://")
	dec, err := b64decode(rest)
	if err != nil {
		return nil, "", fmt.Errorf("vmess: %w", err)
	}
	var v vmessJSON
	if err := json.Unmarshal(dec, &v); err != nil {
		return nil, "", fmt.Errorf("vmess: JSON 解析失败: %w", err)
	}
	port := anyToInt(v.Port)
	if v.Add == "" || port <= 0 || port > 65535 || v.ID == "" {
		return nil, "", errors.New("vmess: 缺少 add/port/id")
	}

	cipher := v.Scy
	if cipher == "" {
		cipher = "auto"
	}
	m := map[string]any{
		"type":    "vmess",
		"server":  v.Add,
		"port":    port,
		"uuid":    v.ID,
		"alterId": anyToInt(v.Aid),
		"cipher":  cipher,
	}
	if v.Net != "" && v.Net != "tcp" {
		m["network"] = v.Net
	}
	if v.TLS == "tls" {
		m["tls"] = true
		if v.SNI != "" {
			m["servername"] = v.SNI
		}
	}
	if v.Alpn != "" {
		parts := strings.Split(v.Alpn, ",")
		out := parts[:0]
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			m["alpn"] = out
		}
	}
	switch v.Net {
	case "ws":
		ws := map[string]any{}
		if v.Path != "" {
			ws["path"] = v.Path
		}
		if v.Host != "" {
			ws["headers"] = map[string]string{"Host": v.Host}
		}
		m["ws-opts"] = ws
	case "grpc":
		m["grpc-opts"] = map[string]any{"grpc-service-name": v.Path}
	}
	return m, fallbackName(v.PS, v.Add, port), nil
}

// anyToInt 把 JSON 中可能是字符串或数字的字段转为 int。
func anyToInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	}
	return 0
}

// parseVLESS 解析 vless://uuid@host:port?...#name 链接。
func parseVLESS(link string) (map[string]any, string, error) {
	u, err := url.Parse(link)
	if err != nil {
		return nil, "", fmt.Errorf("vless: %w", err)
	}
	uuid := u.User.Username()
	host, port, err := splitHostPort(u.Host)
	if err != nil {
		return nil, "", err
	}
	if uuid == "" {
		return nil, "", errors.New("vless: 缺少 uuid")
	}
	q := u.Query()

	m := map[string]any{
		"type":   "vless",
		"server": host,
		"port":   port,
		"uuid":   uuid,
	}
	security := q.Get("security")
	switch security {
	case "tls":
		m["tls"] = true
	case "reality":
		m["tls"] = true
		ro := map[string]any{"public-key": q.Get("pbk")}
		if sid := q.Get("sid"); sid != "" {
			ro["short-id"] = sid
		}
		m["reality-opts"] = ro
	}
	if sni := q.Get("sni"); sni != "" {
		m["servername"] = sni
	}
	if flow := q.Get("flow"); flow != "" {
		m["flow"] = flow
	}
	if fp := q.Get("fp"); fp != "" {
		m["client-fingerprint"] = fp
	}
	if enc := q.Get("encryption"); enc != "" && enc != "none" {
		m["encryption"] = enc
	}
	if queryBool(q, "allowInsecure", "allow_insecure", "insecure") {
		m["skip-cert-verify"] = true
	}
	if alpn := queryALPN(q); len(alpn) > 0 {
		m["alpn"] = alpn
	}
	if network := q.Get("type"); network != "" && network != "tcp" {
		m["network"] = network
		setTransportOpts(m, network, q)
	}
	return m, fallbackName(fragmentName(u), host, port), nil
}

// parseTrojan 解析 trojan://password@host:port?...#name 链接。
func parseTrojan(link string) (map[string]any, string, error) {
	u, err := url.Parse(link)
	if err != nil {
		return nil, "", fmt.Errorf("trojan: %w", err)
	}
	password := u.User.Username()
	host, port, err := splitHostPort(u.Host)
	if err != nil {
		return nil, "", err
	}
	if password == "" {
		return nil, "", errors.New("trojan: 缺少密码")
	}
	q := u.Query()

	m := map[string]any{
		"type":     "trojan",
		"server":   host,
		"port":     port,
		"password": password,
	}
	if sni := q.Get("sni"); sni != "" {
		m["sni"] = sni
	}
	if queryBool(q, "allowInsecure", "allow_insecure", "insecure") {
		m["skip-cert-verify"] = true
	}
	if alpn := queryALPN(q); len(alpn) > 0 {
		m["alpn"] = alpn
	}
	if fp := q.Get("fp"); fp != "" {
		m["client-fingerprint"] = fp
	}
	if network := q.Get("type"); network != "" && network != "tcp" {
		m["network"] = network
		setTransportOpts(m, network, q)
	}
	return m, fallbackName(fragmentName(u), host, port), nil
}

// parseHysteria2 解析 hysteria2:// 或 hy2:// 链接（userinfo 即密码）。
func parseHysteria2(link string) (map[string]any, string, error) {
	u, err := url.Parse(link)
	if err != nil {
		return nil, "", fmt.Errorf("hysteria2: %w", err)
	}
	password := u.User.Username()
	host, port, err := splitHostPort(u.Host)
	if err != nil {
		return nil, "", err
	}
	if password == "" {
		return nil, "", errors.New("hysteria2: 缺少密码")
	}
	q := u.Query()

	m := map[string]any{
		"type":     "hysteria2",
		"server":   host,
		"port":     port,
		"password": password,
	}
	if sni := q.Get("sni"); sni != "" {
		m["sni"] = sni
	}
	if queryBool(q, "insecure", "allowInsecure", "allow_insecure") {
		m["skip-cert-verify"] = true
	}
	if alpn := queryALPN(q); len(alpn) > 0 {
		m["alpn"] = alpn
	}
	if obfs := q.Get("obfs"); obfs != "" {
		m["obfs"] = obfs
		if p := q.Get("obfs-password"); p != "" {
			m["obfs-password"] = p
		}
	}
	if mport := q.Get("mport"); mport != "" {
		m["ports"] = mport
	}
	return m, fallbackName(fragmentName(u), host, port), nil
}

// parseTUIC 解析 tuic://uuid:password@host:port?...#name 链接。
func parseTUIC(link string) (map[string]any, string, error) {
	u, err := url.Parse(link)
	if err != nil {
		return nil, "", fmt.Errorf("tuic: %w", err)
	}
	uuid := u.User.Username()
	password, _ := u.User.Password()
	host, port, err := splitHostPort(u.Host)
	if err != nil {
		return nil, "", err
	}
	if uuid == "" {
		return nil, "", errors.New("tuic: 缺少 uuid")
	}
	q := u.Query()

	m := map[string]any{
		"type":   "tuic",
		"server": host,
		"port":   port,
		"uuid":   uuid,
	}
	if password != "" {
		m["password"] = password
	}
	if sni := q.Get("sni"); sni != "" {
		m["sni"] = sni
	}
	if queryBool(q, "insecure", "allowInsecure", "allow_insecure") {
		m["skip-cert-verify"] = true
	}
	if alpn := queryALPN(q); len(alpn) > 0 {
		m["alpn"] = alpn
	}
	if v := q.Get("congestion_controller"); v != "" {
		m["congestion-controller"] = v
	}
	if v := q.Get("udp_relay_mode"); v != "" {
		m["udp-relay-mode"] = v
	}
	if queryBool(q, "disable_sni") {
		m["disable-sni"] = true
	}
	return m, fallbackName(fragmentName(u), host, port), nil
}
