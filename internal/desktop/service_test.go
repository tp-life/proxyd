package desktop

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// stubConn 是服务探测测试使用的最小 net.Conn，实现不执行任何网络 I/O。
type stubConn struct{}

// Read 实现 net.Conn；测试不会调用，始终返回错误。
func (stubConn) Read([]byte) (int, error) { return 0, fmt.Errorf("stub read") }

// Write 实现 net.Conn；测试不会调用，始终返回错误。
func (stubConn) Write([]byte) (int, error) { return 0, fmt.Errorf("stub write") }

// Close 实现 net.Conn；探测成功后调用并返回 nil。
func (stubConn) Close() error { return nil }

// LocalAddr 实现 net.Conn；测试不依赖地址，返回 nil。
func (stubConn) LocalAddr() net.Addr { return nil }

// RemoteAddr 实现 net.Conn；测试不依赖地址，返回 nil。
func (stubConn) RemoteAddr() net.Addr { return nil }

// SetDeadline 实现 net.Conn；测试不使用 deadline，返回 nil。
func (stubConn) SetDeadline(time.Time) error { return nil }

// SetReadDeadline 实现 net.Conn；测试不使用读 deadline，返回 nil。
func (stubConn) SetReadDeadline(time.Time) error { return nil }

// SetWriteDeadline 实现 net.Conn；测试不使用写 deadline，返回 nil。
func (stubConn) SetWriteDeadline(time.Time) error { return nil }

// TestProbeLocalServicesPreservesOrder 验证并发探测保持输入顺序并隔离单项失败。
//
// 参数说明：t 为 Go 测试上下文。
// 返回值说明：无；RDP 成功、VNC 失败且顺序稳定时通过。
// 错误情况：拨号地址错误、失败扩散或结果乱序时测试失败。
func TestProbeLocalServicesPreservesOrder(t *testing.T) {
	checkedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	statuses := probeLocalServices(context.Background(), []ServiceSpec{
		{Protocol: ProtocolRDP, Port: 3389},
		{Protocol: ProtocolVNC, Port: 5900},
	}, func(_ context.Context, _, address string) (net.Conn, error) {
		if address == "127.0.0.1:3389" {
			return stubConn{}, nil
		}
		return nil, fmt.Errorf("connection refused")
	}, checkedAt)
	if len(statuses) != 2 || statuses[0].Protocol != ProtocolRDP || !statuses[0].Listening || statuses[1].Protocol != ProtocolVNC || statuses[1].Listening {
		t.Fatalf("服务探测结果错误: %+v", statuses)
	}
	if !statuses[0].CheckedAt.Equal(checkedAt) || !statuses[1].CheckedAt.Equal(checkedAt) {
		t.Fatalf("服务探测时间不一致: %+v", statuses)
	}
}
