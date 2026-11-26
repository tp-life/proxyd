package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"

	"proxyd/internal/config"
)

// serverKeyRelPath 是内置托管服务端密钥在 stateDir 下的相对路径。
// 密钥决定 token：文件在则 token 稳定，删除后下次启动生成全新 token。
// 配置 key-file 时改用该路径（可指向 tailcat genkey 生成的密钥，使两边 token 一致）。
const serverKeyRelPath = "remote/server.private.json"

// autoRegionDiscoveryTimeout 限制自动 DERP 地图获取与就近区域探测时间。
// 该流程位于服务启动和 remote 配置事务的同步路径，必须有上界。
const autoRegionDiscoveryTimeout = 15 * time.Second

// serverKeyPath 返回服务端密钥的实际路径：配置 key-file 时展开 ~/ 后使用，
// 否则用内置托管路径 <stateDir>/remote/server.private.json。
func (m *Manager) serverKeyPath(cfg config.RemoteConfig) string {
	if p := strings.TrimSpace(cfg.KeyFile); p != "" {
		return expandHome(p)
	}
	return filepath.Join(m.stateDir, serverKeyRelPath)
}

// expandHome 把 ~/ 开头的路径展开为用户主目录下的绝对路径。
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// autoRegionLocked 返回自动就近模式下的 DERP 区域：首次调用恢复持久缓存或经备用地图探测并缓存，
// 之后（含配置变更引发的重建）沿用缓存，保证 token 中的区域信息跨进程稳定。
// DERPMapURL 变化时缓存失效、重新探测。调用方需持有 m.mu。
func (m *Manager) autoRegionLocked(cfg config.RemoteConfig) (*tailcfg.DERPRegion, error) {
	if m.autoRegion != nil && m.autoRegionMapURL == cfg.DERPMapURL {
		return m.autoRegion, nil
	}
	if region := m.loadDERPRegion(cfg.DERPMapURL); region != nil {
		m.autoRegion, m.autoRegionMapURL = region, cfg.DERPMapURL
		m.logf("[remote] 恢复持久 DERP 区域 %d（%s）", region.RegionID, region.RegionName)
		return region, nil
	}
	sources := []string{cfg.DERPMapURL}
	if cfg.DERPMapURL == "" {
		sources = []string{tailcat.DefaultDERPMapURL, fallbackDERPMapURL}
	}
	ctx, cancel := context.WithTimeout(context.Background(), autoRegionDiscoveryTimeout)
	defer cancel()
	region, err := discoverDERPRegion(ctx, -1, sources)
	if err != nil {
		return nil, err
	}
	m.autoRegion = region
	if err := m.saveDERPRegion(cfg.DERPMapURL, region); err != nil {
		m.logf("[remote] 保存 DERP 区域缓存失败: %v", err)
	}
	m.autoRegionMapURL = cfg.DERPMapURL
	m.logf("[remote] 自动选定 DERP 区域 %d（%s），持久保持不变", m.autoRegion.RegionID, m.autoRegion.RegionName)
	return m.autoRegion, nil
}

// startServerLocked 按配置启动隧道服务端并刷新 token；调用方需持有 m.mu。
// 参数说明：cfg 为 config.RemoteConfig，已验证的服务端配置快照。
// 返回值说明：error，身份恢复、SSH 初始化与 tailcat 启动成功时为 nil。
// 错误情况：文件、区域查询或服务启动失败时返回错误；旧密钥不自动增加 PSK，避免身份迁移。
func (m *Manager) startServerLocked(cfg config.RemoteConfig) error {
	identity, err := loadOrCreateTailcatKey(m.serverKeyPath(cfg))
	if err != nil {
		return err
	}

	regionID, hosts, err := parseRegion(cfg.Region)
	if err != nil {
		return err
	}

	s := &tailcat.Server{
		Key: identity.Private,
		// tailcat v0.6 起，Key 为零时自动生成随机 PSK（v0.8.0 语义不变）；
		// 必须恢复文件中的 PSK 才能保持 token 稳定。
		// 老文件没有 PSK，显式沿用兼容模式，避免升级后已分发的 token 全部失效。
		PresharedKey:        identity.Public.PresharedKey,
		DisablePresharedKey: identity.Public.PresharedKey.IsZero(),
		Hybrid:              cfg.Hybrid,
		Logf:                logger.Logf(m.logf),
		DERPMapURL:          cfg.DERPMapURL,
		// tailcat v0.8.0 起使用动态 AllowClient 钩子：每次客户端接入时按最新配置
		// 实时评估白名单、临时身份、受限空列表与 TTL 边界，不再需要占位假密钥，
		// 白名单变更也无需重建隧道服务端。
		AllowClient: m.allowClient,
	}
	// 启动前仍校验已配置的白名单与临时身份公钥格式，避免损坏配置静默落入运行态。
	seenKey := map[string]bool{}
	for _, text := range append(allowKeys(cfg.Allow), cfg.TempKey) {
		text = strings.TrimSpace(text)
		if text == "" || seenKey[text] {
			continue
		}
		seenKey[text] = true
		if _, err := ValidateClientKey(text); err != nil {
			return err
		}
	}
	if len(hosts) > 0 {
		// 自建 derper：构造内嵌区域，token 会携带完整区域信息，客户端无需 DERP map。
		// RegionID 900 沿用 tailcfg 对自定义区域的编号约定，仅作占位。
		region := &tailcfg.DERPRegion{RegionID: 900, RegionCode: "custom", RegionName: cfg.Region}
		for _, h := range hosts {
			region.Nodes = append(region.Nodes, &tailcfg.DERPNode{HostName: h})
		}
		s.Region = region
	} else if regionID > 0 {
		sources := []string{cfg.DERPMapURL}
		if cfg.DERPMapURL == "" {
			sources = []string{tailcat.DefaultDERPMapURL, fallbackDERPMapURL}
		}
		ctx, cancel := context.WithTimeout(context.Background(), autoRegionDiscoveryTimeout)
		defer cancel()
		s.Region, err = discoverDERPRegion(ctx, tailcfg.DERPRegionID(regionID), sources)
		if err != nil {
			return err
		}
	} else {
		// 自动就近：进程内粘性。区域探测结果随网络抖动可能在相邻区域间摇摆，
		// 而 token 嵌入了区域信息——若每次重建都重新探测，任何配置变更（白名单/
		// 端口/内嵌 SSH）都可能让已分发的 token 指向旧区域而失效。因此首次探测后
		// 缓存区域对象，重建时沿用（DERPMapURL 变化时重新探测）。
		region, err := m.autoRegionLocked(cfg)
		if err != nil {
			return err
		}
		s.Region = region
	}

	servePorts := map[uint16]bool{}
	for _, p := range cfg.Serve {
		port := uint16(p)
		servePorts[port] = true
		s.ServedTCPPorts = append(s.ServedTCPPorts, filter.PortRange{First: port, Last: port})
		if !(port == 22 && cfg.BuiltinSSH) {
			s.ServedUDPPorts = append(s.ServedUDPPorts, filter.PortRange{First: port, Last: port})
		}
	}
	type parsedServeTarget struct {
		network string
		addr    string
		raw     string
	}
	serveTargets := map[uint16]parsedServeTarget{}
	for _, item := range cfg.ServeTargets {
		netType, targetAddr, perr := config.ParseRemoteServeTarget(item.Target)
		if perr != nil {
			return fmt.Errorf("暴露端口映射 %d → %q 无效: %w", item.Port, item.Target, perr)
		}
		port := uint16(item.Port)
		serveTargets[port] = parsedServeTarget{network: netType, addr: targetAddr, raw: strings.TrimSpace(item.Target)}
		if !servePorts[port] {
			s.ServedTCPPorts = append(s.ServedTCPPorts, filter.PortRange{First: port, Last: port})
			if netType != "unix" && !(port == 22 && cfg.BuiltinSSH) {
				s.ServedUDPPorts = append(s.ServedUDPPorts, filter.PortRange{First: port, Last: port})
			}
		}
	}
	// 内嵌免密 SSH：隧道 22 端口由本模块的可取消 shell handler 直接处理
	// （隧道即认证），覆盖对 127.0.0.1:22 的转发，无需系统 sshd。
	// 不再使用 tailcat 内置 handler：上游实现在客户端异常断开时不终止
	// shell，会残留 PTY、子进程和处理协程。host key 持久化在 state-dir/remote。
	var sshHandler func(net.Conn)
	if cfg.BuiltinSSH {
		peerKeyLookup := func(remoteAddr net.Addr) string {
			if pub, ok := s.PeerKey(remoteAddr); ok && !pub.IsZero() {
				return pub.String()
			}
			return ""
		}
		sshHandler, err = managedShellSSHHandlerWithOptions(m.stateDir, m.sshAccess, cfg.ShellUser, shellSSHOptions{
			allowLocalForwarding: true,
			peerKeyLookup:        peerKeyLookup,
		})
		if err != nil {
			return err
		}
		if !servePorts[22] {
			if _, mapped := serveTargets[22]; !mapped {
				s.ServedTCPPorts = append(s.ServedTCPPorts, filter.PortRange{First: 22, Last: 22})
			}
		}
	}
	s.OnTCP = func(port uint16) func(net.Conn) {
		if port == 22 && sshHandler != nil {
			return m.guardConnection(port, sshHandler)
		}
		if servePorts[port] {
			return m.guardConnection(port, func(c net.Conn) {
				m.serveConn(c, port)
			})
		}
		if target, ok := serveTargets[port]; ok {
			return m.guardConnection(port, func(c net.Conn) {
				m.serveTargetConn(c, port, target.network, target.addr, target.raw)
			})
		}
		return nil // RST
	}
	s.OnUDP = func(port uint16) func(tailcat.ConnPacketConn) {
		if port == 22 && sshHandler != nil {
			return nil
		}
		if servePorts[port] {
			return m.guardUDPFlow(port, func(flow tailcat.ConnPacketConn) {
				m.serveUDPFlow(flow, port, "loopback", strconv.Itoa(int(port)), fmt.Sprintf("127.0.0.1:%d", port))
			})
		}
		if target, ok := serveTargets[port]; ok && target.network != "unix" {
			return m.guardUDPFlow(port, func(flow tailcat.ConnPacketConn) {
				m.serveUDPFlow(flow, port, target.network, target.addr, target.raw)
			})
		}
		return nil
	}

	if err := s.Start(); err != nil {
		return err
	}
	m.srv = s
	m.token = string(s.TailcatAddr())
	m.serveErr = ""
	sshNote := ""
	if cfg.BuiltinSSH {
		sshNote = "，内嵌免密 SSH（隧道 22 端口）"
		if cfg.SSHAuthRequired {
			sshNote = "，内嵌 SSH 公钥认证（隧道 22 端口）"
		}
	}
	hybridNote := ""
	if cfg.Hybrid {
		hybridNote = "，后量子混合握手（ML-KEM-768）"
	}
	mappingNote := ""
	if len(cfg.ServeTargets) > 0 {
		mappingNote = fmt.Sprintf("，自定义映射 %d 条", len(cfg.ServeTargets))
	}
	if strings.TrimSpace(cfg.KeyFile) != "" {
		m.logf("[remote] 隧道服务端已启动，暴露端口 %v%s%s%s，密钥文件 %s", cfg.Serve, mappingNote, sshNote, hybridNote, m.serverKeyPath(cfg))
	} else {
		m.logf("[remote] 隧道服务端已启动，暴露端口 %v%s%s%s", cfg.Serve, mappingNote, sshNote, hybridNote)
	}
	return nil
}

// stopServerLocked 停止隧道服务端；调用方需持有 m.mu。
func (m *Manager) stopServerLocked() {
	if m.srv != nil {
		_ = m.srv.Close()
		m.srv = nil
		m.token = ""
	}
	m.serveErr = ""
}

// loopbackDialTimeout 限制 serve 语义下向本机目标端口建连的总时长。
const loopbackDialTimeout = 10 * time.Second

// loopbackHosts 是 serve 语义依次尝试的本机目标地址：IPv4 回环优先，
// 失败后回退 IPv6 回环，兼容只监听 ::1 的本机服务。
var loopbackHosts = [...]string{"127.0.0.1", "::1"}

// serveConn 把隧道内到来的连接转发到本机回环端口（serve 语义的实现）。
func (m *Manager) serveConn(c net.Conn, port uint16) {
	defer c.Close()
	upstream, err := dialLoopback(port)
	if err != nil {
		m.logf("[remote] 隧道连接 → 本机端口 %d 失败: %v", port, err)
		return
	}
	defer upstream.Close()
	tuneLocalConn(upstream)
	relay(c, upstream)
}

// serveTargetConn 把隧道端口转发到自定义目标（本机异端口、局域网主机/域名或 Unix 域套接字）。
func (m *Manager) serveTargetConn(c net.Conn, port uint16, network, addr, rawTarget string) {
	defer c.Close()
	upstream, err := dialServeTarget(network, addr)
	if err != nil {
		m.logf("[remote] 隧道连接 端口 %d → %s 失败: %v", port, rawTarget, err)
		return
	}
	defer upstream.Close()
	if network != "unix" {
		tuneLocalConn(upstream)
	}
	relay(c, upstream)
}

// serveUDPFlow 把隧道内的 UDP 流（例如 Windows RDP 3389 UDP 传输）转发到本机或局域网 UDP 目标。
func (m *Manager) serveUDPFlow(flow tailcat.ConnPacketConn, port uint16, network, addr, rawTarget string) {
	defer flow.Close()
	var (
		upstream net.Conn
		err      error
	)
	if network == "loopback" {
		upstream, err = dialLoopbackUDP(addr)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), loopbackDialTimeout)
		defer cancel()
		var dialer net.Dialer
		upstream, err = dialer.DialContext(ctx, "udp", addr)
	}
	if err != nil {
		m.logf("[remote] 隧道 UDP 端口 %d → %s 失败: %v", port, rawTarget, err)
		return
	}
	defer upstream.Close()
	relayUDP(flow, upstream)
}

// dialLoopbackUDP 依次尝试 127.0.0.1 与 ::1 建立本机 UDP 连接，兼容纯 IPv6 回环环境。
func dialLoopbackUDP(portStr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), loopbackDialTimeout)
	defer cancel()
	var dialer net.Dialer
	var failures []string
	for _, host := range loopbackHosts {
		conn, err := dialer.DialContext(ctx, "udp", net.JoinHostPort(host, portStr))
		if err == nil {
			return conn, nil
		}
		failures = append(failures, fmt.Sprintf("%s: %v", host, err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("连接本机 UDP 端口 %s 失败（%s）", portStr, strings.Join(failures, "; "))
}

// dialServeTarget 按解析后的目标类型拨号上游服务：loopback 走 127.0.0.1/::1 回退，
// tcp 直拨 host:port（支持 IP 与系统 DNS 主机名），unix 连接本机 Unix 域套接字。
func dialServeTarget(network, addr string) (net.Conn, error) {
	switch network {
	case "loopback":
		port, err := strconv.Atoi(addr)
		if err != nil || port <= 0 || port > 65535 {
			return nil, fmt.Errorf("无效本机目标端口 %q", addr)
		}
		return dialLoopback(uint16(port))
	case "tcp", "unix":
		ctx, cancel := context.WithTimeout(context.Background(), loopbackDialTimeout)
		defer cancel()
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, addr)
	default:
		return nil, fmt.Errorf("不支持的目标类型 %q", network)
	}
}

// dialLoopback 依次尝试 127.0.0.1 与 ::1 上的 port，返回第一个可用连接。
//
// 参数说明：
//   - port: uint16，隧道要访问的本机端口。
//
// 返回值说明：net.Conn，成功建立的本机 TCP 连接；两个地址都失败时返回合并错误。
//
// 错误情况：两次尝试共享 loopbackDialTimeout 总预算；连接被拒绝会立即换下一个
// 地址，预算耗尽则不再尝试，并把各地址的失败原因拼成单行错误返回给调用方记录。
func dialLoopback(port uint16) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), loopbackDialTimeout)
	defer cancel()
	var dialer net.Dialer
	var failures []string
	for _, host := range loopbackHosts {
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
		if err == nil {
			return conn, nil
		}
		// 各地址的失败原因用 "; " 拼成单行，避免多行日志在 Web/CLI 日志面板里断行。
		failures = append(failures, fmt.Sprintf("%s: %v", host, err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.New(strings.Join(failures, "; "))
}

// allowKeys 提取白名单条目的公钥列表（别名只用于管理展示，不参与握手判定）。
func allowKeys(entries []config.RemoteAllowEntry) []string {
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, e.Key)
	}
	return keys
}

// relay 双向拷贝两个连接，任一端结束后关闭两边，并等待另一个拷贝协程退出。
//
// 参数说明：
//   - a: net.Conn，双向转发的一端。
//   - b: net.Conn，双向转发的另一端。
//
// 返回值说明：无；两个拷贝协程均退出后返回。
//
// 错误情况：转发错误不向上返回；关闭两端会解除另一个方向的阻塞，
// 等待第二个完成信号可保证函数返回时不残留持有连接的拷贝协程。
func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	copyOne := func(dst, src net.Conn) {
		buffer := getRelayCopyBuffer()
		defer putRelayCopyBuffer(buffer)
		_, _ = io.CopyBuffer(dst, src, buffer)
		done <- struct{}{}
	}
	go copyOne(a, b)
	go copyOne(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

// ValidateKeyFile 校验自定义服务端密钥文件：已存在时必须能解析为 tailcat 密钥
// 且含私钥；不存在时视为通过（启动时会在该路径生成新密钥）。
func ValidateKeyFile(path string) error {
	path = expandHome(strings.TrimSpace(path))
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取密钥文件 %s 失败: %w", path, err)
	}
	var saved tailcat.PrivateKey
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("密钥文件 %s 不是合法的 tailcat 密钥: %w", path, err)
	}
	if saved.Private.IsZero() {
		return fmt.Errorf("密钥文件 %s 不含私钥", path)
	}
	return nil
}

// loadOrCreateNodeKey 读取客户端所需的持久化节点私钥。
// 参数说明：path 为 string，tailcat JSON 密钥文件路径。
// 返回值说明：key.NodePrivate 和 error，返回文件中的节点身份。
// 错误情况：文件读取、解析或创建失败时返回错误，不使用零值私钥降级。
func loadOrCreateNodeKey(path string) (priv key.NodePrivate, err error) {
	saved, err := loadOrCreateTailcatKey(path)
	if err != nil {
		return priv, err
	}
	return saved.Private, nil
}

// loadOrCreateTailcatKey 读取完整 tailcat 身份，保留 v0.6 起新增的持久化 PSK（v0.7.0 语义不变）。
// 参数说明：path 为 string，服务端或客户端密钥文件路径。
// 返回值说明：*tailcat.PrivateKey 和 error；老文件保持原值，新文件包含随机 PSK。
// 错误情况：损坏文件或写入失败时返回错误。新文件通过同目录原子替换以 0600 落盘；
// 服务端调用由 Manager 锁串行化，既有身份不会因为一次启动失败而被重新生成。
func loadOrCreateTailcatKey(path string) (*tailcat.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if err := ValidateServerKeyData(data); err != nil {
			return nil, err
		}
		var saved tailcat.PrivateKey
		if jerr := json.Unmarshal(data, &saved); jerr != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", path, jerr)
		}
		return &saved, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}

	fresh := tailcat.NewPrivateKey()
	data, err = json.MarshalIndent(fresh, "", "\t")
	if err != nil {
		return nil, err
	}
	if err := WriteServerKeyData(path, data); err != nil {
		return nil, err
	}
	return fresh, nil
}
