package config

import "testing"

// TestDesktopConfigDefaultsAndClone 验证桌面默认端口与配置事务克隆互不共享切片。
//
// 参数说明：t 为 Go 测试上下文。
//
// 返回值说明：无；默认值、连接默认端口和深复制断言均成立时通过。
//
// 错误情况：旧配置缺少 desktop 段却未补默认值，或克隆修改污染源配置时测试失败。
func TestDesktopConfigDefaultsAndClone(t *testing.T) {
	configuration := DesktopConfig{Connections: []DesktopConnection{{
		Name: "办公室电脑", Remote: "office", Protocol: DesktopProtocolRDP,
	}}}
	configuration.ApplyDefaults()
	if configuration.RDPPort != DefaultDesktopRDPPort || configuration.VNCPort != DefaultDesktopVNCPort {
		t.Fatalf("桌面服务默认端口错误: %+v", configuration)
	}
	if got := configuration.Connections[0].RemotePort; got != DefaultDesktopRDPPort {
		t.Fatalf("RDP 连接默认端口 = %d，期望 %d", got, DefaultDesktopRDPPort)
	}
	clone := configuration.Clone()
	clone.Connections[0].Name = "已修改"
	if configuration.Connections[0].Name != "办公室电脑" {
		t.Fatalf("Clone 与源配置共享 Connections: %+v", configuration.Connections)
	}
}

// TestDesktopConfigValidation 验证端口、唯一名称和用户名注入边界。
//
// 参数说明：t 为 Go 测试上下文。
//
// 返回值说明：无；合法配置通过且每种非法配置均被拒绝时通过。
//
// 错误情况：RDP/VNC 共用端口、连接重名、非法协议或用户名换行未被阻止时测试失败。
func TestDesktopConfigValidation(t *testing.T) {
	valid := DesktopConfig{
		RDPPort: 3390,
		VNCPort: 5901,
		Connections: []DesktopConnection{{
			Name: "办公室", Remote: "office", Protocol: DesktopProtocolRDP, RemotePort: 3389, Username: `DOMAIN\user`,
		}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法桌面配置被拒绝: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*DesktopConfig)
	}{
		{name: "服务端口冲突", mutate: func(configuration *DesktopConfig) { configuration.VNCPort = configuration.RDPPort }},
		{name: "档案重名", mutate: func(configuration *DesktopConfig) {
			configuration.Connections = append(configuration.Connections, configuration.Connections[0])
		}},
		{name: "非法协议", mutate: func(configuration *DesktopConfig) { configuration.Connections[0].Protocol = "spice" }},
		{name: "用户名换行", mutate: func(configuration *DesktopConfig) {
			configuration.Connections[0].Username = "user\r\nfull address:s:evil"
		}},
		{name: "直接保存token", mutate: func(configuration *DesktopConfig) {
			configuration.Connections[0].Remote = "tcAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configuration := valid.Clone()
			test.mutate(&configuration)
			if err := configuration.Validate(); err == nil {
				t.Fatalf("非法桌面配置未被拒绝: %+v", configuration)
			}
		})
	}
}
