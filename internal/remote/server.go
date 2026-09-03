package remote

import (
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

// serverKeyRelPath 是服务端密钥在 stateDir 下的相对路径。
// 密钥决定 token：文件在则 token 稳定，删除后下次启动生成全新 token。
const serverKeyRelPath = "remote/server.private.json"

// startServerLocked 按配置启动隧道服务端并刷新 token；调用方需持有 m.mu。
func (m *Manager) startServerLocked(cfg config.RemoteConfig) error {
	priv, err := loadOrCreateNodeKey(filepath.Join(m.stateDir, serverKeyRelPath))
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
	for _, text := range append(append([]string(nil), cfg.Allow...), cfg.TempKey) {
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
	} else {
		s.RegionID = tailcfg.DERPRegionID(regionID)
	}

	servePorts := map[uint16]bool{}
	for _, p := range cfg.Serve {
		servePorts[uint16(p)] = true
		s.ServedTCPPorts = append(s.ServedTCPPorts, filter.PortRange{First: uint16(p), Last: uint16(p)})
	}
	s.OnTCP = func(port uint16) func(net.Conn) {
		if !servePorts[port] {
			return nil // RST
		}
		return func(c net.Conn) {
			m.serveConn(c, port)
		}
	}

	if err := s.Start(); err != nil {
		return err
	}
	m.srv = s
	m.token = string(s.ConnBlob())
	m.serveErr = ""
	m.logf("[remote] 隧道服务端已启动，暴露端口 %v", cfg.Serve)
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
// 同时对已知客户端（白名单+临时身份）按公钥统计活动连接数。
func (m *Manager) serveConn(c net.Conn, port uint16) {
	keyText := m.attributeClient(c.RemoteAddr())
	if keyText != "" {
		m.mu.Lock()
		m.activeClients[keyText]++
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			m.activeClients[keyText]--
			m.mu.Unlock()
		}()
	}
	defer c.Close()
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), 10*time.Second)
	if err != nil {
		m.logf("[remote] 隧道连接 → 127.0.0.1:%d 失败: %v", port, err)
		return
	}
	defer upstream.Close()
	relay(c, upstream)
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
	known := knownClientAddrs(m.cfg.Allow, m.cfg.TempKey)
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
