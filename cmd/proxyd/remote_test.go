package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

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

// TestParseRemoteAllowAdd 验证 CLI 能按计划解析别名、TTL 与端口最小权限。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：纯参数解析测试，不访问网络；非法 TTL 与缺失 flag 值必须返回错误。
func TestParseRemoteAllowAdd(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	entry, err := parseRemoteAllowAdd([]string{"nodekey:test", "家里", "--ttl", "1h", "--ports=22,8080"}, now)
	if err != nil {
		t.Fatalf("合法 allow add 参数解析失败: %v", err)
	}
	if entry.Key != "nodekey:test" || entry.Name != "家里" || entry.ExpiresAt == nil || !entry.ExpiresAt.Equal(now.Add(time.Hour)) || !reflect.DeepEqual(entry.Ports, []int{22, 8080}) {
		t.Fatalf("解析结果错误: %+v", entry)
	}
	if _, err := parseRemoteAllowAdd([]string{"nodekey:test", "--ttl", "0"}, now); err == nil {
		t.Fatal("非正 TTL 应被拒绝")
	}
	if _, err := parseRemoteAllowAdd([]string{"nodekey:test", "--ports"}, now); err == nil {
		t.Fatal("缺少 --ports 值应被拒绝")
	}
}

// TestFormatRemoteExpiry 验证永久、过期和短期授权的命令行展示语义。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：无外部错误；失败表示 TTL 展示边界回归。
func TestFormatRemoteExpiry(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if got := formatRemoteExpiry(nil, now); got != "永久" {
		t.Fatalf("nil 过期时间应显示永久，got %q", got)
	}
	expired := now
	if got := formatRemoteExpiry(&expired, now); got != "已过期" {
		t.Fatalf("到达边界应显示已过期，got %q", got)
	}
	soon := now.Add(30 * time.Second)
	if got := formatRemoteExpiry(&soon, now); got != "<1m" {
		t.Fatalf("不足一分钟应显示 <1m，got %q", got)
	}
}
