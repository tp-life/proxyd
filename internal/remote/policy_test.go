package remote

// 本文件验证 remote 领域层新增的授权、审计与观测规则。
// 测试只使用内存状态，不建立真实 DERP 连接，确保安全边界能快速、稳定地回归。

import (
	"io"
	"net"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"

	"proxyd/internal/config"
)

// attributedTestConn 为内存连接覆盖远端地址，用来模拟 tailcat netstack 的公钥地址映射。
type attributedTestConn struct {
	net.Conn
	remoteAddr net.Addr
}

// RemoteAddr 返回测试指定的隧道地址，使授权包装器能够执行真实的公钥归因流程。
//
// 参数说明：无。
//
// 返回值说明：net.Addr，构造测试连接时注入的客户端地址。
//
// 错误情况：无；测试必须在构造时提供地址。
func (c *attributedTestConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

// tunnelTCPAddr 为测试公钥构造 tailcat 实际会分配的隧道 TCP 地址。
//
// 参数说明：
//   - publicKey: key.NodePublic，待归因的客户端公钥。
//
// 返回值说明：net.Addr，可直接传给 decideConnection 模拟入站连接。
//
// 错误情况：无；tcAddrForKey 对合法公钥总能生成 IPv6 地址。
func tunnelTCPAddr(publicKey key.NodePublic) net.Addr {
	address := tcAddrForKey(publicKey)
	return &net.TCPAddr{IP: net.IP(address.AsSlice()), Port: 49152}
}

// TestDecideConnectionPolicy 验证开放模式、TTL、端口范围、临时身份与受限空列表。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：不访问网络或磁盘，所有失败都代表授权领域规则发生回归。
func TestDecideConnectionPolicy(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	allowedPrivate := key.NewNode()
	allowedKey := allowedPrivate.Public().String()
	allowedAddr := tunnelTCPAddr(allowedPrivate.Public())
	unknownAddr := tunnelTCPAddr(key.NewNode().Public())

	if decision := decideConnection(config.RemoteConfig{}, unknownAddr, 22, now); !decision.Allowed {
		t.Fatalf("开放模式应允许未知客户端，got %+v", decision)
	}

	restricted := config.RemoteConfig{
		AllowRestricted: true,
		Allow: []config.RemoteAllowEntry{{
			Name:      "维护者",
			Key:       allowedKey,
			ExpiresAt: timePointer(now.Add(time.Minute)),
			Ports:     []int{22, 8080},
		}},
	}
	if decision := decideConnection(restricted, allowedAddr, 22, now); !decision.Allowed || decision.Name != "维护者" {
		t.Fatalf("有效授权应放行并保留别名，got %+v", decision)
	}
	if decision := decideConnection(restricted, allowedAddr, 443, now); decision.Allowed || decision.Reason == "" {
		t.Fatalf("未授权端口应拒绝并给出原因，got %+v", decision)
	}
	if decision := decideConnection(restricted, allowedAddr, 22, now.Add(time.Minute)); decision.Allowed || decision.Reason != "客户端授权已过期" {
		t.Fatalf("到达过期边界应立即拒绝，got %+v", decision)
	}
	if decision := decideConnection(restricted, unknownAddr, 22, now); decision.Allowed {
		t.Fatalf("受限模式应拒绝未知客户端，got %+v", decision)
	}
	if decision := decideConnection(config.RemoteConfig{AllowRestricted: true}, unknownAddr, 22, now); decision.Allowed {
		t.Fatalf("清扫后的受限空列表不得退化为开放模式，got %+v", decision)
	}

	tempPrivate := key.NewNode()
	tempConfig := config.RemoteConfig{AllowRestricted: true, TempKey: tempPrivate.Public().String()}
	if decision := decideConnection(tempConfig, tunnelTCPAddr(tempPrivate.Public()), 65535, now); !decision.Allowed || decision.Name != "临时身份" {
		t.Fatalf("临时身份应按应急策略访问全部端口，got %+v", decision)
	}
}

// timePointer 返回独立的时间指针，便于测试构造可选过期时间。
//
// 参数说明：
//   - value: time.Time，需要保存的时间值。
//
// 返回值说明：*time.Time，指向逃逸后的独立副本。
//
// 错误情况：无。
func timePointer(value time.Time) *time.Time {
	return &value
}

// TestAuditLogRingOrder 验证固定容量审计环覆盖最旧记录且 tail 保持时间顺序。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：不访问外部资源；失败表示审计查询可能漏掉最新事件或顺序错误。
func TestAuditLogRingOrder(t *testing.T) {
	log := newAuditLog(3)
	for port := 20; port <= 23; port++ {
		log.Append(AuditEntry{TargetPort: port, Action: AuditActionConnected})
	}
	all := log.Tail(10)
	if len(all) != 3 || all[0].TargetPort != 21 || all[2].TargetPort != 23 {
		t.Fatalf("容量覆盖后的顺序错误: %+v", all)
	}
	recent := log.Tail(2)
	if len(recent) != 2 || recent[0].TargetPort != 22 || recent[1].TargetPort != 23 {
		t.Fatalf("tail 应返回最近两条且保持旧到新顺序: %+v", recent)
	}
	if empty := log.Tail(0); len(empty) != 0 {
		t.Fatalf("非正 tail 应返回空集合: %+v", empty)
	}
}

// TestGuardConnectionAudit 验证真实连接包装器会强制端口授权，并完整记录连接生命周期和字节数。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：仅使用 net.Pipe 内存连接；失败表示授权强制点或审计计量发生回归。
func TestGuardConnectionAudit(t *testing.T) {
	clientPrivate := key.NewNode()
	clientKey := clientPrivate.Public().String()
	manager := NewManager(t.TempDir(), nil)
	manager.cfg = config.RemoteConfig{
		AllowRestricted: true,
		Allow: []config.RemoteAllowEntry{{
			Name:  "审计客户端",
			Key:   clientKey,
			Ports: []int{22},
		}},
	}

	allowedServer, allowedClient := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.guardConnection(22, func(connection net.Conn) {
			payload := make([]byte, 2)
			_, _ = io.ReadFull(connection, payload)
			_, _ = connection.Write([]byte("ack"))
		})(&attributedTestConn{Conn: allowedServer, remoteAddr: tunnelTCPAddr(clientPrivate.Public())})
	}()
	if _, err := allowedClient.Write([]byte("hi")); err != nil {
		t.Fatalf("写入允许连接失败: %v", err)
	}
	response := make([]byte, 3)
	if _, err := io.ReadFull(allowedClient, response); err != nil {
		t.Fatalf("读取允许连接响应失败: %v", err)
	}
	_ = allowedClient.Close()
	<-done

	allowedAudit := manager.Audit(10)
	if len(allowedAudit) != 2 || allowedAudit[0].Action != AuditActionConnected || allowedAudit[1].Action != AuditActionDisconnected {
		t.Fatalf("允许连接应记录建立与断开事件: %+v", allowedAudit)
	}
	if allowedAudit[1].RxBytes != 2 || allowedAudit[1].TxBytes != 3 || allowedAudit[1].ClientName != "审计客户端" {
		t.Fatalf("断开事件的身份或流量计量错误: %+v", allowedAudit[1])
	}

	rejectedServer, rejectedClient := net.Pipe()
	manager.guardConnection(443, func(net.Conn) {
		t.Fatal("未授权端口不得进入下游处理器")
	})(&attributedTestConn{Conn: rejectedServer, remoteAddr: tunnelTCPAddr(clientPrivate.Public())})
	_ = rejectedClient.Close()
	rejectedAudit := manager.Audit(1)
	if len(rejectedAudit) != 1 || rejectedAudit[0].Action != AuditActionRejected || rejectedAudit[0].TargetPort != 443 || rejectedAudit[0].Reason == "" {
		t.Fatalf("未授权端口应生成包含原因的拒绝事件: %+v", rejectedAudit)
	}
}

// TestPeerObservations 验证服务端状态被稳定映射为直连/DERP、流量与在线快照。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：不调用 tailcat 网络；失败表示上层展示合约发生回归。
func TestPeerObservations(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	directKey := key.NewNode().Public()
	derpKey := key.NewNode().Public()
	status := &ipnstate.Status{Peer: map[key.NodePublic]*ipnstate.PeerStatus{
		directKey: {
			CurAddr:       "198.51.100.8:41641",
			RxBytes:       100,
			TxBytes:       200,
			LastHandshake: now.Add(-time.Minute),
		},
		derpKey: {
			Relay:         "hkg",
			RxBytes:       300,
			TxBytes:       400,
			LastHandshake: now.Add(-10 * time.Minute),
		},
	}}
	observations := peerObservations(
		status,
		[]config.RemoteAllowEntry{{Name: "直连设备", Key: directKey.String()}},
		derpKey.String(),
		map[string]int64{derpKey.String(): 1},
		now,
	)
	byKey := make(map[string]PeerObservation, len(observations))
	for _, observation := range observations {
		byKey[observation.Key] = observation
	}
	direct := byKey[directKey.String()]
	if !direct.Online || direct.Path != PathDirect || direct.Endpoint == "" || direct.Name != "直连设备" || direct.RxBytes != 100 || direct.TxBytes != 200 {
		t.Fatalf("直连状态映射错误: %+v", direct)
	}
	derp := byKey[derpKey.String()]
	if !derp.Online || derp.Path != PathDERP || derp.DERPRegion != "hkg" || derp.Name != "临时身份" || derp.Active != 1 {
		t.Fatalf("DERP 状态映射错误: %+v", derp)
	}
}

// TestDurationMillis 验证 RTT 毫秒转换不会把成功的亚毫秒探测展示成零。
//
// 参数说明：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：无外部错误；失败表示探测展示精度回归。
func TestDurationMillis(t *testing.T) {
	if got := durationMillis(400 * time.Microsecond); got != 1 {
		t.Fatalf("亚毫秒成功探测应至少显示 1ms，got %d", got)
	}
	if got := durationMillis(1500 * time.Microsecond); got != 2 {
		t.Fatalf("RTT 应四舍五入，got %d", got)
	}
	if got := durationMillis(0); got != 0 {
		t.Fatalf("非正 RTT 应保持 0，got %d", got)
	}
}
