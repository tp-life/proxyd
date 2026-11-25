package desktop

// 本文件生成针对 tailcat 隧道优化的 .rdp 配置文件内容。
//
// 由于系统 RDP 客户端连接到 127.0.0.1:<port> 时默认按本机万兆局域网策略传输，
// 会开启壁纸、窗口拖动动画与高码率无压缩帧，极易打满公网/中继隧道并造成严重卡顿。
// 通过显式写入压缩、带宽自检与画质预设，可显著提升跨网远程桌面的流畅度。

import (
	"fmt"
	"strings"
)

const (
	// QualitySmooth 面向 DERP 中继或高延迟网络，优先保证操作跟手度。
	QualitySmooth = "smooth"
	// QualityBalanced 是默认均衡预设，开启压缩并关闭高耗带宽特效。
	QualityBalanced = "balanced"
	// QualityHigh 面向低延迟直连网络，保留完整视觉特效与 32 位色深。
	QualityHigh = "high"
)

// NormalizeQuality 规范化画质预设文本，空值默认回退 balanced。
func NormalizeQuality(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case QualitySmooth:
		return QualitySmooth
	case QualityHigh:
		return QualityHigh
	default:
		return QualityBalanced
	}
}

// RenderRDPFile 生成不含密码的临时 .rdp 文本，并按画质预设写入网络与渲染优化参数。
func RenderRDPFile(address, username, quality string) string {
	clean := strings.NewReplacer("\r", "", "\n", "")
	address = clean.Replace(strings.TrimSpace(address))
	username = clean.Replace(strings.TrimSpace(username))

	var b strings.Builder
	fmt.Fprintf(&b, "full address:s:%s\r\n", address)
	b.WriteString("prompt for credentials:i:1\r\n")
	if username != "" {
		fmt.Fprintf(&b, "username:s:%s\r\n", username)
	}
	b.WriteString("compression:i:1\r\n")
	b.WriteString("networkautodetect:i:1\r\n")
	b.WriteString("bandwidthautodetect:i:1\r\n")
	b.WriteString("autoreconnection enabled:i:1\r\n")
	b.WriteString("enableworkspacereconnect:i:1\r\n")
	b.WriteString("use multimon:i:0\r\n")

	switch NormalizeQuality(quality) {
	case QualitySmooth:
		b.WriteString("connection type:i:2\r\n")
		b.WriteString("session bpp:i:16\r\n")
		b.WriteString("disable wallpaper:i:1\r\n")
		b.WriteString("disable full window drag:i:1\r\n")
		b.WriteString("disable menu anims:i:1\r\n")
		b.WriteString("disable themes:i:1\r\n")
		b.WriteString("allow font smoothing:i:0\r\n")
		b.WriteString("allow desktop composition:i:0\r\n")
	case QualityHigh:
		b.WriteString("connection type:i:6\r\n")
		b.WriteString("session bpp:i:32\r\n")
		b.WriteString("disable wallpaper:i:0\r\n")
		b.WriteString("disable full window drag:i:0\r\n")
		b.WriteString("disable menu anims:i:0\r\n")
		b.WriteString("disable themes:i:0\r\n")
		b.WriteString("allow font smoothing:i:1\r\n")
	default:
		b.WriteString("connection type:i:3\r\n")
		b.WriteString("session bpp:i:24\r\n")
		b.WriteString("disable wallpaper:i:1\r\n")
		b.WriteString("disable full window drag:i:1\r\n")
		b.WriteString("disable menu anims:i:1\r\n")
		b.WriteString("disable themes:i:0\r\n")
		b.WriteString("allow font smoothing:i:1\r\n")
	}
	return b.String()
}
