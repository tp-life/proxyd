package app

// 本文件负责把代理运行态转换为订阅领域可理解的网络降级选项。订阅包只接收一个
// 普通代理 URL，不依赖 App、mihomo Runner 或完整配置，从而保持领域边界单向。

import (
	"fmt"

	"proxyd/internal/proxy/subscribe"
)

// subscriptionFetchOptions 根据当前 mihomo 运行态生成订阅拉取策略。
//
// 参数说明：无；读取 App 的配置快照和代理 Runner 状态。
//
// 返回值说明：subscribe.FetchOptions。只有 Runner 已运行且 mixed-port 有效时，才
// 返回 http://127.0.0.1:<port> 作为备用代理；其余情况返回零值并保持原有直连行为。
//
// 错误情况：本函数不返回错误。端口合法性由配置层统一校验；运行态与配置变更并发
// 时最多得到一次已经过期的回环端口，订阅拉取会把连接失败按正常降级错误处理。
func (a *App) subscriptionFetchOptions() subscribe.FetchOptions {
	a.mu.RLock()
	mixedPort := a.cfg.MixedPort
	a.mu.RUnlock()
	if a.runner == nil || !a.runner.Running() || mixedPort <= 0 || mixedPort > 65535 {
		return subscribe.FetchOptions{}
	}
	return subscribe.FetchOptions{
		FallbackProxyURL: fmt.Sprintf("http://127.0.0.1:%d", mixedPort),
	}
}
