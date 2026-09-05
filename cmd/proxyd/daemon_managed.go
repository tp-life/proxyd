package main

import (
	"fmt"
	"path/filepath"
	"time"

	"proxyd/internal/autostart"
	"proxyd/internal/config"
)

// 本文件编排系统托管服务的启动与重启；系统查询由 autostart 基础设施适配器执行。

// managedService 和 inspectService 是只读适配器入口，测试可替换以避免操作本机服务。
var managedService = autostart.Managed
var inspectService = autostart.Inspect

// requestManagedStop 请求旧实例退出；参数为 int PID，返回 error，保留信号错误供调用方处理。
var requestManagedStop = terminate

// managedStart 优先使用同配置的系统服务，避免 start/restart 与开机服务争抢 PID 和端口。
// 参数：cfg 为 *config.Config；cfgPath 为 string 配置路径；restart 为 bool 是否替换当前实例。
// 返回：bool 表示已经接管本次命令，error 表示查询、终止旧实例或等待就绪失败。
// 错误：系统查询失败必须返回，不能降级派生竞争进程；等待上限 60 秒，超时保留系统服务继续启动。
func managedStart(cfg *config.Config, cfgPath string, restart bool) (bool, error) {
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return true, err
	}
	managed, err := managedService(abs)
	if err != nil {
		return true, err
	}
	if !managed {
		return false, nil
	}
	oldPID := 0
	if restart {
		// 重启时只请求旧实例退出；KeepAlive 负责拉起新实例。这里不删除 PID 文件，
		// 因为新进程可能已写入自己的 PID，命令侧删除会破坏其生命周期登记。
		if pid, alive := readPIDFile(pidPath(cfg)); alive {
			oldPID = pid
			if err := requestManagedStop(pid); err != nil {
				return true, fmt.Errorf("请求旧实例退出失败: %w", err)
			}
		}
	}
	fmt.Println("等待系统托管的 proxyd 就绪（最多 60 秒）…")
	return true, waitManagedService(cfg, oldPID, time.Now().Add(time.Minute))
}

// waitManagedService 等待进程归属与健康端点同时满足，拒绝把另一个独立实例的 HTTP 响应当作成功。
// 参数：cfg 为 *config.Config；oldPID 为 int，重启前的 PID，0 表示普通启动；deadline 为 time.Time 截止时刻。
// 返回：error，系统 PID 与配置 PID 一致且健康检查通过时为 nil。
// 错误：服务卸载、外部实例冲突或等待超时；每次系统查询和 HTTP 请求也有独立超时，轮询间隔 300ms。
func waitManagedService(cfg *config.Config, oldPID int, deadline time.Time) error {
	var snapshot autostart.RuntimeStatus
	for time.Now().Before(deadline) {
		snapshot = inspectService()
		if !snapshot.Loaded && snapshot.State != "unknown" {
			return fmt.Errorf("系统服务已卸载：%s", snapshot.Message)
		}
		pid, alive := readPIDFile(pidPath(cfg))
		if alive && pid != oldPID {
			if snapshot.Running && snapshot.PID == pid && healthEndpointResponds(daemonHealthClient, "http://"+cfg.APIListen, cfg.APISecret) {
				fmt.Printf("系统托管的 proxyd 已就绪 (pid %d)\nweb 控制台: http://%s/\n", pid, cfg.APIListen)
				return nil
			}
		}
		// launchd 查询与 PID 文件读取不是原子快照。系统可能恰在两次读取之间启动，
		// 因此不根据单次不一致立即认定双实例，等待后续轮询确认或给出最终超时原因。
		time.Sleep(300 * time.Millisecond)
	}
	if pid, alive := readPIDFile(pidPath(cfg)); alive && pid != oldPID && snapshot.State != "unknown" && pid != snapshot.PID {
		return fmt.Errorf("独立实例 (pid %d) 正在运行，系统服务无法接管；请执行 proxyd restart 完成切换", pid)
	}
	return fmt.Errorf("等待系统服务就绪超时：%s；日志: %s", snapshot.Message, logPathFor(cfg))
}
