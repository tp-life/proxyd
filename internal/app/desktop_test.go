package app

import (
	"path/filepath"
	"testing"

	"proxyd/internal/config"
)

// newDesktopTestApp 构造可验证原子落盘的桌面应用实例。
//
// 参数说明：t 为 Go 测试上下文。
//
// 返回值说明：返回 App 与配置文件路径；App 在测试结束时自动关闭。
//
// 错误情况：基础配置或 App 构造失败时立即终止当前测试。
func newDesktopTestApp(t *testing.T) (*App, string) {
	t.Helper()
	directory := t.TempDir()
	configuration, err := config.Quick([]string{"https://example.invalid/desktop-test"}, "")
	if err != nil {
		t.Fatalf("构造桌面测试配置失败: %v", err)
	}
	configuration.StateDir = directory
	configuration.APIListen = "127.0.0.1:19091"
	path := filepath.Join(directory, "config.yaml")
	application, err := New(configuration, path)
	if err != nil {
		t.Fatalf("构造桌面测试 App 失败: %v", err)
	}
	t.Cleanup(application.Shutdown)
	return application, path
}

// TestDesktopConnectionPersistence 验证连接档案新增、重命名、删除均走配置事务。
//
// 参数说明：t 为 Go 测试上下文。
//
// 返回值说明：无；每一步内存状态与最终磁盘状态一致时通过。
//
// 错误情况：重复名称被接受、更新找不到原档案、删除未落盘或密码类字段意外出现时失败。
func TestDesktopConnectionPersistence(t *testing.T) {
	application, path := newDesktopTestApp(t)
	connection := config.DesktopConnection{
		Name: " 办公室电脑 ", Remote: " office ", Protocol: "RDP", Username: ` DOMAIN\user `,
	}
	if err := application.AddDesktopConnection(connection); err != nil {
		t.Fatalf("新增桌面连接失败: %v", err)
	}
	stored := application.Config().Desktop.Connections
	if len(stored) != 1 || stored[0].Name != "办公室电脑" || stored[0].RemotePort != 3389 || stored[0].Protocol != "rdp" {
		t.Fatalf("桌面连接未规范化保存: %+v", stored)
	}
	if err := application.AddDesktopConnection(stored[0]); err == nil {
		t.Fatal("重复桌面连接名称应被拒绝")
	}

	updated := stored[0]
	updated.Name = "机房电脑"
	updated.Protocol = "vnc"
	updated.RemotePort = 5901
	if err := application.UpdateDesktopConnection("办公室电脑", updated); err != nil {
		t.Fatalf("更新桌面连接失败: %v", err)
	}
	if err := application.DeleteDesktopConnection("机房电脑"); err != nil {
		t.Fatalf("删除桌面连接失败: %v", err)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("重载落盘配置失败: %v", err)
	}
	if len(reloaded.Desktop.Connections) != 0 {
		t.Fatalf("删除后磁盘仍有桌面连接: %+v", reloaded.Desktop.Connections)
	}
}

// TestSetDesktopServiceMovesExposedPort 验证修改已开放服务端口时跨配置原子迁移。
//
// 参数说明：t 为 Go 测试上下文。
//
// 返回值说明：无；旧端口被移除、新端口成为唯一开放项且磁盘一致时通过。
//
// 错误情况：desktop 配置与 remote.serve 漂移、旧端口残留或关闭开放状态失效时失败。
func TestSetDesktopServiceMovesExposedPort(t *testing.T) {
	application, path := newDesktopTestApp(t)
	if err := application.SetDesktopService(config.DesktopProtocolRDP, 3389, true); err != nil {
		t.Fatalf("开放 RDP 默认端口失败: %v", err)
	}
	if err := application.SetDesktopService(config.DesktopProtocolRDP, 3390, true); err != nil {
		t.Fatalf("迁移 RDP 服务端口失败: %v", err)
	}
	current := application.Config()
	if current.Desktop.RDPPort != 3390 || len(current.Remote.Serve) != 1 || current.Remote.Serve[0] != 3390 {
		t.Fatalf("端口迁移后配置不一致: desktop=%+v serve=%v", current.Desktop, current.Remote.Serve)
	}
	if err := application.SetDesktopService(config.DesktopProtocolRDP, 3390, false); err != nil {
		t.Fatalf("关闭 RDP 隧道开放失败: %v", err)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("重载桌面服务配置失败: %v", err)
	}
	if reloaded.Desktop.RDPPort != 3390 || len(reloaded.Remote.Serve) != 0 {
		t.Fatalf("磁盘桌面服务配置不一致: desktop=%+v serve=%v", reloaded.Desktop, reloaded.Remote.Serve)
	}
}
