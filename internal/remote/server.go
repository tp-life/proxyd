package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
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

// autoRegionLocked 返回自动就近模式下的 DERP 区域：首次调用经 Expand 探测并缓存，
// 之后（含配置变更引发的重建）沿用缓存，保证 token 中的区域信息在进程生命周期内稳定。
// DERPMapURL 变化时缓存失效、重新探测。调用方需持有 m.mu。
func (m *Manager) autoRegionLocked(cfg config.RemoteConfig) (*tailcfg.DERPRegion, error) {
	if m.autoRegion != nil && m.autoRegionMapURL == cfg.DERPMapURL {
		return m.autoRegion, nil
	}
	ci := &tailcat.ConnInfo{RegionID: -1} // -1 = 自动探测最近区域
	opts := []any{tailcat.ExpandForServer}
	if cfg.DERPMapURL != "" {
		opts = append(opts, tailcat.DERPMapURL(cfg.DERPMapURL))
	}
	if err := ci.Expand(context.Background(), opts...); err != nil {
		return nil, fmt.Errorf("探测 DERP 区域失败: %w", err)
	}
	m.autoRegion = ci.Region[0]
	m.autoRegionMapURL = cfg.DERPMapURL
	m.logf("[remote] 自动选定 DERP 区域 %d（%s），进程内保持不变", m.autoRegion.RegionID, m.autoRegion.RegionName)
	return m.autoRegion, nil
}

// startServerLocked 按配置启动隧道服务端并刷新 token；调用方需持有 m.mu。
func (m *Manager) startServerLocked(cfg config.RemoteConfig) error {
	priv, err := loadOrCreateNodeKey(m.serverKeyPath(cfg))
	if err != nil {
		return err
	}

	regionID, hosts, err := parseRegion(cfg.Region)
	if err != nil {
		return err
	}

	s := &tailcat.Server{
		Key:        priv,
		Logf:       logger.Logf(m.logf),
		DERPMapURL: cfg.DERPMapURL,
	}
	// 客户端公钥白名单：非空时只有列表内的客户端能完成握手，空则放行所有。
	// 临时身份公钥（temp-key）与白名单叠加生效，去重后统一登记。
	seenKey := map[string]bool{}
	for _, text := range append(allowKeys(cfg.Allow), cfg.TempKey) {
		text = strings.TrimSpace(text)
		if text == "" || seenKey[text] {
			continue
		}
		seenKey[text] = true
		pub, err := ValidateClientKey(text)
		if err != nil {
			return err
		}
		s.AllowedClients = append(s.AllowedClients, pub)
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
		s.RegionID = tailcfg.DERPRegionID(regionID)
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
		servePorts[uint16(p)] = true
		s.ServedTCPPorts = append(s.ServedTCPPorts, filter.PortRange{First: uint16(p), Last: uint16(p)})
	}
	// 内嵌免密 SSH：隧道 22 端口由进程内 SSH 服务器直接处理（隧道即认证，
	// 与 tailcat serve 的 no-auth-ssh 同模型），覆盖对 127.0.0.1:22 的转发，
	// 无需系统 sshd。host key 由 tailcat 库生成于用户配置目录 tailcat/ssh 下。
	var sshHandler func(net.Conn)
	if cfg.BuiltinSSH {
		if !tailcat.SupportsSSHServer() {
			return fmt.Errorf("当前平台不支持内嵌 SSH 服务")
		}
		sshHandler = s.SSHConnHandler(tailcat.SSHOptions{Shell: true})
		if !servePorts[22] {
			s.ServedTCPPorts = append(s.ServedTCPPorts, filter.PortRange{First: 22, Last: 22})
		}
	}
	s.OnTCP = func(port uint16) func(net.Conn) {
		if port == 22 && sshHandler != nil {
			return m.withClientTracking(sshHandler)
		}
		if !servePorts[port] {
			return nil // RST
		}
		return m.withClientTracking(func(c net.Conn) {
			m.serveConn(c, port)
		})
	}

	if err := s.Start(); err != nil {
		return err
	}
	m.srv = s
	m.token = string(s.ConnBlob())
	m.serveErr = ""
	sshNote := ""
	if cfg.BuiltinSSH {
		sshNote = "，内嵌免密 SSH（隧道 22 端口）"
	}
	if strings.TrimSpace(cfg.KeyFile) != "" {
		m.logf("[remote] 隧道服务端已启动，暴露端口 %v%s，密钥文件 %s", cfg.Serve, sshNote, m.serverKeyPath(cfg))
	} else {
		m.logf("[remote] 隧道服务端已启动，暴露端口 %v%s", cfg.Serve, sshNote)
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

// serveConn 把隧道内到来的连接转发到本机回环端口（serve 语义的实现）。
func (m *Manager) serveConn(c net.Conn, port uint16) {
	defer c.Close()
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), 10*time.Second)
	if err != nil {
		m.logf("[remote] 隧道连接 → 127.0.0.1:%d 失败: %v", port, err)
		return
	}
	defer upstream.Close()
	relay(c, upstream)
}

// withClientTracking 为连接处理器叠加「已知客户端（白名单+临时身份）活动连接数」统计；
// 开放模式下的陌生客户端无法反推公钥，直接透传。
func (m *Manager) withClientTracking(h func(net.Conn)) func(net.Conn) {
	return func(c net.Conn) {
		keyText := m.attributeClient(c.RemoteAddr())
		if keyText == "" {
			h(c)
			return
		}
		m.mu.Lock()
		m.activeClients[keyText]++
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			m.activeClients[keyText]--
			m.mu.Unlock()
		}()
		h(c)
	}
}

// allowKeys 提取白名单条目的公钥列表（别名只用于管理展示，不参与握手判定）。
func allowKeys(entries []config.RemoteAllowEntry) []string {
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, e.Key)
	}
	return keys
}

// attributeClient 把入站连接的远端隧道地址归属到已知客户端公钥
// （白名单+临时身份）；开放模式下的陌生客户端无法反推公钥，返回空串。
func (m *Manager) attributeClient(remoteAddr net.Addr) string {
	ap, ok := remoteAddr.(*net.TCPAddr)
	if !ok {
		return ""
	}
	addr, ok := netip.AddrFromSlice(ap.IP)
	if !ok {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	known := knownClientAddrs(allowKeys(m.cfg.Allow), m.cfg.TempKey)
	return known[addr]
}

// relay 双向拷贝两个连接，任一端结束后两边都关闭。
func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
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

// loadOrCreateNodeKey 读取持久化节点密钥；不存在时生成新密钥并以 0600 落盘。
// 服务端密钥决定 token，客户端密钥决定 --allow 白名单身份：文件在则身份稳定。
func loadOrCreateNodeKey(path string) (priv key.NodePrivate, err error) {
	data, err := os.ReadFile(path)
	if err == nil {
		var saved tailcat.PrivateKey
		if jerr := json.Unmarshal(data, &saved); jerr != nil {
			return priv, fmt.Errorf("解析 %s 失败: %w", path, jerr)
		}
		return saved.Private, nil
	}
	if !os.IsNotExist(err) {
		return priv, fmt.Errorf("读取 %s 失败: %w", path, err)
	}

	fresh := tailcat.NewPrivateKey()
	data, err = json.MarshalIndent(fresh, "", "\t")
	if err != nil {
		return priv, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return priv, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return priv, fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return fresh.Private, nil
}
