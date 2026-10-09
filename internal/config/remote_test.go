package config

import "testing"

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
