package remote

// 本文件是 tailcat 观测能力的稳定适配层。
// 上层只依赖 ProbeResult/PeerObservation，不接触 tailcat 与 Tailscale 的不稳定状态结构。

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"

	"proxyd/internal/config"
)

const (
	// PathDirect 表示 disco 探测或服务端状态确认正在使用点对点 UDP 路径。
	PathDirect = "direct"
	// PathDERP 表示数据当前通过 DERP 中继传输。
	PathDERP = "derp"
	// PathUnknown 表示尚无足够握手信息判断连接路径。
	PathUnknown = "unknown"
)

// ProbeResult 是对一个已保存远端执行隧道内探测后的稳定结果。
type ProbeResult struct {
	Online     bool      `json:"online"`
	RTTMillis  int64     `json:"rtt_ms"`
	Path       string    `json:"path"`
	Endpoint   string    `json:"endpoint,omitempty"`
	DERPRegion string    `json:"derp_region,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
}

// PeerObservation 是 tailcat 服务端上报的一个入站客户端连接质量快照。
//
// 数据来自 tailcat v0.7.0 起 Status().Peer 填充的 ipnstate.PeerStatus：客户端完成
// 一次握手后即出现，服务端不会主动移除，直到隧道重建或进程退出，因此列表可能包含
// 已经离线的历史客户端。不在白名单/临时身份中的公钥同样保留（Name 为空），
// 用于开放模式下发现陌生客户端；展示层按需过滤。
type PeerObservation struct {
	Key           string    `json:"key"`
	Name          string    `json:"name,omitempty"`
	Online        bool      `json:"online"`
	Path          string    `json:"path"`
	Endpoint      string    `json:"endpoint,omitempty"`
	DERPRegion    string    `json:"derp_region,omitempty"`
	RTTMillis     int64     `json:"rtt_ms,omitempty"`
	RxBytes       int64     `json:"rx_bytes"`
	TxBytes       int64     `json:"tx_bytes"`
	Active        int64     `json:"active"`
	LastHandshake time.Time `json:"last_handshake,omitempty"`
}

// Probe 使用 tailcat 的 disco ping 探测远端在线状态、RTT 与当前传输路径。
//
// 参数说明：
//   - ctx: context.Context，控制 DERP 建连、路径发现与 ping 等待的取消/超时。
//   - token: string，目标服务器完整 tailcat 连接 token。
//   - clientKey: key.NodePrivate，可选稳定客户端身份；零值时由 tailcat 创建临时身份。
//
// 返回值说明：ProbeResult，成功时 Online=true，并包含毫秒 RTT 及 direct/derp 路径。
//
// 错误情况：token 无效、DERP 不可达、白名单拒绝或 ping 超时时返回错误，不伪造离线结果。
func Probe(ctx context.Context, token string, clientKey key.NodePrivate) (ProbeResult, error) {
	if err := ValidateToken(token); err != nil {
		return ProbeResult{}, err
	}
	client := tailcat.NewClient(tailcat.ConnBlob(token))
	client.Logf = func(string, ...any) {}
	if !clientKey.IsZero() {
		client.Key = clientKey
	}
	defer client.Close()
	return probeWithClient(ctx, client)
}

// probeWithClient 在已有 tailcat 客户端上执行 disco ping。
//
// 参数说明：
//   - ctx: context.Context，控制 DERP 建连、路径发现与 ping 等待的取消/超时。
//   - client: *tailcat.Client，调用方拥有的客户端；本函数不会关闭它。
//
// 返回值说明：ProbeResult，字段语义与 Probe 一致。
//
// 错误情况：ping 超时、白名单拒绝或客户端不可用时返回错误。
func probeWithClient(ctx context.Context, client *tailcat.Client) (ProbeResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}

	ping, err := client.DiscoPing(ctx)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("探测远端失败: %w", err)
	}
	result := ProbeResult{
		Online:    true,
		RTTMillis: durationMillis(time.Duration(ping.LatencySeconds * float64(time.Second))),
		Path:      PathUnknown,
		CheckedAt: time.Now().UTC(),
	}
	if ping.Endpoint != "" {
		result.Path = PathDirect
		result.Endpoint = ping.Endpoint
	} else if ping.DERPRegionID != 0 || ping.DERPRegionCode != "" {
		result.Path = PathDERP
		result.DERPRegion = strings.TrimSpace(ping.DERPRegionCode)
		if result.DERPRegion == "" {
			result.DERPRegion = fmt.Sprintf("%d", ping.DERPRegionID)
		}
	}
	return result, nil
}

// ProbeRemote 使用 Manager 持久化的客户端身份探测一个完整 token。
//
// 参数说明：
//   - ctx: context.Context，控制探测生命周期。
//   - token: string，已经由应用层解析出的完整远端 token。
//
// 返回值说明：ProbeResult，字段语义与 Probe 一致。
//
// 错误情况：客户端身份加载失败时降级为临时身份；实际探测错误原样返回给应用层。
func (m *Manager) ProbeRemote(ctx context.Context, token string) (ProbeResult, error) {
	if err := ValidateToken(token); err != nil {
		return ProbeResult{}, err
	}
	m.mu.Lock()
	if m.cfg.Disabled {
		m.mu.Unlock()
		return ProbeResult{}, fmt.Errorf("远程访问模块已禁用")
	}
	clientKey := m.clientKeyLocked()
	m.mu.Unlock()

	// 复用 Manager 共享客户端：探测不再创建第二个同 node key 的 magicsock，
	// 避免把正在使用的转发/桌面会话握手顶掉。
	client, release := m.clients.acquire(token, clientKey)
	defer release()
	return probeWithClient(ctx, client)
}

// peerObservations 把 tailcat Server.Status 转换为项目稳定的入站客户端观测模型。
//
// 参数说明：
//   - status: *ipnstate.Status，tailcat 服务端即时状态；可为 nil。tailcat v0.7.0 起
//     Status().Peer 才会被填充；更早版本该集合恒为空，本函数返回空切片。
//   - allowed: []config.RemoteAllowEntry，用于补充客户端别名；未命中的公钥保留记录但 Name 为空。
//   - tempKey: string，临时身份公钥，命中时使用固定可读名称。
//   - active: map[string]int64，当前业务连接计数快照。
//   - now: time.Time，判断近期握手在线状态的统一时钟。
//
// 返回值说明：[]PeerObservation，按 tailcat 状态遍历得到的独立切片。
//
// 错误情况：无；缺少状态或字段时返回空集合/unknown，不向上暴露 tailcat 内部异常。
func peerObservations(status *ipnstate.Status, allowed []config.RemoteAllowEntry, tempKey string, active map[string]int64, now time.Time) []PeerObservation {
	if status == nil || len(status.Peer) == 0 {
		return []PeerObservation{}
	}
	names := make(map[string]string, len(allowed)+1)
	for _, entry := range allowed {
		names[strings.TrimSpace(entry.Key)] = strings.TrimSpace(entry.Name)
	}
	if strings.TrimSpace(tempKey) != "" {
		names[strings.TrimSpace(tempKey)] = "临时身份"
	}

	result := make([]PeerObservation, 0, len(status.Peer))
	for publicKey, peer := range status.Peer {
		if peer == nil {
			continue
		}
		keyText := publicKey.String()
		// Name 为空表示公钥未登记（开放模式下的陌生客户端）：保留原始 nodekey，
		// Web 面板只渲染命中白名单/临时身份的条目，CLI 则直接显示公钥。
		observation := PeerObservation{
			Key:           keyText,
			Name:          names[keyText],
			Path:          PathUnknown,
			RxBytes:       peer.RxBytes,
			TxBytes:       peer.TxBytes,
			Active:        active[keyText],
			LastHandshake: peer.LastHandshake,
		}
		switch {
		case peer.CurAddr != "":
			observation.Path = PathDirect
			observation.Endpoint = peer.CurAddr
		case peer.Relay != "":
			observation.Path = PathDERP
			observation.DERPRegion = peer.Relay
		}
		// tailcat 无控制面，PeerStatus.Online 恒为 false；以近期握手、magicsock 的
		// 活跃标记或仍有业务连接任一成立近似在线（这些字段同样自 v0.7.0 起才有值），
		// 过旧握手则保守标为离线。
		observation.Online = observation.Active > 0 || peer.Active ||
			(!peer.LastHandshake.IsZero() && now.Sub(peer.LastHandshake) <= 2*time.Minute)
		result = append(result, observation)
	}
	// tailcat 的 Peer 是 map；这里按别名、公钥排序，避免 CLI 与 Web 每次刷新时顺序跳动，
	// 同时让 API 响应和测试结果保持确定性。
	sort.Slice(result, func(i, j int) bool {
		left := strings.ToLower(strings.TrimSpace(result[i].Name))
		right := strings.ToLower(strings.TrimSpace(result[j].Name))
		if left == right {
			return result[i].Key < result[j].Key
		}
		if left == "" {
			return false
		}
		if right == "" {
			return true
		}
		return left < right
	})
	return result
}

// durationMillis 把探测耗时转换为至少 1ms 的整数，避免成功的亚毫秒探测显示为零。
//
// 参数说明：
//   - duration: time.Duration，探测返回的往返耗时。
//
// 返回值说明：int64，四舍五入后的毫秒值；非正值返回 0。
//
// 错误情况：无；极大值由 int64 毫秒范围自然限制。
func durationMillis(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return max(1, int64(math.Round(float64(duration)/float64(time.Millisecond))))
}
