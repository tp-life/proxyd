// Package node defines the shared proxy-node model used across proxyd.
package node

// Node is one proxy server parsed from a subscription.
type Node struct {
	// Name is the display name, unique across all subscriptions
	// (prefixed with the subscription name on collision or always, see subscribe.Merge).
	Name string
	// Subscription is the name of the subscription this node came from.
	Subscription string
	// Mapping is the mihomo outbound proxy map (keys per mihomo docs: type/server/port/...).
	// "name" inside Mapping is always kept in sync with Name.
	Mapping map[string]any
	// Alive reports the last health-check result.
	Alive bool
	// Delay is the last measured latency in milliseconds (0 when unknown/dead).
	Delay uint16
	// FailReason 是最近一次健康检测失败的原因（Alive=true 时为空）。
	FailReason string
}

// Key returns a stable identity used for dedup and port-mapping persistence:
// protocol + address + credentials, independent of display name.
func (n *Node) Key() string {
	m := n.Mapping
	get := func(k string) string {
		if v, ok := m[k]; ok {
			switch s := v.(type) {
			case string:
				return s
			case int:
				return itoa(s)
			case float64:
				return itoa(int(s))
			}
		}
		return ""
	}
	cred := get("uuid")
	if cred == "" {
		cred = get("password")
	}
	return get("type") + "|" + get("server") + "|" + get("port") + "|" + cred
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [12]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
