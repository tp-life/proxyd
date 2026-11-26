package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestValidateRemoteRelay 验证中继值对象接受自动、固定区域、自建主机和私有地图，
// 同时在进入运行态前拒绝含端口的主机、非 HTTP(S) URL 与非法区域。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；所有输入的期望与实际校验结果一致时测试通过。
//
// 错误情况：任一合法输入被拒绝或非法输入被接受时调用 t.Errorf 标记失败。
func TestValidateRemoteRelay(t *testing.T) {
	tests := []struct {
		name       string
		region     string
		derpMapURL string
		wantError  bool
	}{
		{name: "默认自动"},
		{name: "固定区域", region: "302"},
		{name: "多个自建主机", region: "derp-a.example.com, derp-b.example.com"},
		{name: "私有 HTTPS 地图", derpMapURL: "https://control.example.com/derpmap/default?token=secret"},
		{name: "内网 HTTP 地图", derpMapURL: "http://derp.internal.example/map"},
		{name: "非正区域", region: "0", wantError: true},
		{name: "主机携带端口", region: "derp.example.com:8443", wantError: true},
		{name: "主机携带 scheme", region: "https://derp.example.com", wantError: true},
		{name: "相对地图", derpMapURL: "/derpmap.json", wantError: true},
		{name: "非 HTTP 地图", derpMapURL: "file:///tmp/derp.json", wantError: true},
		{name: "地图 fragment", derpMapURL: "https://control.example.com/map#private", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRemoteRelay(test.region, test.derpMapURL)
			if (err != nil) != test.wantError {
				t.Errorf("ValidateRemoteRelay(%q, %q) error = %v, wantError = %v", test.region, test.derpMapURL, err, test.wantError)
			}
		})
	}
}

// TestValidateRemoteServeTargets 验证本机异端口、局域网主机、域名与 Unix 套接字映射校验规则。
func TestValidateRemoteServeTargets(t *testing.T) {
	valid := []RemoteServeTarget{
		{Port: 8080, Target: "80"},
		{Port: 5555, Target: "10.2.200.213:5555"},
		{Port: 5556, Target: "android.lan:5555"},
		{Port: 8022, Target: "/run/app.sock"},
		{Port: 8023, Target: "unix:/tmp/demo.sock"},
	}
	if err := ValidateRemoteServeTargets([]int{22}, valid); err != nil {
		t.Fatalf("合法映射应通过校验: %v", err)
	}
	if netType, addr, err := ParseRemoteServeTarget("80"); err != nil || netType != "loopback" || addr != "80" {
		t.Fatalf("纯端口解析错误: %q %q %v", netType, addr, err)
	}
	if netType, addr, err := ParseRemoteServeTarget("unix:/tmp/a.sock"); err != nil || netType != "unix" || addr != "/tmp/a.sock" {
		t.Fatalf("unix: 解析错误: %q %q %v", netType, addr, err)
	}
	if err := ValidateRemoteServeTargets([]int{8080}, []RemoteServeTarget{{Port: 8080, Target: "80"}}); err == nil {
		t.Fatal("与普通暴露端口冲突时应报错")
	}
	for _, bad := range []RemoteServeTarget{
		{Port: 0, Target: "80"},
		{Port: 8080, Target: ""},
		{Port: 8080, Target: "0"},
		{Port: 8080, Target: "99999"},
		{Port: 8080, Target: "unix:"},
		{Port: 8080, Target: "not-a-target"},
		{Port: 8080, Target: "10.0.0.1:0"},
	} {
		if err := ValidateRemoteServeTargets(nil, []RemoteServeTarget{bad}); err == nil {
			t.Errorf("非法映射 %+v 应被拒绝", bad)
		}
	}
}

// TestLoadRemotePeersIgnoresProxySection 验证精简配置（缺 port-range/rules）仍可解析远端名。
func TestLoadRemotePeersIgnoresProxySection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "mode: rule\nremote:\n  remotes:\n    - name: mac\n      token: tcAAAA\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("前提：完整 Load 应拒绝缺 port-range 的配置")
	}
	peers, err := LoadRemotePeers(path)
	if err != nil {
		t.Fatalf("LoadRemotePeers: %v", err)
	}
	if len(peers) != 1 || peers[0].Name != "mac" {
		t.Errorf("peers = %+v", peers)
	}
}
