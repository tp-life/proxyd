package subscribe

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"testing"
)

// b64u 是测试用的 base64 RawURLEncoding 编码。
func b64u(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// encodeShareBody 把多行链接按订阅格式整体 base64 编码。
func encodeShareBody(lines ...string) string {
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	return base64.StdEncoding.EncodeToString([]byte(body))
}

// buildShareLinks 构造各协议样本链接。
func buildShareLinks() map[string]string {
	links := map[string]string{}

	// ss SIP002: base64(method:password)@host:port#name
	userinfo := b64u("aes-256-gcm:ss-pass-sip002")
	links["ss-sip002"] = fmt.Sprintf("ss://%s@1.2.3.4:8388#%s", userinfo, url.PathEscape("SS SIP002 节点"))

	// ss 旧格式: base64(method:password@host:port)#name
	links["ss-legacy"] = "ss://" + b64u("chacha20-ietf-poly1305:ss-pass-legacy@5.6.7.8:8389") + "#SS-Legacy"

	// ssr: base64(host:port:protocol:method:obfs:base64pass/?...&remarks=...)
	ssrRaw := fmt.Sprintf("9.8.7.6:443:origin:aes-256-cfb:plain:%s/?obfsparam=%s&protoparam=%s&remarks=%s",
		b64u("ssr-pass"), b64u("obfsp"), b64u("protop"), b64u("SSR 节点"))
	links["ssr"] = "ssr://" + b64u(ssrRaw)

	// vmess: base64(JSON)
	links["vmess"] = "vmess://" + base64.StdEncoding.EncodeToString([]byte(
		`{"v":"2","ps":"VMess 节点","add":"vm.example.com","port":"443","id":"11111111-2222-3333-4444-555555555555","aid":"0","scy":"auto","net":"ws","type":"none","host":"cdn.example.com","path":"/ray","tls":"tls","sni":"sni.example.com"}`))

	// vless reality + ws
	links["vless"] = "vless://22222222-3333-4444-5555-666666666666@vl.example.com:443" +
		"?security=reality&sni=www.microsoft.com&fp=chrome&pbk=PUBKEY123&sid=ab12&type=ws&path=%2Fvless&host=cdn2.example.com#VLESS-Node"

	// trojan + grpc
	links["trojan"] = "trojan://tj-pass@tj.example.com:443?sni=tj.example.com&type=grpc&serviceName=tjsvc#Trojan-Node"

	// hysteria2
	links["hy2"] = "hy2://hy2-pass@hy2.example.com:8443?sni=hy2.example.com&insecure=1#HY2-Node"

	return links
}

func TestParseShare(t *testing.T) {
	links := buildShareLinks()
	order := []string{"ss-sip002", "ss-legacy", "ssr", "vmess", "vless", "trojan", "hy2"}
	lines := make([]string, 0, len(order))
	for _, k := range order {
		lines = append(lines, links[k])
	}
	nodes, err := ParseShare([]byte(encodeShareBody(lines...)), "机场B")
	if err != nil {
		t.Fatalf("ParseShare 失败: %v", err)
	}
	if len(nodes) != len(order) {
		t.Fatalf("期望 %d 个节点，得到 %d", len(order), len(nodes))
	}
	byName := map[string]map[string]any{}
	for _, n := range nodes {
		byName[n.Name] = n.Mapping
	}

	ss := byName["SS SIP002 节点"]
	if ss == nil {
		t.Fatalf("缺少 ss-sip002 节点，已有: %v", keys(byName))
	}
	if ss["type"] != "ss" || ss["server"] != "1.2.3.4" || ss["port"] != 8388 ||
		ss["cipher"] != "aes-256-gcm" || ss["password"] != "ss-pass-sip002" {
		t.Errorf("ss SIP002 字段不正确: %+v", ss)
	}

	legacy := byName["SS-Legacy"]
	if legacy == nil || legacy["type"] != "ss" || legacy["server"] != "5.6.7.8" ||
		legacy["port"] != 8389 || legacy["cipher"] != "chacha20-ietf-poly1305" || legacy["password"] != "ss-pass-legacy" {
		t.Errorf("ss 旧格式字段不正确: %+v", legacy)
	}

	ssr := byName["SSR 节点"]
	if ssr == nil || ssr["type"] != "ssr" || ssr["server"] != "9.8.7.6" || ssr["port"] != 443 ||
		ssr["cipher"] != "aes-256-cfb" || ssr["password"] != "ssr-pass" ||
		ssr["protocol"] != "origin" || ssr["obfs"] != "plain" ||
		ssr["obfs-param"] != "obfsp" || ssr["protocol-param"] != "protop" {
		t.Errorf("ssr 字段不正确: %+v", ssr)
	}

	vm := byName["VMess 节点"]
	if vm == nil || vm["type"] != "vmess" || vm["server"] != "vm.example.com" || vm["port"] != 443 ||
		vm["uuid"] != "11111111-2222-3333-4444-555555555555" || vm["alterId"] != 0 ||
		vm["cipher"] != "auto" || vm["network"] != "ws" || vm["tls"] != true ||
		vm["servername"] != "sni.example.com" {
		t.Errorf("vmess 字段不正确: %+v", vm)
	}
	ws, _ := vm["ws-opts"].(map[string]any)
	if ws == nil || ws["path"] != "/ray" {
		t.Errorf("vmess ws-opts 不正确: %+v", vm["ws-opts"])
	}
	if hdrs, _ := ws["headers"].(map[string]string); hdrs["Host"] != "cdn.example.com" {
		t.Errorf("vmess ws-opts headers 不正确: %+v", ws["headers"])
	}

	vl := byName["VLESS-Node"]
	if vl == nil || vl["type"] != "vless" || vl["server"] != "vl.example.com" || vl["port"] != 443 ||
		vl["uuid"] != "22222222-3333-4444-5555-666666666666" ||
		vl["tls"] != true || vl["servername"] != "www.microsoft.com" ||
		vl["client-fingerprint"] != "chrome" || vl["network"] != "ws" {
		t.Errorf("vless 字段不正确: %+v", vl)
	}
	ro, _ := vl["reality-opts"].(map[string]any)
	if ro == nil || ro["public-key"] != "PUBKEY123" || ro["short-id"] != "ab12" {
		t.Errorf("vless reality-opts 不正确: %+v", vl["reality-opts"])
	}

	tj := byName["Trojan-Node"]
	if tj == nil || tj["type"] != "trojan" || tj["server"] != "tj.example.com" || tj["port"] != 443 ||
		tj["password"] != "tj-pass" || tj["sni"] != "tj.example.com" || tj["network"] != "grpc" {
		t.Errorf("trojan 字段不正确: %+v", tj)
	}
	grpc, _ := tj["grpc-opts"].(map[string]any)
	if grpc == nil || grpc["grpc-service-name"] != "tjsvc" {
		t.Errorf("trojan grpc-opts 不正确: %+v", tj["grpc-opts"])
	}

	hy2 := byName["HY2-Node"]
	if hy2 == nil || hy2["type"] != "hysteria2" || hy2["server"] != "hy2.example.com" ||
		hy2["port"] != 8443 || hy2["password"] != "hy2-pass" ||
		hy2["sni"] != "hy2.example.com" || hy2["skip-cert-verify"] != true {
		t.Errorf("hysteria2 字段不正确: %+v", hy2)
	}
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestParseSharePlaintextAndSkip(t *testing.T) {
	links := buildShareLinks()
	// 明文（未 base64）+ 一行垃圾 + 一行不支持协议，应容错跳过
	body := links["trojan"] + "\ngarbage-line\nhttp://not-supported\n" + links["ss-sip002"] + "\n"
	nodes, err := ParseShare([]byte(body), "sub")
	if err != nil {
		t.Fatalf("ParseShare 失败: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("期望 2 个节点，得到 %d", len(nodes))
	}
}

func TestParseShareAllBad(t *testing.T) {
	if _, err := ParseShare([]byte("%%%not-base64%%%"), "sub"); err == nil {
		t.Fatal("全部解析失败时应返回错误")
	}
}
