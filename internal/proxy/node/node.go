// Package node 定义 proxyd 各层共享的代理节点领域模型与稳定身份规则。
package node

import (
	"strings"
	"sync/atomic"
)

// Display 是节点面向控制台的只读展示行。
//
// 它与权威运行态字段（Node.Alive/Delay/FailReason）分离：写方先改权威字段，再原子地发布
// 一整行展示值；读方只做一次原子读。因此概览在健康检测进行中读取时，既不会读到「上一轮的
// 存活状态 + 这一轮的延迟」这类半成品组合，也不与 worker 的写回产生数据竞争。
type Display struct {
	// Alive 表示最近一次普通节点或完整链路健康检测是否成功。
	Alive bool
	// Delay 是最近一次健康检测的毫秒延迟；未知或失败时为 0。
	Delay uint16
	// FailReason 是最近一次健康检测失败的原因（Alive=true 时为空）。
	FailReason string
	// Testing 表示该节点本轮健康检测尚未产出结果；控制台据此把该节点的延迟列显示为
	// 「测速中…」，而不是容易被误读的上一轮结果。
	Testing bool
}

// Node 表示从订阅或手动配置解析出的一个代理服务器实体。
//
// 并发契约：节点一旦进入运行态（App.nodes、assignments），其 Name/Subscription/Mapping
// 就视为只读；需要改名或改写 Mapping 的路径（订阅合并、草稿）必须先复制（Clone /
// CloneWithMapping），绝不能原地改写——概览、端口表与配置生成会并发读取这些字段，
// 其中 Mapping 的并发写入还会直接触发运行时 fatal error 终止进程。
// 运行态结果字段（Alive/Delay/FailReason）由写方写完后再发布展示行（见 Display），
// 读取方一律经 Display()，不要直接读字段。
type Node struct {
	// Name 是跨订阅唯一的展示名；名称冲突时由 subscribe.Merge 添加订阅后缀。
	Name string
	// Subscription 是节点来源订阅名；手动节点使用固定来源名 manual。
	Subscription string
	// Mapping 是 mihomo 出站代理原始映射，内部 name 始终与 Name 同步。
	Mapping map[string]any
	// Alive 表示最近一次普通节点或完整链路健康检测是否成功。
	Alive bool
	// Delay 是最近一次健康检测的毫秒延迟；未知或失败时为 0。
	Delay uint16
	// FailReason 是最近一次健康检测失败的原因（Alive=true 时为空）。
	FailReason string
	// display 是上面三个结果字段的只读投影，供控制台逐节点展示测速进度（见 Display）。
	// 该字段带 noCopy，Node 因此不可整结构赋值，复制请用 Clone。
	display atomic.Pointer[Display]
}

// Key 返回用于端口快照与通用去重基础的稳定节点身份。
//
// 参数：无；方法读取当前 Node.Mapping。
//
// 返回值：string，由协议、地址、端口、主凭据组成；链式节点额外包含
// dialer-proxy 名称，避免同一服务器经不同上游拨号时被错误去重。凭据提取优先级为
// uuid → password → auth-key → private-key（后两者覆盖 tailscale/wireguard/ssh 等
// 隧道类出站）。tailscale 类型无常规 server/port 时以 control-url 顶替 server 位置
// 参与身份。普通节点保持历史 Key 格式不变，从而兼容已经持久化的端口映射快照。
// 合并节点时应调用 DedupKey；它会为拥有独立 tsnet 状态的 Tailscale 出站补充名称维度。
//
// 错误情况：无；缺失或未知类型字段按空字符串参与身份计算。
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
	if cred == "" {
		cred = get("auth-key")
	}
	if cred == "" {
		cred = get("private-key")
	}
	server := get("server")
	// tailscale 出站没有常规 server/port，控制面地址才是节点身份的稳定部分。
	if server == "" && get("type") == TunnelTypeTailscale {
		server = get("control-url")
	}
	key := get("type") + "|" + server + "|" + get("port") + "|" + cred
	if dialer := n.DialerProxy(); dialer != "" {
		key += "|dialer=" + dialer
	}
	return key
}

// DedupKey 返回节点合并阶段使用的领域身份，不改变端口快照所用的历史 Key 格式。
//
// 参数说明：无；方法读取 Node.Name 与 Node.Mapping，不修改节点内容。
//
// 返回值说明：普通节点直接返回 Key；Tailscale 节点额外包含出站名称，因为 mihomo
// 会按名称为每个出站分配独立 tsnet 状态目录与设备身份。同一控制面下两个没有
// auth-key 的审批节点因此仍是两个聚合实体，不能按空凭据合并。
//
// 错误情况：无；nil 节点返回空字符串。名称为空的异常 Tailscale 节点仍生成稳定
// 后缀，后续配置解析会负责报告缺少名称，而不会在此阶段 panic。
func (n *Node) DedupKey() string {
	if n == nil {
		return ""
	}
	key := n.Key()
	if !n.IsTailscale() {
		return key
	}
	return key + "|identity=" + strings.TrimSpace(n.Name)
}

// DialerProxy 返回该节点配置的链式拨号目标。
//
// 参数：无；方法读取 Mapping 中 mihomo 标准字段 `dialer-proxy`。
//
// 返回值：string，去除首尾空白后的代理或代理组名称；未配置时为空字符串。
//
// 错误情况：无；nil Mapping 或非字符串字段按未配置处理，真正的配置类型错误会在
// mihomo 适配器解析阶段进入节点 FailReason。
func (n *Node) DialerProxy() string {
	if n == nil || n.Mapping == nil {
		return ""
	}
	value, _ := n.Mapping["dialer-proxy"].(string)
	return strings.TrimSpace(value)
}

// Display 返回节点当前发布的展示行。
//
// 参数：无；只读取一次原子指针，绝不触碰权威运行态字段。
//
// 返回值：Display，包含存活状态、延迟、失败原因与本轮是否仍在测速；尚未发布过展示行的
// 节点返回零值行（等价于「还没测过」）。生产路径上节点在诞生时就发布了展示行
// （见 subscribe.newNode、LoadSnapshot、Clone），因此这里不会有可观察的中间态。
//
// 错误情况：无；nil 接收者返回零值行。概览、端口表与订阅统计必须经由本方法读取，
// 不要直接读 Alive/Delay/FailReason——那些字段可能正被健康检测的 worker 写回。
func (n *Node) Display() Display {
	if n == nil {
		return Display{}
	}
	if d := n.display.Load(); d != nil {
		return *d
	}
	return Display{}
}

// MarkTesting 把节点当前的展示值打包成「测速中」行发布，标记本轮健康检测开始。
//
// 参数：无。
//
// 返回值：无。
//
// 错误情况：无。必须在任何结果写入之前调用：行里保存的是本轮开始前的存活状态与延迟，
// 概览据此在结果落定前持续返回这组稳定值，而不是探测过程中的中间状态。
func (n *Node) MarkTesting() {
	if n == nil {
		return
	}
	n.display.Store(&Display{
		Alive:      n.Alive,
		Delay:      n.Delay,
		FailReason: n.FailReason,
		Testing:    true,
	})
}

// PublishResult 按节点当前的权威字段发布一行最终结果（Testing 为 false）。
//
// 参数：无。
//
// 返回值：无。
//
// 错误情况：无。调用方必须已经写完 Alive/Delay/FailReason；原子存储带 release 语义，
// 读到该行的读者一定能看到这些写入。
func (n *Node) PublishResult() {
	if n == nil {
		return
	}
	n.display.Store(&Display{Alive: n.Alive, Delay: n.Delay, FailReason: n.FailReason})
}

// PublishDisplay 发布指定的展示行，不改动权威字段。
//
// 参数：
//   - d: Display，要发布的展示值。
//
// 返回值：无。
//
// 错误情况：无。用于把一份已经得到的结果先行展示到别的节点上（例如单订阅测速在克隆节点上
// 得到结果后，增量更新已发布节点对应行的延迟列），权威字段仍等提交阶段统一回填。
func (n *Node) PublishDisplay(d Display) {
	if n == nil {
		return
	}
	value := d
	n.display.Store(&value)
}

// Clone 复制节点的字段与展示行，Mapping 保持只读共享。
//
// 参数：无。
//
// 返回值：*Node，字段独立的新节点；nil 接收者返回 nil。
//
// 错误情况：无。Node 含原子字段，不能整结构赋值，复制一律走本方法；需要独立修改 Mapping
// 的调用方（如订阅草稿）请再自行复制一层映射。
func (n *Node) Clone() *Node {
	if n == nil {
		return nil
	}
	cloned := &Node{
		Name:         n.Name,
		Subscription: n.Subscription,
		Mapping:      n.Mapping,
		Alive:        n.Alive,
		Delay:        n.Delay,
		FailReason:   n.FailReason,
	}
	if d := n.display.Load(); d != nil {
		value := *d
		cloned.display.Store(&value)
	}
	return cloned
}

// CloneWithMapping 在 Clone 的基础上再复制 Mapping 顶层键值。
//
// 参数：无。
//
// 返回值：*Node，字段、展示行与 Mapping 顶层键值都独立的新节点；nil 接收者返回 nil。
//
// 错误情况：无。供合并、草稿这类需要改写 name/dialer-proxy 的调用方使用：那些键都在
// Mapping 顶层，因此只复制顶层，嵌套协议选项保持只读共享，避免无意义的深层复制。
func (n *Node) CloneWithMapping() *Node {
	cloned := n.Clone()
	if cloned == nil || cloned.Mapping == nil {
		return cloned
	}
	mapping := make(map[string]any, len(cloned.Mapping)+1)
	for key, value := range cloned.Mapping {
		mapping[key] = value
	}
	cloned.Mapping = mapping
	return cloned
}

// itoa 把非负整数转换为十进制文本，供稳定身份生成使用。
//
// 参数：
//   - i: int，节点端口等非负整数。
//
// 返回值：string，十进制表示；0 返回 "0"。
//
// 错误情况：该轻量实现不支持负数；调用点只传入协议端口，因此不会出现负值。
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
