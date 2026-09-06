package core

// 本文件实现可恢复的代理停用；不使用仅清理 TUN 的进程退出接口代替模块停用。
import (
	"github.com/metacubex/mihomo/hub"
	"github.com/metacubex/mihomo/tunnel/statistic"
)

// Suspend 关闭全部代理入口、DNS/TUN 和活动连接，保留磁盘节点与规则配置。
// 参数：无；返回 error，空配置应用失败时返回上游错误。
// Runner 锁防止与 Reload 并发；暂停后再次 Reload 可以恢复，重复调用不重载核心。
func (r *Runner) Suspend() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return nil
	}
	if err := hub.Parse([]byte("mixed-port: 0\nport: 0\nsocks-port: 0\nredir-port: 0\ntproxy-port: 0\nexternal-controller: ''\nlisteners: []\ntun:\n  enable: false\ndns:\n  enable: false\nlog-level: silent\n")); err != nil {
		return err
	}
	statistic.DefaultManager.Range(func(c statistic.Tracker) bool { _ = c.Close(); return true })
	r.started = false
	return nil
}

// Running 返回核心是否已应用配置。参数无；返回 bool；Runner 锁保证与重载一致，无错误。
func (r *Runner) Running() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.started }
