package main

import (
	"testing"

	"proxyd/internal/app"
)

func TestParseAutoPortArg(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"off", 0, true},
		{"OFF", 0, true},
		{"0", 0, true},
		{"41998", 41998, true},
		{"65535", 65535, true},
		{"65536", 0, false},
		{"-1", 0, false},
		{"abc", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, err := parseAutoPortArg(c.in)
		if c.ok != (err == nil) || got != c.want {
			t.Errorf("parseAutoPortArg(%q) = %d, %v; want %d, ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

func TestResolveManualIndex(t *testing.T) {
	entries := []app.ManualNodeEntry{
		{Index: 0, URL: "http://h:8080", Name: "节点甲"},
		{Index: 1, URL: "socks5://h:1080", Name: "h:1080"},
	}
	if idx, err := resolveManualIndex(entries, "1"); err != nil || idx != 1 {
		t.Errorf("by index: %d, %v", idx, err)
	}
	if idx, err := resolveManualIndex(entries, "节点甲"); err != nil || idx != 0 {
		t.Errorf("by name: %d, %v", idx, err)
	}
	if _, err := resolveManualIndex(entries, "5"); err == nil {
		t.Error("下标越界应报错")
	}
	if _, err := resolveManualIndex(entries, "不存在"); err == nil {
		t.Error("未知名称应报错")
	}
}

func TestParseCFlag(t *testing.T) {
	cfg, rest := parseCFlag("t", []string{"-c", "/tmp/x.yaml", "add", "u"})
	if cfg != "/tmp/x.yaml" || len(rest) != 2 || rest[0] != "add" {
		t.Errorf("cfg=%q rest=%v", cfg, rest)
	}
	// flag 在位置参数之后不再解析（Go flag 语义，usage 已注明）
	cfg, rest = parseCFlag("t", []string{"add", "-c", "/tmp/x.yaml"})
	if len(rest) != 3 || rest[0] != "add" {
		t.Errorf("rest=%v", rest)
	}
}
