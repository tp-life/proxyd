package remote

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// TransientForward 是不写入配置文件的临时本地转发。
//
// 它复用常驻转发的数据面实现，但生命周期完全由调用它的前台命令控制；这保证桌面
// 客户端关闭后监听端口和 tailcat 客户端会一起释放，不会留下隐藏的长期入口。
type TransientForward struct {
	runner *forwardRunner
	once   sync.Once
}

// StartTransientForward 在随机回环端口启动一条临时 tailcat TCP 转发。
//
// 参数说明：
//   - token: string，远端完整 tailcat 连接 token。
//   - remotePort: int，远端 RDP/VNC 等服务监听端口。
//   - clientKey: key.NodePrivate，本机稳定客户端身份；零值表示使用临时身份。
//
// 返回值说明：*TransientForward 和 error；成功对象可通过 Address 获取本地地址，
// 使用结束必须调用 Close。
//
// 错误情况：token 非法、端口越界或本机无法绑定回环监听时返回错误；失败时不会留下
// listener 或 tailcat 客户端资源。
func StartTransientForward(token string, remotePort int, clientKey key.NodePrivate) (*TransientForward, error) {
	if err := ValidateToken(token); err != nil {
		return nil, err
	}
	if remotePort <= 0 || remotePort > 65535 {
		return nil, fmt.Errorf("端口 %d 超出 1-65535", remotePort)
	}
	return startTransientForward("127.0.0.1:0", token, remotePort, clientKey, nil)
}

// startTransientForward 使用可注入拨号器启动临时转发，供生产入口与单元测试复用。
//
// 参数说明：
//   - listen: string，本地监听地址；生产固定为随机回环端口。
//   - token: string，已校验或测试注入的远端 token。
//   - remotePort: int，目标 TCP 端口。
//   - clientKey: key.NodePrivate，tailcat 客户端身份。
//   - dial: func，可选测试拨号器；nil 时使用真实 tailcat 客户端。
//
// 返回值说明：*TransientForward 和 error；成功时 accept loop 已经启动。
//
// 错误情况：本地监听失败时返回错误，runner 不会进入运行状态。
func startTransientForward(listen, token string, remotePort int, clientKey key.NodePrivate, dial func(ctx context.Context) (net.Conn, error)) (*TransientForward, error) {
	runner := newForwardRunner("desktop", listen, tailcat.ConnBlob(token), uint16(remotePort), logger.Discard, clientKey, dial)
	if err := runner.start(); err != nil {
		return nil, err
	}
	return &TransientForward{runner: runner}, nil
}

// Address 返回桌面客户端应连接的本地回环地址。
//
// 参数说明：无。
//
// 返回值说明：string，格式为 127.0.0.1:<系统分配端口>。
//
// 错误情况：无；该对象只能由成功的 StartTransientForward 构造，因此 listener 必然存在。
func (f *TransientForward) Address() string {
	return f.runner.ln.Addr().String()
}

// ActiveConnections 返回当前正在通过临时转发传输的本地连接数。
//
// 参数说明：无。
//
// 返回值说明：int64；accept 已建立但仍在拨远端的连接也计入活动数。
//
// 错误情况：无；计数使用原子变量读取，允许桌面会话清扫器并发调用。
func (f *TransientForward) ActiveConnections() int64 {
	return f.runner.active.Load()
}

// Close 停止监听并释放 tailcat 客户端及活动转发连接。
//
// 参数说明：无。
//
// 返回值说明：error；当前实现的关闭动作均为尽力而为，因此始终返回 nil。
//
// 错误情况：重复调用是安全的，不会重复关闭 done channel 或触发 panic。
func (f *TransientForward) Close() error {
	f.once.Do(func() {
		f.runner.stop()
	})
	return nil
}
