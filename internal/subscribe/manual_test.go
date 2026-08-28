package subscribe

import (
	"testing"
)

func TestParseManualNodeHTTPWithAuth(t *testing.T) {
	n, err := ParseManualNode("http://user:pass@192.168.1.1:8080#my-http")
	if err != nil {
		t.Fatalf("ParseManualNode: %v", err)
	}
	if n.Subscription != ManualSubscription {
		t.Errorf("Subscription = %q, want manual", n.Subscription)
	}
	if n.Name != "my-http" {
		t.Errorf("Name = %q, want my-http", n.Name)
	}
	m := n.Mapping
	if m["type"] != "http" || m["server"] != "192.168.1.1" || m["port"] != 8080 {
		t.Errorf("mapping: %+v", m)
	}
	if m["username"] != "user" || m["password"] != "pass" {
		t.Errorf("auth: %+v", m)
	}
	if _, ok := m["tls"]; ok {
		t.Errorf("http 不应带 tls: %+v", m)
	}
	if m["name"] != "my-http" {
		t.Errorf("mapping name 未同步: %+v", m)
	}
}

func TestParseManualNodeSocks5(t *testing.T) {
	n, err := ParseManualNode("socks5://u:p@10.0.0.2:1080")
	if err != nil {
		t.Fatalf("ParseManualNode: %v", err)
	}
	m := n.Mapping
	if m["type"] != "socks5" || m["server"] != "10.0.0.2" || m["port"] != 1080 {
		t.Errorf("mapping: %+v", m)
	}
	if m["username"] != "u" || m["password"] != "p" {
		t.Errorf("auth: %+v", m)
	}
	if n.Name != "10.0.0.2:1080" {
		t.Errorf("无 fragment 时应兜底 host:port, got %q", n.Name)
	}
}

func TestParseManualNodeHTTPSNoAuth(t *testing.T) {
	n, err := ParseManualNode("https://example.com:8443")
	if err != nil {
		t.Fatalf("ParseManualNode: %v", err)
	}
	if n.Mapping["type"] != "http" || n.Mapping["tls"] != true {
		t.Errorf("mapping: %+v", n.Mapping)
	}
	if _, ok := n.Mapping["username"]; ok {
		t.Errorf("无认证不应有 username: %+v", n.Mapping)
	}
}

func TestParseManualNodeShareLink(t *testing.T) {
	// ss://method:password@host:port#name（SIP002 明文 userinfo）
	n, err := ParseManualNode("ss://aes-128-gcm:pw@1.2.3.4:8388#ss-node")
	if err != nil {
		t.Fatalf("ParseManualNode: %v", err)
	}
	if n.Subscription != ManualSubscription || n.Name != "ss-node" {
		t.Errorf("node: %+v", n)
	}
	if n.Mapping["type"] != "ss" || n.Mapping["cipher"] != "aes-128-gcm" || n.Mapping["password"] != "pw" {
		t.Errorf("mapping: %+v", n.Mapping)
	}
}

func TestParseManualNodeInvalid(t *testing.T) {
	for _, bad := range []string{"", "not-a-url", "http://nohost", "socks5://h:0", "foo://bar:1"} {
		if _, err := ParseManualNode(bad); err == nil {
			t.Errorf("%q 应解析失败", bad)
		}
	}
}

func TestParseManualNodesPartial(t *testing.T) {
	nodes, errs := ParseManualNodes([]string{"http://h:8080", "bad", "socks5://h:1080"})
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(nodes))
	}
	if errs[0] != nil || errs[1] == nil || errs[2] != nil {
		t.Fatalf("errs = %v", errs)
	}
}
