package main

// 「远程连接」周边模块的 CLI：status/on/off/token/serve/remotes/forwards
// 作为运行中实例的 API 客户端实现；pipe 与 ssh 是纯客户端命令，不依赖守护进程。

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"tailscale.com/types/key"

	"proxyd/internal/config"
	"proxyd/internal/remote"
)

// allowEntryJSON 对应客户端授权 API 形态，包含身份、可选 TTL 与端口最小权限。
type allowEntryJSON struct {
	Name      string     `json:"name,omitempty"`
	Key       string     `json:"key"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Ports     []int      `json:"ports,omitempty"`
}

// remotePeerObservationJSON 对应服务端已知入站客户端的只读连接质量快照。
type remotePeerObservationJSON struct {
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

// remotePingJSON 对应一次已保存远端的 disco ping 结果。
type remotePingJSON struct {
	Online     bool      `json:"online"`
	RTTMillis  int64     `json:"rtt_ms"`
	Path       string    `json:"path"`
	Endpoint   string    `json:"endpoint,omitempty"`
	DERPRegion string    `json:"derp_region,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
}

// remoteStatusJSON 对应 GET /api/remote 的响应（token 为打码摘要）。
type remoteStatusJSON struct {
	Enabled         bool                        `json:"enabled"`
	Running         bool                        `json:"running"`
	Error           string                      `json:"error,omitempty"`
	Token           string                      `json:"token,omitempty"`
	ClientKey       string                      `json:"client_key,omitempty"`
	Region          string                      `json:"region,omitempty"`
	Serve           []int                       `json:"serve"`
	Allow           []allowEntryJSON            `json:"allow"`
	AllowRestricted bool                        `json:"allow_restricted"`
	TempKey         string                      `json:"temp_key,omitempty"`
	KeyFile         string                      `json:"key_file"`
	BuiltinSSH      bool                        `json:"builtin_ssh"`
	WebTerminal     bool                        `json:"web_terminal"`
	APIListen       string                      `json:"api_listen"`
	APILoopback     bool                        `json:"api_loopback"`
	ClientActivity  map[string]int64            `json:"client_activity,omitempty"`
	Peers           []remotePeerObservationJSON `json:"peers"`
	Forwards        []struct {
		Name       string `json:"name"`
		Listen     string `json:"listen"`
		Remote     string `json:"remote"`
		RemotePort int    `json:"remote_port"`
		Enabled    bool   `json:"enabled"`
		Running    bool   `json:"running"`
		Active     int64  `json:"active"`
		LastError  string `json:"last_error,omitempty"`
	} `json:"forwards"`
}

// cmdRemote 是 remote 命令组入口；pipe/genkey 子命令不经过 API，其余命令操作运行中实例。
//
// 参数说明：
//   - args: []string，包含可选 -c 配置路径、remote 子命令与其参数。
//
// 返回值说明：error，参数解析与对应操作成功时为 nil。
//
// 错误情况：参数非法、本地 API 不可达、后端拒绝配置或纯客户端连接失败时返回错误；
// on/off 只调用一次总开关端点，由后端保证 remote 与 builtin-ssh 原子联动。
func cmdRemote(args []string) error {
	cfgFile, rest, err := parseCFlag("remote", args)
	if err != nil {
		return err
	}
	sub := "status"
	if len(rest) > 0 {
		sub = rest[0]
	}
	// pipe 是 ProxyCommand 用的纯客户端管道，无需守护进程。
	if sub == "pipe" {
		return cmdRemotePipe(rest[1:], cfgFile)
	}
	// genkey 生成应急客户端身份（纯本地，不触碰守护进程与 state-dir）。
	if sub == "genkey" {
		privText, pubText := remote.GenerateClientKey()
		fmt.Printf("客户端私钥（自行妥善保存，连接时用 PROXYD_CLIENT_KEY 或 --client-key 指定）：%s\n", privText)
		fmt.Printf("客户端公钥（录入对端白名单：proxyd remote allow add）：%s\n", pubText)
		return nil
	}

	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	switch sub {
	case "status":
		return remotePrintStatus(c)
	case "on", "off":
		enabled := sub == "on"
		var st remoteStatusJSON
		if err := c.do(http.MethodPost, "/api/remote", map[string]bool{"enabled": enabled}, &st); err != nil {
			return err
		}
		if enabled {
			fmt.Println("远程连接服务端与内嵌 SSH 已开启（proxyd remote token 查看连接 token）")
		} else {
			fmt.Println("远程连接服务端与内嵌 SSH 已关闭")
		}
		return nil
	case "token":
		var out struct {
			Token string `json:"token"`
		}
		if err := c.do(http.MethodGet, "/api/remote/token", nil, &out); err != nil {
			return err
		}
		if out.Token == "" {
			return fmt.Errorf("服务端未在运行，没有可用 token（先 proxyd remote on）")
		}
		fmt.Println(out.Token)
		return nil
	case "serve":
		return cmdRemoteServe(c, rest[1:])
	case "allow":
		return cmdRemoteAllow(c, rest[1:])
	case "audit":
		return cmdRemoteAudit(c, rest[1:])
	case "tempkey":
		return cmdRemoteTempKey(c, rest[1:])
	case "keyfile":
		return cmdRemoteKeyFile(c, rest[1:])
	case "builtin-ssh":
		return cmdRemoteBuiltinSSH(c, rest[1:])
	case "web-terminal":
		return cmdRemoteWebTerminal(c, rest[1:])
	case "remotes":
		return cmdRemoteRemotes(c, rest[1:])
	case "forwards":
		return cmdRemoteForwards(c, rest[1:])
	}
	return fmt.Errorf("未知子命令 %q（status|on|off|token|serve|allow|audit|tempkey|keyfile|builtin-ssh|web-terminal|remotes|forwards|genkey|pipe）", sub)
}

// remotePrintStatus 打印远程连接状态汇总。
func remotePrintStatus(c *apiClient) error {
	var st remoteStatusJSON
	if err := c.do(http.MethodGet, "/api/remote", nil, &st); err != nil {
		return err
	}
	state := "关闭"
	if st.Running {
		state = "运行中"
	} else if st.Enabled {
		state = "配置已开启，但未在运行"
	}
	fmt.Printf("远程连接：%s\n", state)
	if st.Error != "" {
		fmt.Printf("错误：%s\n", st.Error)
	}
	if st.Token != "" {
		fmt.Printf("本机 token：%s（proxyd remote token 查看完整值）\n", st.Token)
	}
	if st.ClientKey != "" {
		fmt.Printf("客户端公钥：%s（对端 tailcat serve --allow 白名单用）\n", st.ClientKey)
	}
	if len(st.Allow) > 0 {
		names := make([]string, 0, len(st.Allow))
		for _, e := range st.Allow {
			if e.Name != "" {
				names = append(names, e.Name)
			}
		}
		if len(names) > 0 {
			fmt.Printf("客户端白名单：%d 个（%s；仅列表内客户端可连接，proxyd remote allow 管理）\n", len(st.Allow), strings.Join(names, "、"))
		} else {
			fmt.Printf("客户端白名单：%d 个（仅列表内客户端可连接，proxyd remote allow 管理）\n", len(st.Allow))
		}
	} else if st.AllowRestricted && st.TempKey == "" {
		fmt.Println("客户端白名单：受限空列表（当前拒绝所有客户端；添加授权或显式清空列表可改变策略）")
	}
	if st.TempKey != "" {
		active := st.ClientActivity[st.TempKey]
		fmt.Printf("临时身份公钥：%s（给客户端连入本机用；活动连接 %d；proxyd remote tempkey 查看私钥/重置）\n", st.TempKey, active)
	}
	if st.Region != "" {
		fmt.Printf("DERP 区域：%s\n", st.Region)
	}
	if st.KeyFile != "" {
		fmt.Printf("密钥文件：%s\n", st.KeyFile)
	}
	if st.BuiltinSSH {
		fmt.Println("内嵌免密 SSH：已开启（隧道 22 端口由进程内 SSH 处理，proxyd remote builtin-ssh off 关闭）")
	}
	if st.WebTerminal {
		fmt.Printf("Web 终端：已开启（管理 API %s；proxyd remote web-terminal off 可立即关闭）\n", st.APIListen)
	}
	fmt.Printf("暴露端口：%s\n", formatPorts(st.Serve))
	if len(st.Peers) > 0 {
		fmt.Println("\n入站客户端：")
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "CLIENT\tSTATUS\tPATH\tRTT\tRX\tTX\tACTIVE")
		for _, peer := range st.Peers {
			client := peer.Name
			if client == "" {
				client = peer.Key
			}
			status := "离线"
			if peer.Online {
				status = "在线"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
				client, status, formatRemotePath(peer.Path, peer.DERPRegion), formatRemoteRTT(peer.RTTMillis),
				formatBytes(peer.RxBytes), formatBytes(peer.TxBytes), peer.Active)
		}
		_ = tw.Flush()
	}
	if len(st.Forwards) > 0 {
		fmt.Println("\n本地转发：")
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tLISTEN\tREMOTE\tENABLED\tRUNNING\tACTIVE\tERROR")
		for _, f := range st.Forwards {
			fmt.Fprintf(tw, "%s\t%s\t%s:%d\t%v\t%v\t%d\t%s\n",
				f.Name, f.Listen, f.Remote, f.RemotePort, f.Enabled, f.Running, f.Active, f.LastError)
		}
		tw.Flush()
	}
	return nil
}

// formatPorts 把端口列表格式化为逗号分隔文本。
func formatPorts(ports []int) string {
	if len(ports) == 0 {
		return "（无）"
	}
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ", ")
}

// formatRemotePath 把内部路径枚举转换为适合命令行阅读的中文文本。
//
// 参数说明：
//   - path: string，direct、derp 或 unknown。
//   - derpRegion: string，中继路径的区域代码或 ID。
//
// 返回值说明：string，直连、中继（区域）或未知。
//
// 错误情况：无；未知枚举保守显示“未知”。
func formatRemotePath(path, derpRegion string) string {
	switch path {
	case remote.PathDirect:
		return "直连"
	case remote.PathDERP:
		if derpRegion != "" {
			return "中继(" + derpRegion + ")"
		}
		return "中继"
	default:
		return "未知"
	}
}

// formatRemoteRTT 格式化毫秒 RTT；零值表示上游未提供而非真实 0ms。
//
// 参数说明：
//   - milliseconds: int64，观测到的往返耗时。
//
// 返回值说明：string，正数显示“数字ms”，否则显示长横线。
//
// 错误情况：无。
func formatRemoteRTT(milliseconds int64) string {
	if milliseconds <= 0 {
		return "—"
	}
	return fmt.Sprintf("%dms", milliseconds)
}

// formatRemoteExpiry 把可选绝对过期时间转换为永久、已过期或剩余时长。
//
// 参数说明：
//   - expiresAt: *time.Time，为 nil 时表示永久授权。
//   - now: time.Time，判断边界与计算剩余时长的统一时钟。
//
// 返回值说明：string，适合列表展示的简短文本。
//
// 错误情况：无；不足一分钟的有效授权仍显示“<1m”，避免误报已过期。
func formatRemoteExpiry(expiresAt *time.Time, now time.Time) string {
	if expiresAt == nil {
		return "永久"
	}
	remaining := expiresAt.Sub(now)
	if remaining <= 0 {
		return "已过期"
	}
	if remaining < time.Minute {
		return "<1m"
	}
	return remaining.Truncate(time.Minute).String()
}

// cmdRemoteServe 查看或设置经隧道暴露的本机端口。
func cmdRemoteServe(c *apiClient, args []string) error {
	if len(args) == 0 {
		var st remoteStatusJSON
		if err := c.do(http.MethodGet, "/api/remote", nil, &st); err != nil {
			return err
		}
		fmt.Printf("暴露端口：%s\n", formatPorts(st.Serve))
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("用法: proxyd remote serve [端口,端口...]（空串清空）")
	}
	var ports []int
	if strings.TrimSpace(args[0]) != "" {
		for _, s := range strings.Split(args[0], ",") {
			p, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				return fmt.Errorf("端口 %q 无效", s)
			}
			ports = append(ports, p)
		}
	}
	var st remoteStatusJSON
	if err := c.do(http.MethodPost, "/api/remote/serve", map[string]any{"ports": ports}, &st); err != nil {
		return err
	}
	fmt.Printf("暴露端口已更新：%s\n", formatPorts(st.Serve))
	return nil
}

// cmdRemoteAllow 查看或增删客户端授权；add 支持 --ttl 与 --ports 最小权限。
// 空列表仅在用户显式删除最后一项时恢复开放，自动 TTL 清扫为空会保持拒绝模式。
func cmdRemoteAllow(c *apiClient, args []string) error {
	load := func() (remoteStatusJSON, error) {
		var st remoteStatusJSON
		if err := c.do(http.MethodGet, "/api/remote", nil, &st); err != nil {
			return remoteStatusJSON{}, err
		}
		return st, nil
	}
	save := func(entries []allowEntryJSON) error {
		var st remoteStatusJSON
		return c.do(http.MethodPost, "/api/remote/allow", map[string]any{"entries": entries}, &st)
	}

	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		st, err := load()
		if err != nil {
			return err
		}
		entries := st.Allow
		if len(entries) == 0 {
			if st.TempKey != "" {
				fmt.Println("白名单为空，但临时身份已生效：仅临时身份可连接")
				return nil
			}
			if st.AllowRestricted {
				fmt.Println("授权列表已因过期清扫为空：当前拒绝所有客户端；显式执行 allow del/重新设置可改变策略")
				return nil
			}
			fmt.Println("白名单为空：放行所有持有 token 的客户端")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tKEY\tEXPIRES\tPORTS")
		for _, e := range entries {
			name := e.Name
			if name == "" {
				name = "-"
			}
			ports := "全部"
			if len(e.Ports) > 0 {
				ports = formatPorts(e.Ports)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, e.Key, formatRemoteExpiry(e.ExpiresAt, time.Now()), ports)
		}
		_ = tw.Flush()
		return nil
	case "add":
		entry, err := parseRemoteAllowAdd(args[1:], time.Now())
		if err != nil {
			return err
		}
		st, err := load()
		if err != nil {
			return err
		}
		entries := st.Allow
		for _, e := range entries {
			if e.Key == entry.Key {
				fmt.Println("该公钥已在白名单中")
				return nil
			}
			if entry.Name != "" && e.Name == entry.Name {
				return fmt.Errorf("别名 %q 已被占用", entry.Name)
			}
		}
		if err := save(append(entries, entry)); err != nil {
			return err
		}
		if entry.Name != "" {
			fmt.Printf("已加入白名单：%s（当前为受限模式，仅列表内客户端可连接）\n", entry.Name)
		} else {
			fmt.Println("已加入白名单（当前为受限模式，仅列表内客户端可连接）")
		}
		return nil
	case "del":
		if len(args) != 2 {
			return fmt.Errorf("用法: proxyd remote allow del <别名|nodekey:... 公钥>")
		}
		st, err := load()
		if err != nil {
			return err
		}
		entries := st.Allow
		next := make([]allowEntryJSON, 0, len(entries))
		found := false
		for _, e := range entries {
			if e.Name == args[1] || e.Key == args[1] {
				found = true
				continue
			}
			next = append(next, e)
		}
		if !found {
			return fmt.Errorf("白名单中没有别名或公钥为 %q 的条目", args[1])
		}
		if err := save(next); err != nil {
			return err
		}
		if len(next) == 0 {
			fmt.Println("已移出白名单（白名单已清空，恢复放行所有客户端）")
		} else {
			fmt.Println("已移出白名单")
		}
		return nil
	}
	return fmt.Errorf("未知子命令 %q（list|add|del）", sub)
}

// parseRemoteAllowAdd 解析 allow add 的位置参数、TTL 和端口限制。
//
// 参数说明：
//   - args: []string，格式为 <公钥> [别名] [--ttl 时长] [--ports 端口,...]。
//   - now: time.Time，TTL 换算绝对过期时间时采用的统一时钟。
//
// 返回值说明：allowEntryJSON 和 error，成功时可直接提交给 HTTP API。
//
// 错误情况：缺少公钥、重复别名、未知参数、非正 TTL 或非法端口文本时返回用法错误。
func parseRemoteAllowAdd(args []string, now time.Time) (allowEntryJSON, error) {
	if len(args) == 0 {
		return allowEntryJSON{}, fmt.Errorf("用法: proxyd remote allow add <nodekey:... 客户端公钥> [别名] [--ttl 1h] [--ports 22,8080]")
	}
	entry := allowEntryJSON{Key: strings.TrimSpace(args[0])}
	for index := 1; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--ttl":
			if index+1 >= len(args) {
				return allowEntryJSON{}, fmt.Errorf("--ttl 后必须提供时长，例如 1h")
			}
			index++
			if err := applyRemoteAllowTTL(&entry, args[index], now); err != nil {
				return allowEntryJSON{}, err
			}
		case strings.HasPrefix(argument, "--ttl="):
			if err := applyRemoteAllowTTL(&entry, strings.TrimPrefix(argument, "--ttl="), now); err != nil {
				return allowEntryJSON{}, err
			}
		case argument == "--ports":
			if index+1 >= len(args) {
				return allowEntryJSON{}, fmt.Errorf("--ports 后必须提供逗号分隔端口")
			}
			index++
			ports, err := parseRemotePorts(args[index])
			if err != nil {
				return allowEntryJSON{}, err
			}
			entry.Ports = ports
		case strings.HasPrefix(argument, "--ports="):
			ports, err := parseRemotePorts(strings.TrimPrefix(argument, "--ports="))
			if err != nil {
				return allowEntryJSON{}, err
			}
			entry.Ports = ports
		case strings.HasPrefix(argument, "-"):
			return allowEntryJSON{}, fmt.Errorf("未知 allow add 参数 %q", argument)
		case entry.Name == "":
			entry.Name = strings.TrimSpace(argument)
		default:
			return allowEntryJSON{}, fmt.Errorf("只能提供一个别名，额外参数 %q 无法识别", argument)
		}
	}
	return entry, nil
}

// applyRemoteAllowTTL 把正数 Go duration 换算为授权绝对过期时间。
//
// 参数说明：
//   - entry: *allowEntryJSON，要更新的授权条目。
//   - raw: string，例如 1h、24h 或 168h。
//   - now: time.Time，计算过期时间的基准。
//
// 返回值说明：error，解析成功时为 nil。
//
// 错误情况：duration 语法非法或小于等于零时返回错误。
func applyRemoteAllowTTL(entry *allowEntryJSON, raw string, now time.Time) error {
	duration, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || duration <= 0 {
		return fmt.Errorf("TTL %q 无效，必须是正数时长（如 1h、24h、168h）", raw)
	}
	expiresAt := now.Add(duration).UTC()
	entry.ExpiresAt = &expiresAt
	return nil
}

// parseRemotePorts 解析逗号分隔的授权端口。
//
// 参数说明：
//   - raw: string，例如 22,8080；空串表示不限制端口。
//
// 返回值说明：[]int 和 error，成功时返回端口切片。
//
// 错误情况：任一片段不是整数时返回错误；范围与重复项继续由服务端统一校验。
func parseRemotePorts(raw string) ([]int, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	ports := make([]int, 0, len(parts))
	for _, part := range parts {
		port, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("授权端口 %q 无效", part)
		}
		ports = append(ports, port)
	}
	return ports, nil
}

// remoteAuditEntryJSON 对应连接审计 API 的单条安全事件。
type remoteAuditEntryJSON struct {
	Time       time.Time `json:"time"`
	ClientKey  string    `json:"client_key,omitempty"`
	ClientName string    `json:"client_name,omitempty"`
	TargetPort int       `json:"target_port"`
	Action     string    `json:"action"`
	Reason     string    `json:"reason,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	RxBytes    int64     `json:"rx_bytes,omitempty"`
	TxBytes    int64     `json:"tx_bytes,omitempty"`
}

// cmdRemoteAudit 查询并打印最近的 remote 连接审计事件。
//
// 参数说明：
//   - c: *apiClient，指向运行中 proxyd 实例。
//   - args: []string，可选 --tail N 或 --tail=N，默认 100。
//
// 返回值说明：error，请求与参数均成功时返回 nil。
//
// 错误情况：参数格式非法、tail 越界或 API 不可达时返回错误。
func cmdRemoteAudit(c *apiClient, args []string) error {
	tail := 100
	switch {
	case len(args) == 0:
	case len(args) == 2 && args[0] == "--tail":
		parsed, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("tail %q 不是整数", args[1])
		}
		tail = parsed
	case len(args) == 1 && strings.HasPrefix(args[0], "--tail="):
		parsed, err := strconv.Atoi(strings.TrimPrefix(args[0], "--tail="))
		if err != nil {
			return fmt.Errorf("tail %q 不是整数", strings.TrimPrefix(args[0], "--tail="))
		}
		tail = parsed
	default:
		return fmt.Errorf("用法: proxyd remote audit [--tail N]")
	}
	if tail < 1 || tail > 500 {
		return fmt.Errorf("tail 必须在 1..500 之间")
	}
	var response struct {
		Entries []remoteAuditEntryJSON `json:"entries"`
	}
	if err := c.do(http.MethodGet, fmt.Sprintf("/api/remote/audit?tail=%d", tail), nil, &response); err != nil {
		return err
	}
	if len(response.Entries) == 0 {
		fmt.Println("（暂无远程连接记录）")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tCLIENT\tPORT\tACTION\tDURATION\tRX\tTX\tREASON")
	for _, entry := range response.Entries {
		client := entry.ClientName
		if client == "" {
			client = entry.ClientKey
		}
		if client == "" {
			client = "未知客户端"
		}
		duration := "—"
		if entry.DurationMS > 0 {
			duration = (time.Duration(entry.DurationMS) * time.Millisecond).Round(time.Millisecond).String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			entry.Time.Local().Format("2006-01-02 15:04:05"), client, entry.TargetPort,
			formatRemoteAuditAction(entry.Action), duration, formatBytes(entry.RxBytes), formatBytes(entry.TxBytes), entry.Reason)
	}
	return tw.Flush()
}

// formatRemoteAuditAction 把审计动作枚举转换为中文展示文本。
//
// 参数说明：
//   - action: string，connected、rejected 或 disconnected。
//
// 返回值说明：string，对应建立、拒绝、断开；未知值原样返回便于兼容新版后端。
//
// 错误情况：无。
func formatRemoteAuditAction(action string) string {
	switch action {
	case remote.AuditActionConnected:
		return "建立"
	case remote.AuditActionRejected:
		return "拒绝"
	case remote.AuditActionDisconnected:
		return "断开"
	default:
		return action
	}
}

// cmdRemoteTempKey 查看临时身份完整密钥对（无参）或生成/重置（reset）。
// 临时身份是给「客户端」连入本机用的应急 nodekey；默认为空，只经此命令或
// Web 按钮手动生成，程序不会自动创建。私钥是连接凭据，仅经 GET /api/remote/tempkey 透出。
func cmdRemoteTempKey(c *apiClient, args []string) error {
	if len(args) > 0 && args[0] == "reset" {
		var before remoteStatusJSON
		existed := false
		if err := c.do(http.MethodGet, "/api/remote", nil, &before); err == nil {
			existed = before.TempKey != ""
		}
		var st remoteStatusJSON
		if err := c.do(http.MethodPost, "/api/remote/tempkey/reset", nil, &st); err != nil {
			return err
		}
		if existed {
			fmt.Printf("临时身份已重置，新公钥：%s（旧私钥已失效；手动白名单条目不受影响）\n", st.TempKey)
		} else {
			fmt.Printf("临时身份已生成，公钥：%s（给客户端连入本机用，proxyd remote tempkey 查看私钥）\n", st.TempKey)
		}
		return nil
	}
	if len(args) > 0 {
		return fmt.Errorf("用法: proxyd remote tempkey [reset]")
	}
	var out struct {
		Public  string `json:"public"`
		Private string `json:"private"`
	}
	if err := c.do(http.MethodGet, "/api/remote/tempkey", nil, &out); err != nil {
		return err
	}
	fmt.Printf("公钥（已在白名单叠加生效）：%s\n", out.Public)
	fmt.Printf("私钥（给客户端连入本机用：PROXYD_CLIENT_KEY 或 --client-key，注意保密，勿当 token 使用）：%s\n", out.Private)
	return nil
}

// cmdRemoteKeyFile 查看/设置自定义密钥路径，或导出、导入内置托管服务端身份。
// 导出文件与私钥同等敏感，始终以 0600 写入；导入先在客户端预检，再由服务端事务切换。
func cmdRemoteKeyFile(c *apiClient, args []string) error {
	if len(args) == 0 {
		var st remoteStatusJSON
		if err := c.do(http.MethodGet, "/api/remote", nil, &st); err != nil {
			return err
		}
		fmt.Printf("密钥文件：%s\n", st.KeyFile)
		return nil
	}
	if args[0] == "export" {
		if len(args) != 2 {
			return fmt.Errorf("用法: proxyd remote keyfile export <路径>")
		}
		data, err := c.raw(http.MethodGet, "/api/remote/keyfile/export", nil, nil, 60*time.Second)
		if err != nil {
			return err
		}
		if err := os.WriteFile(args[1], data, 0o600); err != nil {
			return fmt.Errorf("写入导出密钥 %s 失败: %w", args[1], err)
		}
		if err := os.Chmod(args[1], 0o600); err != nil {
			return fmt.Errorf("收紧导出密钥 %s 权限失败: %w", args[1], err)
		}
		fmt.Printf("服务端私钥已导出到 %s（敏感凭据，请妥善保管）\n", args[1])
		return nil
	}
	if args[0] == "import" {
		if len(args) != 2 {
			return fmt.Errorf("用法: proxyd remote keyfile import <路径>")
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return fmt.Errorf("读取导入密钥 %s 失败: %w", args[1], err)
		}
		if err := remote.ValidateServerKeyData(data); err != nil {
			return err
		}
		if _, err := c.raw(http.MethodPost, "/api/remote/keyfile/import", data,
			map[string]string{"Content-Type": "application/json"}, 60*time.Second); err != nil {
			return err
		}
		fmt.Println("服务端私钥已导入并切换到内置托管身份；连接 token 已按导入身份更新")
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("用法: proxyd remote keyfile [路径|-]|export <路径>|import <路径>")
	}
	path := args[0]
	if path == "-" {
		path = ""
	}
	var st remoteStatusJSON
	if err := c.do(http.MethodPost, "/api/remote/keyfile", map[string]string{"path": path}, &st); err != nil {
		return err
	}
	if path == "" {
		fmt.Printf("已恢复内置托管密钥：%s（token 已随身份切换更新）\n", st.KeyFile)
	} else {
		fmt.Printf("密钥文件已更新：%s（token 已随身份切换更新）\n", st.KeyFile)
	}
	return nil
}

// cmdRemoteBuiltinSSH 查看（无参）或开关内嵌免密 SSH 服务（on|off）。
// 开启后隧道 22 端口由进程内 SSH 服务器直接处理（隧道即认证，无需系统 sshd），
// 与 tailcat serve 的 no-auth-ssh 同模型；token 不变。
func cmdRemoteBuiltinSSH(c *apiClient, args []string) error {
	if len(args) == 0 {
		var st remoteStatusJSON
		if err := c.do(http.MethodGet, "/api/remote", nil, &st); err != nil {
			return err
		}
		fmt.Printf("内嵌免密 SSH：%v\n", st.BuiltinSSH)
		return nil
	}
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		return fmt.Errorf("用法: proxyd remote builtin-ssh [on|off]")
	}
	enabled := args[0] == "on"
	var st remoteStatusJSON
	if err := c.do(http.MethodPost, "/api/remote/builtin-ssh", map[string]bool{"enabled": enabled}, &st); err != nil {
		return err
	}
	if enabled {
		fmt.Println("内嵌免密 SSH 已开启：对端直接 proxyd ssh / tailcat ssh 即可登录（隧道即认证，无需系统 sshd 与账号密码）")
	} else {
		fmt.Println("内嵌免密 SSH 已关闭：隧道 22 端口恢复转发 127.0.0.1:22（系统 sshd）")
	}
	return nil
}

// confirmWebTerminalExposure 在交互终端中要求用户确认非回环 Web 终端的远程 shell 风险。
// 声明为变量便于测试替换输入行为，默认仅接受 y/yes，回车按安全默认值取消。
var confirmWebTerminalExposure = func(apiListen string) bool {
	fmt.Fprintf(os.Stderr,
		"警告：api-listen %s 可被非本机客户端访问；开启 Web 终端等价于向该地址提供当前用户 shell。\n确认仍要开启？[y/N] ",
		apiListen,
	)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// cmdRemoteWebTerminal 查看或热切换浏览器终端；非回环地址强制二次确认。
//
// 参数说明：
//   - c: *apiClient，指向运行中 proxyd 管理 API。
//   - args: []string，无参查看，或 on|off；自动化场景允许 on --yes 显式确认风险。
//
// 返回值说明：error，查询/切换成功时为 nil。
//
// 错误情况：参数非法、非交互环境未提供 --yes、用户取消、API 安全门拒绝或配置
// 事务失败时返回错误；off 永远无需确认，便于紧急关闭。
func cmdRemoteWebTerminal(c *apiClient, args []string) error {
	if len(args) == 0 {
		var status remoteStatusJSON
		if err := c.do(http.MethodGet, "/api/remote", nil, &status); err != nil {
			return err
		}
		state := "关闭"
		if status.WebTerminal {
			state = "开启"
		}
		fmt.Printf("Web 终端：%s（api-listen %s）\n", state, status.APIListen)
		return nil
	}
	if len(args) > 2 || (args[0] != "on" && args[0] != "off") || (len(args) == 2 && args[1] != "--yes") {
		return fmt.Errorf("用法: proxyd remote web-terminal [on|off] [--yes]")
	}
	enabled := args[0] == "on"
	acknowledged := len(args) == 2 && args[1] == "--yes"
	var current remoteStatusJSON
	if err := c.do(http.MethodGet, "/api/remote", nil, &current); err != nil {
		return err
	}
	if enabled && !current.APILoopback && !acknowledged {
		if !isTerminal() {
			return fmt.Errorf("api-listen %s 不是回环地址，非交互环境请审阅风险后使用 on --yes", current.APIListen)
		}
		if !confirmWebTerminalExposure(current.APIListen) {
			return fmt.Errorf("已取消开启 Web 终端")
		}
		acknowledged = true
	}
	var updated remoteStatusJSON
	if err := c.do(http.MethodPost, "/api/remote/web-terminal", map[string]bool{
		"enabled":              enabled,
		"acknowledge_exposure": acknowledged,
	}, &updated); err != nil {
		return err
	}
	if enabled {
		fmt.Printf("Web 终端已开启（api-listen %s）；该能力等价于当前用户 shell，仅在需要时开启\n", updated.APIListen)
		return nil
	}
	fmt.Println("Web 终端已关闭；新 WebSocket 会话将返回 404")
	return nil
}

// cmdRemoteRemotes 管理保存的远端（名称 → token）。
func cmdRemoteRemotes(c *apiClient, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		var out struct {
			Remotes []struct {
				Name  string `json:"name"`
				Token string `json:"token"`
			} `json:"remotes"`
		}
		if err := c.do(http.MethodGet, "/api/remote/remotes", nil, &out); err != nil {
			return err
		}
		if len(out.Remotes) == 0 {
			fmt.Println("（无保存的远端）")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tTOKEN\tSTATUS\tPATH\tRTT")
		for _, r := range out.Remotes {
			ping, pingErr := probeRemoteForCLI(c, r.Name)
			if pingErr != nil {
				fmt.Fprintf(tw, "%s\t%s\t离线\t—\t—\n", r.Name, r.Token)
				continue
			}
			fmt.Fprintf(tw, "%s\t%s\t在线\t%s\t%s\n",
				r.Name, r.Token, formatRemotePath(ping.Path, ping.DERPRegion), formatRemoteRTT(ping.RTTMillis))
		}
		_ = tw.Flush()
		return nil
	case "add":
		if len(args) != 3 {
			return fmt.Errorf("用法: proxyd remote remotes add <名> <token>")
		}
		var entry struct {
			Name string `json:"name"`
		}
		if err := c.do(http.MethodPost, "/api/remote/remotes",
			map[string]string{"name": args[1], "token": args[2]}, &entry); err != nil {
			return err
		}
		fmt.Printf("远端 %q 已保存（proxyd ssh %s 可直接连接）\n", entry.Name, entry.Name)
		return nil
	case "del":
		if len(args) != 2 {
			return fmt.Errorf("用法: proxyd remote remotes del <名>")
		}
		if err := c.do(http.MethodDelete, "/api/remote/remotes/"+args[1], nil, nil); err != nil {
			return err
		}
		fmt.Printf("远端 %q 已删除\n", args[1])
		return nil
	}
	return fmt.Errorf("未知子命令 %q（list|add|del）", sub)
}

// probeRemoteForCLI 调用守护进程对已保存远端执行一次带上限的 disco ping。
//
// 参数说明：
//   - c: *apiClient，指向运行中 proxyd 实例。
//   - name: string，remotes 中的远端名称。
//
// 返回值说明：remotePingJSON 和 error，成功时含 RTT 与 direct/derp 路径。
//
// 错误情况：守护进程不可达、远端离线、授权拒绝或 12 秒探测超时时返回错误。
func probeRemoteForCLI(c *apiClient, name string) (remotePingJSON, error) {
	var result remotePingJSON
	err := c.doTimeout(http.MethodPost, "/api/remote/ping", map[string]string{"remote": name}, &result, 15*time.Second)
	return result, err
}

// cmdRemoteForwards 管理本地转发。
func cmdRemoteForwards(c *apiClient, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		return remotePrintStatus(c)
	case "add":
		if len(args) != 5 {
			return fmt.Errorf("用法: proxyd remote forwards add <名> <监听地址> <远端名|token> <远端端口>")
		}
		port, err := strconv.Atoi(args[4])
		if err != nil {
			return fmt.Errorf("远端端口 %q 无效", args[4])
		}
		if err := c.do(http.MethodPost, "/api/remote/forwards", map[string]any{
			"name": args[1], "listen": args[2], "remote": args[3], "remote_port": port,
		}, nil); err != nil {
			return err
		}
		fmt.Printf("转发 %q 已创建并启用\n", args[1])
		return nil
	case "del":
		if len(args) != 2 {
			return fmt.Errorf("用法: proxyd remote forwards del <名>")
		}
		if err := c.do(http.MethodDelete, "/api/remote/forwards/"+args[1], nil, nil); err != nil {
			return err
		}
		fmt.Printf("转发 %q 已删除\n", args[1])
		return nil
	case "on", "off":
		if len(args) != 2 {
			return fmt.Errorf("用法: proxyd remote forwards %s <名>", sub)
		}
		if err := c.do(http.MethodPut, "/api/remote/forwards/"+args[1], map[string]bool{"enabled": sub == "on"}, nil); err != nil {
			return err
		}
		fmt.Printf("转发 %q 已%s\n", args[1], map[string]string{"on": "启用", "off": "停用"}[sub])
		return nil
	}
	return fmt.Errorf("未知子命令 %q（list|add|del|on|off）", sub)
}

// cmdRemotePipe 把 stdio 与远端隧道端口双向管道化（OpenSSH ProxyCommand 用法）。
// 纯客户端命令，不需要守护进程运行。客户端身份密钥取自 cfgFile 对应配置的
// state-dir（缺省回退默认 state-dir），让对端 --allow 白名单能识别本机；
// 密钥加载失败时告警并回退为临时身份（保持旧行为可用）。
func cmdRemotePipe(args []string, cfgFile string) error {
	if len(args) != 2 {
		return fmt.Errorf("用法: proxyd remote pipe [-c 配置] <token> <端口>")
	}
	port, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("端口 %q 无效", args[1])
	}
	return remote.Pipe(context.Background(), args[0], port, pipeClientKey(cfgFile), os.Stdin, os.Stdout)
}

// pipeClientKey 解析 pipe 应使用的客户端身份密钥，优先级：
//  1. PROXYD_CLIENT_KEY 环境变量（临时/应急身份，proxyd ssh --client-key 即经此透传）
//  2. cfgFile 配置 state-dir 下的持久密钥（配置不可读时回退默认 state-dir）
//
// 密钥读写失败则告警并返回零值（Dial 会回退为每次连接生成临时身份）。
func pipeClientKey(cfgFile string) (priv key.NodePrivate) {
	if text := os.Getenv("PROXYD_CLIENT_KEY"); text != "" {
		priv, err := remote.ParseClientKey(text)
		if err != nil {
			fmt.Fprintf(os.Stderr, "警告: PROXYD_CLIENT_KEY 无效（%v），回退为持久客户端身份\n", err)
		} else {
			return priv
		}
	}
	stateDir := config.DefaultStateDir()
	if cfg, err := config.Load(cfgFile); err == nil && cfg.StateDir != "" {
		stateDir = cfg.StateDir
	}
	priv, err := remote.LoadOrCreateClientKey(stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告: 客户端身份密钥不可用（%v），本次连接使用临时身份，无法通过对端 --allow 白名单\n", err)
		return key.NodePrivate{}
	}
	return priv
}

// cmdSCP 经 tailcat 隧道执行系统 scp（镜像 cmdSSH）：远端操作数以 tc... token
// 作为主机名（可带 user@ 前缀），以 `proxyd remote pipe` 作为 ProxyCommand
// 转发到对端 22 端口，token 主机名因此不会进入 DNS 解析。纯客户端命令。
func cmdSCP(args []string) error {
	args, clientKeyText, err := extractClientKey(args)
	if err != nil {
		return err
	}
	cfgFile, rest, err := parseCFlag("scp", args)
	if err != nil {
		return err
	}
	token, err := findTunnelToken(rest)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	scpExe, err := exec.LookPath("scp")
	if err != nil {
		return fmt.Errorf("未找到系统 scp 客户端（%v）", err)
	}

	// ProxyCommand 由 scp/ssh 经 shell 执行，引号规则与 cmdSSH 一致：
	// Windows 下 cmd.exe 不允许 Go %q 的反斜杠转义，用普通双引号包路径。
	// -c 透传原因同 cmdSSH（客户端身份密钥与配置 state-dir 对齐）。
	quotedExe := fmt.Sprintf("%q", exe)
	quotedCfg := fmt.Sprintf("%q", cfgFile)
	if runtime.GOOS == "windows" {
		quotedExe = "\"" + exe + "\""
		quotedCfg = "\"" + cfgFile + "\""
	}
	argv := []string{"-o", fmt.Sprintf("ProxyCommand=%s remote -c %s pipe %s 22", quotedExe, quotedCfg, token)}
	argv = append(argv, rest...)

	cmd := exec.Command(scpExe, argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// --client-key 透传方式同 cmdSSH。
	if clientKeyText != "" {
		priv, err := remote.ParseClientKey(clientKeyText)
		if err != nil {
			return err
		}
		raw, _ := priv.MarshalText()
		cmd.Env = append(os.Environ(), "PROXYD_CLIENT_KEY="+string(raw))
	}
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}

// findTunnelToken 在 scp 参数中定位隧道远端操作数并返回其 token。
//
// 参数：
//   - args: []string，scp 子命令的原始参数（选项与操作数混合）。
//
// 返回值：
//   - string：唯一出现的 tc... 隧道 token。
//   - error：没有隧道操作数时返回用法错误；源与目标出现两个不同 token 时返回
//     单跳限制错误（隧道不支持 tc→tc 一跳中转）。
//
// 错误情况：只有主机部分（剥掉可选 user@ 前缀与 :path 后缀后）通过
// remote.ValidateToken 的参数才被视为隧道操作数，其余参数原样透传给 scp。
func findTunnelToken(args []string) (string, error) {
	token := ""
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		host := a
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}
		if i := strings.Index(host, ":"); i >= 0 {
			host = host[:i]
		}
		if remote.ValidateToken(host) != nil {
			continue
		}
		if token == "" {
			token = host
		} else if token != host {
			return "", fmt.Errorf("源与目标使用了不同的隧道 token；隧道只支持单跳，请分两次传输")
		}
	}
	if token == "" {
		return "", fmt.Errorf("用法: proxyd scp [scp选项...] <源...> <目标>（远端操作数以 tc... token 作为主机名，如 tc-xxxx:/tmp/ 或 root@tc-xxxx:/tmp/）")
	}
	return token, nil
}

// extractClientKey 从原始参数中摘除 --client-key（值可为下一参数或 = 连接形式）。
// 必须先于 parseCFlag 调用：其 flag 集遇到未注册 flag 会直接退出进程。
func extractClientKey(args []string) (rest []string, keyText string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--client-key":
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("--client-key 缺少私钥参数")
			}
			keyText = args[i+1]
			i++
		case strings.HasPrefix(a, "--client-key="):
			keyText = strings.TrimPrefix(a, "--client-key=")
		default:
			rest = append(rest, a)
		}
	}
	return rest, keyText, nil
}

// cmdSSH 经 tailcat 隧道执行系统 ssh（等价 tailcat ssh）：解析远端名 → token，
// 以 `proxyd remote pipe` 作为 ProxyCommand 启动系统 ssh。纯客户端命令。
func cmdSSH(args []string) error {
	args, clientKeyText, err := extractClientKey(args)
	if err != nil {
		return err
	}
	cfgFile, rest, err := parseCFlag("ssh", args)
	if err != nil {
		return err
	}
	// 解析 -p 端口（默认 22）；首个非 flag 参数为 [user@]<远端名|token>，其余透传给 ssh。
	port := "22"
	var dst string
	var sshArgs []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "-p" && i+1 < len(rest):
			port = rest[i+1]
			i++
		case strings.HasPrefix(a, "-p") && len(a) > 2:
			port = a[2:]
		case dst == "" && !strings.HasPrefix(a, "-"):
			dst = a
		default:
			sshArgs = append(sshArgs, a)
		}
	}
	if dst == "" {
		return fmt.Errorf("用法: proxyd ssh [-c 配置] [-p 端口] [--client-key privkey:私钥] [user@]<远端名|token> [ssh 参数/远端命令...]")
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum <= 0 || portNum > 65535 {
		return fmt.Errorf("端口 %q 无效", port)
	}

	user, name, hasUser := strings.Cut(dst, "@")
	if !hasUser {
		name = user
		user = ""
	}

	// 名称 → token：读配置文件的 remotes；直接给 tc... token 时原样使用（无需配置文件）。
	var token string
	if strings.HasPrefix(name, "tc") {
		token = name
	} else {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("读取配置 %s 失败（用于解析远端名）: %w", cfgFile, err)
		}
		if token, err = remote.ResolveToken(cfg.Remote.Remotes, name); err != nil {
			return err
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	sshExe, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("未找到系统 ssh 客户端（%v）；可改用 proxyd remote forwards 做本地转发后自行连接", err)
	}

	// ssh 会把目标主机名代入 ControlPath 等展开，token 太长会撑爆 Unix socket 路径，
	// 因此给 ssh 一个短主机名；真实 token 只出现在 ProxyCommand 参数里。
	sshHost := name
	if strings.HasPrefix(name, "tc") {
		sshHost = "tailcat-" + token[len(token)-8:]
	}
	sshDst := sshHost
	if user != "" {
		sshDst = user + "@" + sshHost
	}
	// ProxyCommand 由 ssh 经 shell 执行：Windows 下 cmd.exe 不允许 Go %q 的反斜杠转义
	// （会吃掉路径里的反斜杠），因此 Windows 用普通双引号包路径。
	// -c 透传给 pipe：让 pipe 从同一配置解析 state-dir，客户端身份密钥与
	// 守护进程/其他 CLI 调用保持一致（对端 --allow 白名单只需登记一个公钥）。
	quotedExe := fmt.Sprintf("%q", exe)
	quotedCfg := fmt.Sprintf("%q", cfgFile)
	if runtime.GOOS == "windows" {
		quotedExe = "\"" + exe + "\""
		quotedCfg = "\"" + cfgFile + "\""
	}
	proxyCmd := fmt.Sprintf("%s remote -c %s pipe %s %d", quotedExe, quotedCfg, token, portNum)

	argv := []string{
		"-o", "UpdateHostKeys no",
		"-o", "StrictHostKeyChecking no",
		"-o", "UserKnownHostsFile " + os.DevNull,
		"-o", "LogLevel ERROR",
		"-o", "ProxyCommand=" + proxyCmd,
		sshDst,
	}
	argv = append(argv, sshArgs...)

	cmd := exec.Command(sshExe, argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// --client-key 指定的临时身份经环境变量透传给 ProxyCommand 的 pipe 子进程，
	// 避免私钥出现在 pipe 的命令行（ps 可见）里。
	if clientKeyText != "" {
		priv, err := remote.ParseClientKey(clientKeyText)
		if err != nil {
			return err
		}
		raw, _ := priv.MarshalText()
		cmd.Env = append(os.Environ(), "PROXYD_CLIENT_KEY="+string(raw))
	}
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}
