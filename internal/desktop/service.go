package desktop

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// ServiceSpec 描述需要检测的本机桌面服务。
type ServiceSpec struct {
	Protocol Protocol
	Port     int
}

// ServiceStatus 是一次本机回环端口检测结果。
type ServiceStatus struct {
	Protocol  Protocol
	Port      int
	Listening bool
	CheckedAt time.Time
}

// serviceDialer 是本机服务探测所需的最小网络端口，便于单元测试隔离实际 socket。
type serviceDialer func(ctx context.Context, network, address string) (net.Conn, error)

// ProbeLocalServices 并发检测一组桌面服务是否在本机回环地址监听。
//
// 参数说明：ctx 控制请求取消；specs 是经过配置层校验的协议与端口列表。
//
// 返回值说明：[]ServiceStatus，顺序与输入一致；每项包含统一检查时间和监听结果。
//
// 错误情况：单个端口拒绝、超时或上下文取消都折叠为 Listening=false，避免一个未开启
// 的可选服务让整个状态页失败。该探测只建立 TCP 握手后立即关闭，不发送协议数据。
func ProbeLocalServices(ctx context.Context, specs []ServiceSpec) []ServiceStatus {
	dialer := &net.Dialer{Timeout: 350 * time.Millisecond}
	return probeLocalServices(ctx, specs, dialer.DialContext, time.Now())
}

// probeLocalServices 使用注入拨号器执行并发探测。
//
// 参数说明：ctx 控制取消；specs 为目标；dial 为测试或生产拨号器；checkedAt 为统一时间。
//
// 返回值说明：[]ServiceStatus，保持输入顺序，避免并发完成顺序导致 UI 卡片跳动。
//
// 错误情况：拨号错误只标记对应项未监听；成功连接会立即关闭，关闭错误没有业务影响。
func probeLocalServices(ctx context.Context, specs []ServiceSpec, dial serviceDialer, checkedAt time.Time) []ServiceStatus {
	statuses := make([]ServiceStatus, len(specs))
	var wait sync.WaitGroup
	wait.Add(len(specs))
	for index, spec := range specs {
		go func() {
			defer wait.Done()
			status := ServiceStatus{Protocol: spec.Protocol, Port: spec.Port, CheckedAt: checkedAt}
			if spec.Port < 1 || spec.Port > 65535 || (spec.Protocol != ProtocolRDP && spec.Protocol != ProtocolVNC) {
				statuses[index] = status
				return
			}
			address := net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.Port))
			connection, err := dial(ctx, "tcp", address)
			if err == nil {
				status.Listening = true
				_ = connection.Close()
			}
			statuses[index] = status
		}()
	}
	wait.Wait()
	return statuses
}

// ValidateServiceSpec 校验桌面服务探测与开放动作的协议和端口。
//
// 参数说明：spec 为目标协议及实际系统服务端口。
//
// 返回值说明：error；协议和端口合法时返回 nil。
//
// 错误情况：未知协议或端口越界返回错误，防止 API 以桌面功能名义开放任意非法端口。
func ValidateServiceSpec(spec ServiceSpec) error {
	if spec.Protocol != ProtocolRDP && spec.Protocol != ProtocolVNC {
		return fmt.Errorf("桌面协议 %q 不受支持", spec.Protocol)
	}
	if spec.Port < 1 || spec.Port > 65535 {
		return fmt.Errorf("桌面服务端口 %d 超出 1-65535", spec.Port)
	}
	return nil
}
