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

// PeerObservation 是服务端针对一个已知入站客户端的只读连接质量快照。
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
	m.mu.Lock()
	clientKey := m.clientKeyLocked()
	m.mu.Unlock()
	return Probe(ctx, token, clientKey)
}

// peerObservations 把 tailcat Server.Status 转换为项目稳定的入站客户端观测模型。
//
// 参数说明：
//   - status: *ipnstate.Status，tailcat 服务端即时状态；可为 nil。
//   - allowed: []config.RemoteAllowEntry，用于补充客户端别名。
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
		// tailcat 无控制面，PeerStatus.Online 通常没有意义；近期握手、引擎 Active
		// 或仍有业务连接任一成立即可确认在线，过旧握手则保守标为离线。
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
