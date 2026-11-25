package remote

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// desktopWarmupRefreshInterval 是活动桌面会话刷新路径与 RTT 的周期。
const desktopWarmupRefreshInterval = 15 * time.Second

// TransportSnapshot 是临时桌面转发的实时隧道质量快照（直连/DERP、RTT、是否启用 UDP）。
type TransportSnapshot struct {
	Path       string    `json:"path"`
	Endpoint   string    `json:"endpoint,omitempty"`
	DERPRegion string    `json:"derp_region,omitempty"`
	RTTMillis  int64     `json:"rtt_ms,omitempty"`
	Warming    bool      `json:"warming"`
	UDPEnabled bool      `json:"udp_enabled"`
	Error      string    `json:"error,omitempty"`
	CheckedAt  time.Time `json:"checked_at,omitempty"`
}

// TransientForward 是不写入配置文件的临时本地转发。
//
// 它复用常驻转发的数据面实现，但生命周期完全由调用它的前台命令控制；这保证桌面
// 客户端关闭后监听端口和 tailcat 客户端会一起释放，不会留下隐藏的长期入口。
type TransientForward struct {
	runner *forwardRunner
	udp    *udpForwardRunner
	once   sync.Once

	transportMu sync.Mutex
	transport   TransportSnapshot
}

// StartTransientForward 在随机回环端口启动一条临时 tailcat TCP 转发。
func StartTransientForward(token string, remotePort int, clientKey key.NodePrivate) (*TransientForward, error) {
	return StartTransientForwardWithOptions(token, remotePort, clientKey, false)
}

// StartTransientForwardWithOptions 启动临时转发，可选在同回环端口同步启用 UDP 转发（RDP 8+）
// 并异步预热 tailcat 隧道以提前完成 NAT 直连打洞。
func StartTransientForwardWithOptions(token string, remotePort int, clientKey key.NodePrivate, enableUDP bool) (*TransientForward, error) {
	if err := ValidateToken(token); err != nil {
		return nil, err
	}
	if remotePort <= 0 || remotePort > 65535 {
		return nil, fmt.Errorf("端口 %d 超出 1-65535", remotePort)
	}
	client := tailcat.NewClient(tailcat.Addr(token))
	client.Logf = logger.Discard
	if !clientKey.IsZero() {
		client.Key = clientKey
	}
	release := func() { _ = client.Close() }
	return startTransientForwardWithClient(token, remotePort, clientKey, client, release, enableUDP, logger.Discard)
}

// startTransientForward 使用可注入拨号器启动临时转发，供生产入口与单元测试复用。
func startTransientForward(listen, token string, remotePort int, clientKey key.NodePrivate, dial func(ctx context.Context) (net.Conn, error)) (*TransientForward, error) {
	return startTransientForwardWithDial(listen, token, remotePort, clientKey, dial, nil)
}

// startTransientForwardWithDial 启动临时转发，并接管共享客户端的释放回调。
func startTransientForwardWithDial(listen, token string, remotePort int, clientKey key.NodePrivate, dial func(ctx context.Context) (net.Conn, error), release func()) (*TransientForward, error) {
	runner := newForwardRunner("desktop", listen, tailcat.Addr(token), uint16(remotePort), logger.Discard, clientKey, dial)
	runner.release = release
	if err := runner.start(); err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	return &TransientForward{runner: runner}, nil
}

// startTransientForwardWithClient 使用真实 tailcat 客户端启动临时转发：
// 1. 若 enableUDP=true，尝试在同一回环端口号上同步绑定 UDP 转发（供 RDP 3389 UDP 使用）；
// 2. 启动后台异步预热（DiscoPing），在用户打开桌面客户端期间提前完成 DERP 与 NAT 直连打洞。
func startTransientForwardWithClient(token string, remotePort int, clientKey key.NodePrivate, client *tailcat.Client, release func(), enableUDP bool, logf func(string, ...any)) (*TransientForward, error) {
	if logf == nil {
		logf = logger.Discard
	}
	maxAttempts := 1
	if enableUDP {
		maxAttempts = 4
	}
	var runner *forwardRunner
	var udpRunner *udpForwardRunner
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		r := newForwardRunner("desktop", "127.0.0.1:0", tailcat.Addr(token), uint16(remotePort), logf, clientKey, sharedClientDial(client, uint16(remotePort)))
		r.sharedClient = client
		if err := r.start(); err != nil {
			lastErr = err
			continue
		}
		if !enableUDP {
			runner = r
			break
		}
		u, uerr := startUDPForwardRunner(r.ln.Addr().String(), sharedClientDialUDP(client, uint16(remotePort)), logf)
		if uerr == nil {
			runner = r
			udpRunner = u
			break
		}
		// 仅在还有重试次数时释放 TCP 重挑端口；最后一次降级为纯 TCP，不阻断连接。
		if attempt+1 < maxAttempts {
			r.stop()
			lastErr = uerr
			continue
		}
		logf("[remote] 远程桌面 UDP 伴随转发监听失败（%v），自动降级为纯 TCP", uerr)
		runner = r
	}
	if runner == nil {
		if release != nil {
			release()
		}
		return nil, lastErr
	}
	runner.release = release
	forward := &TransientForward{
		runner: runner,
		udp:    udpRunner,
		transport: TransportSnapshot{
			Path:       PathUnknown,
			Warming:    true,
			UDPEnabled: udpRunner != nil,
		},
	}
	go forward.warmupLoop(client)
	return forward, nil
}

// warmupLoop 在临时桌面会话启动后立即执行一次 DiscoPing 预热 NAT 直连，并定期刷新链路质量。
func (f *TransientForward) warmupLoop(client *tailcat.Client) {
	probeOnce := func() {
		ctx, cancel := context.WithTimeout(f.runner.ctx, 10*time.Second)
		res, err := probeWithClient(ctx, client)
		cancel()
		if f.runner.ctx.Err() != nil {
			return
		}
		f.transportMu.Lock()
		f.transport.Warming = false
		if err != nil {
			f.transport.Error = err.Error()
			if reg := client.DERPRegion(); reg != nil && f.transport.DERPRegion == "" {
				f.transport.DERPRegion = formatDERPRegion(reg)
			}
		} else {
			f.transport.Path = res.Path
			f.transport.Endpoint = res.Endpoint
			f.transport.DERPRegion = res.DERPRegion
			f.transport.RTTMillis = res.RTTMillis
			f.transport.CheckedAt = res.CheckedAt
			f.transport.Error = ""
		}
		f.transportMu.Unlock()
	}

	probeOnce()
	ticker := time.NewTicker(desktopWarmupRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-f.runner.ctx.Done():
			return
		case <-ticker.C:
			probeOnce()
		}
	}
}

// Transport 返回当前临时桌面转发的最新隧道质量快照（直连/DERP、RTT、是否启用 UDP）。
func (f *TransientForward) Transport() TransportSnapshot {
	f.transportMu.Lock()
	defer f.transportMu.Unlock()
	return f.transport
}

// Address 返回桌面客户端应连接的本地回环地址。
func (f *TransientForward) Address() string {
	return f.runner.ln.Addr().String()
}

// ActiveConnections 返回当前正在通过临时转发传输的本地连接数。
func (f *TransientForward) ActiveConnections() int64 {
	return f.runner.active.Load()
}

// Close 停止监听并释放 tailcat 客户端、伴随 UDP 转发及活动连接。
func (f *TransientForward) Close() error {
	f.once.Do(func() {
		if f.udp != nil {
			f.udp.stop()
		}
		f.runner.stop()
	})
	return nil
}
