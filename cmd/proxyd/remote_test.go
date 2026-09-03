package main

import (
	"strings"
	"testing"

	"github.com/tailscale/tailcat"
)

// newSCPTestToken 本地生成合法的 tailcat 连接 token（不触网）。
func newSCPTestToken(t *testing.T) string {
	t.Helper()
	priv := tailcat.NewPrivateKey()
	ci := priv.Public
	ci.RegionID = 1
	return string(ci.ConnBlob())
}

// TestFindTunnelToken 验证 scp 参数中隧道 token 操作数的识别规则：
// user@ 前缀与 :path 后缀可剥离；零个 token 走用法错误；两个不同 token 报单跳限制。
func TestFindTunnelToken(t *testing.T) {
	tokenA := newSCPTestToken(t)
	tokenB := newSCPTestToken(t)

	cases := []struct {
		name string
		args []string
		want string // 期望返回的 token；空串表示期望报错
	}{
		{"下载", []string{tokenA + ":/var/log/a.log", "./"}, tokenA},
		{"上传", []string{"./file.txt", tokenA + ":/tmp/"}, tokenA},
		{"带用户前缀", []string{"root@" + tokenA + ":/tmp/", "./"}, tokenA},
		{"scp 选项透传", []string{"-P", "22", "-r", "./dir", tokenA + ":/tmp/"}, tokenA},
		{"同一 token 出现两次（保留路径）", []string{tokenA + ":/a", tokenA + ":/b"}, tokenA},
		{"本地文件同名前缀不误判", []string{"./tc-notes.txt", "/tmp/"}, ""},
		{"无隧道操作数", []string{"./a.txt", "/tmp/"}, ""},
		{"两个不同 token", []string{tokenA + ":/a", tokenB + ":/b"}, ""},
	}
	for _, c := range cases {
		got, err := findTunnelToken(c.args)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: findTunnelToken(%v) = %q，期望报错", c.name, c.args, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: findTunnelToken(%v) = %q, %v；期望 %q", c.name, c.args, got, err, c.want)
		}
	}

	// 用法错误文案需提示 token 主机名写法。
	_, err := findTunnelToken([]string{"./a.txt", "/tmp/"})
	if err == nil || !strings.Contains(err.Error(), "tc") {
		t.Fatalf("无隧道操作数时应返回含用法提示的错误，got %v", err)
	}
}
