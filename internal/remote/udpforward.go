package remote

// 本文件实现客户端侧的本地 UDP → tailcat 隧道 UDP 转发器，以及服务端 UDP 流双向拷贝。
//
// 现代 Windows RDP（RDP 8+ / RemoteFX / AVC444）在与 TCP 3389 同端口上协商
// UDP 3389（MS-RDPEUDP）来传输动态视频帧并避免 TCP 队头阻塞。本转发器与本地 TCP
// 临时转发绑定同一回环端口号，让系统 RDP 客户端无缝启用 UDP 传输。

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tailscale/tailcat"
)

const (
	udpRelayBufferSize = 64 << 10
	udpSweepInterval   = 30 * time.Second
)

// udpForwardRunner 监听一个本机回环 UDP 端口，按客户端源地址复用/创建对应的远端 UDP 流。
type udpForwardRunner struct {
	localConn *net.UDPConn
	dial      func(ctx context.Context) (net.Conn, error)
	logf      func(format string, args ...any)
	idle      time.Duration

	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once

	mu      sync.Mutex
	flows   map[netip.AddrPort]*udpClientFlow
	stopped bool
}

type udpClientFlow struct {
	upstream   net.Conn
	lastActive atomic.Int64 // UnixNano
	closeOnce  sync.Once
}

func (f *udpClientFlow) touch() {
	f.lastActive.Store(time.Now().UnixNano())
}

func (f *udpClientFlow) close() {
	f.closeOnce.Do(func() {
		_ = f.upstream.Close()
	})
}

// startUDPForwardRunner 在指定回环地址（通常与 TCP 转发同端口）启动 UDP 转发器。
func startUDPForwardRunner(listenAddr string, dial func(ctx context.Context) (net.Conn, error), logf func(format string, args ...any)) (*udpForwardRunner, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ap, err := netip.ParseAddrPort(listenAddr)
	if err != nil {
		return nil, fmt.Errorf("解析 UDP 监听地址 %q 失败: %w", listenAddr, err)
	}
	conn, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(ap))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &udpForwardRunner{
		localConn: conn,
		dial:      dial,
		logf:      logf,
		idle:      tailcat.DefaultUDPIdleTimeout,
		ctx:       ctx,
		cancel:    cancel,
		flows:     map[netip.AddrPort]*udpClientFlow{},
	}
	go r.readLoop()
	go r.sweepLoop()
	return r, nil
}

// sharedClientDialUDP 返回经共享 tailcat 客户端拨远端 UDP 端口的 dialer。
func sharedClientDialUDP(client *tailcat.Client, port uint16) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()
		return client.DialUDPPort(ctx, port)
	}
}

func (r *udpForwardRunner) readLoop() {
	buf := make([]byte, udpRelayBufferSize)
	for {
		n, src, err := r.localConn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		flow, err := r.getOrCreateFlow(src)
		if err != nil {
			if r.ctx.Err() == nil {
				r.logf("[remote] UDP 转发拨号失败: %v", err)
			}
			continue
		}
		flow.touch()
		if _, err := flow.upstream.Write(buf[:n]); err != nil {
			r.removeFlow(src, flow)
		}
	}
}

func (r *udpForwardRunner) getOrCreateFlow(src netip.AddrPort) (*udpClientFlow, error) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil, net.ErrClosed
	}
	if existing, ok := r.flows[src]; ok {
		r.mu.Unlock()
		return existing, nil
	}
	r.mu.Unlock()

	upstream, err := r.dial(r.ctx)
	if err != nil {
		return nil, err
	}
	flow := &udpClientFlow{upstream: upstream}
	flow.touch()

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		flow.close()
		return nil, net.ErrClosed
	}
	if existing, ok := r.flows[src]; ok {
		r.mu.Unlock()
		flow.close()
		return existing, nil
	}
	r.flows[src] = flow
	r.mu.Unlock()

	go r.pumpUpstream(src, flow)
	return flow, nil
}

func (r *udpForwardRunner) pumpUpstream(src netip.AddrPort, flow *udpClientFlow) {
	defer r.removeFlow(src, flow)
	buf := make([]byte, udpRelayBufferSize)
	for {
		n, err := flow.upstream.Read(buf)
		if n > 0 {
			flow.touch()
			if _, werr := r.localConn.WriteToUDPAddrPort(buf[:n], src); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *udpForwardRunner) removeFlow(src netip.AddrPort, flow *udpClientFlow) {
	r.mu.Lock()
	if cur, ok := r.flows[src]; ok && cur == flow {
		delete(r.flows, src)
	}
	r.mu.Unlock()
	flow.close()
}

func (r *udpForwardRunner) sweepLoop() {
	ticker := time.NewTicker(udpSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case now := <-ticker.C:
			cutoff := now.Add(-r.idle).UnixNano()
			var expired []*udpClientFlow
			r.mu.Lock()
			for src, flow := range r.flows {
				if flow.lastActive.Load() < cutoff {
					expired = append(expired, flow)
					delete(r.flows, src)
				}
			}
			r.mu.Unlock()
			for _, flow := range expired {
				flow.close()
			}
		}
	}
}

func (r *udpForwardRunner) stop() {
	r.stopOnce.Do(func() {
		r.cancel()
		r.mu.Lock()
		r.stopped = true
		flows := make([]*udpClientFlow, 0, len(r.flows))
		for _, flow := range r.flows {
			flows = append(flows, flow)
		}
		clear(r.flows)
		r.mu.Unlock()
		_ = r.localConn.Close()
		for _, flow := range flows {
			flow.close()
		}
	})
}

// relayUDP 在两条已连接的数据报套接字之间双向转发 UDP 报文（保留数据报边界）。
func relayUDP(a, b net.Conn) {
	done := make(chan struct{}, 2)
	pump := func(dst, src net.Conn) {
		buf := make([]byte, udpRelayBufferSize)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go pump(a, b)
	go pump(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}
