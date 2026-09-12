//go:build darwin

package gateway

// macOS root 特权 helper 服务端（docs/adr/0003）：launchd 系统域常驻，
// 监听 /var/run/com.proxyd.gateway.sock（0600 + LOCAL_PEERCRED 属主校验），
// 只执行白名单指令集（pf anchor 应用/清除、IPv4 转发读取与设置、ping）。
// helper 不读业务配置、不连网；日志只含操作类别，不含规则内容。

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// helperLabel 是 helper 的 launchd 服务标识。
	helperLabel = "com.proxyd.gateway-helper"
	// helperPlistPath 是 helper 的 LaunchDaemon plist 路径。
	helperPlistPath = "/Library/LaunchDaemons/com.proxyd.gateway-helper.plist"
	// helperOwnerUIDEnv 是安装链路经 plist 环境变量记录的属主 UID。
	helperOwnerUIDEnv = "PROXYD_GATEWAY_OWNER_UID"
	// gatewayPFAnchor 是本模块独占的 pf anchor 名。
	gatewayPFAnchor = "com.proxyd.gateway"
	// gatewayAnchorFile 是 anchor 规则文件（root 所有，随 pfctl -a -f 载入）。
	gatewayAnchorFile = "/etc/pf.anchors/com.proxyd.gateway"
	// gatewayCombinedPFConf 是在 /etc/pf.conf 基础上追加本模块 anchor 引用后的
	// 完整 pf 配置（pfctl -f 整体载入；anchor 必须被主规则集引用，rdr 才生效）。
	gatewayCombinedPFConf = "/etc/pf.anchors/com.proxyd.gateway.conf"
	// helperForwardOrigPath 记忆 forward.set 首次开启前的 IPv4 转发原值，
	// 供 pf.clear/卸载恢复；/var/run 重启清空，与转发状态重启复位语义一致。
	helperForwardOrigPath = "/var/run/com.proxyd.gateway.forward-orig"
)

// helperRun 执行外部命令；包级变量便于测试替换。
var helperRun = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// darwinHelperOps 是 helperOps 的 macOS 实现；run/writeFile/readFile 可注入测试替身。
type darwinHelperOps struct {
	logf func(format string, args ...any)

	mu          sync.Mutex
	applied     bool   // pf anchor 规则已载入
	forwardOrig string // forward.set 首次开启前的原值（"" 表示未记录）

	run       func(name string, args ...string) (string, error)
	writeFile func(path, content string) error
	readFile  func(path string) (string, error)
}

// newDarwinHelperOps 创建生产实现的执行能力。
func newDarwinHelperOps(logf func(format string, args ...any)) *darwinHelperOps {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &darwinHelperOps{
		logf: logf,
		run:  helperRun,
		writeFile: func(path, content string) error {
			return os.WriteFile(path, []byte(content), 0o644)
		},
		readFile: func(path string) (string, error) {
			data, err := os.ReadFile(path)
			return string(data), err
		},
	}
}

// Applied 报告 pf 规则当前是否已载入（供看门狗判定是否需要超时清理）。
func (o *darwinHelperOps) Applied() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.applied
}

// ApplyPF 载入 pf anchor：写 anchor 文件 → 确保主配置引用本 anchor →
// pfctl 整体载入 → 向 anchor 填充 rdr 规则。
//
// 参数：
//   - anchor: string，RenderPFAnchor 生成的规则文本。
//
// 返回值：error，任一 pfctl 步骤失败时返回。
//
// 错误情况：pf 已启用不算错误；重复 Apply 幂等（anchor 内容整体替换）。
func (o *darwinHelperOps) ApplyPF(anchor string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.writeFile(gatewayAnchorFile, anchor); err != nil {
		return fmt.Errorf("写入 anchor 文件失败: %w", err)
	}
	base, err := o.readFile("/etc/pf.conf")
	if err != nil {
		return fmt.Errorf("读取 /etc/pf.conf 失败: %w", err)
	}
	if err := o.writeFile(gatewayCombinedPFConf, buildGatewayPFConf(base)); err != nil {
		return fmt.Errorf("写入合并 pf 配置失败: %w", err)
	}
	// pf 默认未启用；已启用时 pfctl -e 返回 "pf already enabled"，忽略之。
	if out, err := o.run("/sbin/pfctl", "-e"); err != nil && !strings.Contains(out, "already enabled") {
		return fmt.Errorf("启用 pf 失败: %w", err)
	}
	if _, err := o.run("/sbin/pfctl", "-f", gatewayCombinedPFConf); err != nil {
		return fmt.Errorf("载入 pf 主配置失败: %w", err)
	}
	if _, err := o.run("/sbin/pfctl", "-a", gatewayPFAnchor, "-f", gatewayAnchorFile); err != nil {
		return fmt.Errorf("载入 pf anchor 失败: %w", err)
	}
	o.applied = true
	o.logf("[gateway-helper] pf anchor 已应用")
	return nil
}

// ClearPF 清空本模块 anchor 的全部规则，并恢复被本模块改动的 IPv4 转发原值。
// 规则不存在时 pfctl -F 仍成功，保持幂等。
func (o *darwinHelperOps) ClearPF() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, err := o.run("/sbin/pfctl", "-a", gatewayPFAnchor, "-F", "all"); err != nil {
		return fmt.Errorf("清除 pf anchor 失败: %w", err)
	}
	o.applied = false
	o.logf("[gateway-helper] pf anchor 已清除")
	return o.restoreForwardLocked()
}

// GetForward 读取当前 IPv4 转发状态。
func (o *darwinHelperOps) GetForward() (bool, error) {
	out, err := o.run("/usr/sbin/sysctl", "-n", "net.inet.ip.forwarding")
	if err != nil {
		return false, err
	}
	return parseSysctlBool(out)
}

// SetForward 开关 IPv4 转发；首次开启时把原值记入 /var/run（供恢复与卸载参考）。
func (o *darwinHelperOps) SetForward(enable bool) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	current, err := o.GetForward()
	if err != nil {
		return err
	}
	if enable {
		if o.forwardOrig == "" {
			o.forwardOrig = "0"
			if current {
				o.forwardOrig = "1"
			}
			if err := o.writeFile(helperForwardOrigPath, o.forwardOrig); err != nil {
				o.logf("[gateway-helper] 记录转发原值失败（卸载时将按 0 恢复）: %v", err)
			}
		}
		if current {
			return nil
		}
		return o.setForwardLocked(true)
	}
	return o.restoreForwardLocked()
}

// restoreForwardLocked 把 IPv4 转发恢复到记忆的原值；未记录过则不动作。
// 调用方须持有 o.mu。
func (o *darwinHelperOps) restoreForwardLocked() error {
	if o.forwardOrig == "" {
		if data, err := o.readFile(helperForwardOrigPath); err == nil {
			o.forwardOrig = strings.TrimSpace(data)
		}
	}
	if o.forwardOrig == "" {
		return nil
	}
	return o.setForwardLocked(o.forwardOrig == "1")
}

// setForwardLocked 写 sysctl 转发开关；调用方须持有 o.mu。
func (o *darwinHelperOps) setForwardLocked(enable bool) error {
	value := "0"
	if enable {
		value = "1"
	}
	if _, err := o.run("/usr/sbin/sysctl", "-w", "net.inet.ip.forwarding="+value); err != nil {
		return fmt.Errorf("设置 IPv4 转发失败: %w", err)
	}
	o.logf("[gateway-helper] net.inet.ip.forwarding=%s", value)
	return nil
}

// parseSysctlBool 解析 sysctl -n 的 "0"/"1" 输出。
func parseSysctlBool(out string) (bool, error) {
	switch strings.TrimSpace(out) {
	case "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("无法解析 sysctl 输出 %q", out)
}

// buildGatewayPFConf 在系统 /etc/pf.conf 基础上追加本模块 anchor 引用。
// macOS 上子 anchor 的 rdr 规则只有被主规则集的 rdr-anchor 引用才会生效，
// 因此不能只靠 `pfctl -a <anchor> -f`（那只是在 anchor 内载入规则）；
// 这里沿用 ClashX 增强模式的惯例：保留原配置全部内容（含 com.apple/* anchors），
// 在 rdr-anchor 段追加本模块引用，pfctl -f 整体载入。
//
// 参数：
//   - base: string，/etc/pf.conf 原文。
//
// 返回值：
//   - string：合并后的完整 pf 配置；已含引用时原样返回（幂等）。
//
// 错误情况：无；找不到 com.apple rdr-anchor 段（用户自定义配置）时追加到文件末尾，
// pf 解析器按规则类型归组，追加位置不影响 nat/rdr 语义。
func buildGatewayPFConf(base string) string {
	if strings.Contains(base, gatewayPFAnchor) {
		return base
	}
	reference := "rdr-anchor \"" + gatewayPFAnchor + "\"\n" +
		"load anchor \"" + gatewayPFAnchor + "\" from \"" + gatewayAnchorFile + "\"\n"
	lines := strings.SplitAfter(base, "\n")
	for i, line := range lines {
		if strings.Contains(line, `rdr-anchor "com.apple/*"`) {
			out := make([]string, 0, len(lines)+2)
			out = append(out, lines[:i+1]...)
			out = append(out, reference)
			out = append(out, lines[i+1:]...)
			return strings.Join(out, "")
		}
	}
	if !strings.HasSuffix(base, "\n") {
		base += "\n"
	}
	return base + reference
}

// helperOwnerUID 读取安装链路记录的属主 UID（plist 环境变量）。
//
// 参数：无。
//
// 返回值：
//   - int：允许连接的非 root 属主 UID。
//   - error：环境变量缺失或非法时返回；helper 拒绝在无属主记录的情况下运行。
//
// 错误情况：属主缺失意味着 helper 被手工/异常启动，直接拒绝服务。
func helperOwnerUID() (int, error) {
	raw := strings.TrimSpace(os.Getenv(helperOwnerUIDEnv))
	if raw == "" {
		return -1, fmt.Errorf("缺少属主记录（环境变量 %s）；请通过 proxyd gateway helper install 安装", helperOwnerUIDEnv)
	}
	uid, err := strconv.Atoi(raw)
	if err != nil || uid < 0 {
		return -1, fmt.Errorf("属主 UID %q 非法", raw)
	}
	return uid, nil
}

// listenHelperSocket 创建 helper 监听 socket：清掉残留文件、0600、chown 给属主。
func listenHelperSocket(ownerUID int) (net.Listener, error) {
	_ = os.Remove(helperSocketPath)
	ln, err := net.Listen("unix", helperSocketPath)
	if err != nil {
		return nil, fmt.Errorf("监听 %s 失败: %w", helperSocketPath, err)
	}
	if err := os.Chmod(helperSocketPath, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("设置 socket 权限失败: %w", err)
	}
	if err := os.Chown(helperSocketPath, ownerUID, 0); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("设置 socket 属主失败: %w", err)
	}
	return ln, nil
}

// peerUID 读取 unix socket 对端进程的 UID（LOCAL_PEERCRED）。
//
// 参数：
//   - conn: *net.UnixConn，已接受的连接。
//
// 返回值：
//   - uint32：对端进程有效 UID。
//   - error：内核凭证不可读时返回；调用方必须按拒绝处理。
//
// 错误情况：无法读取对端身份时绝不放行。
func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Xucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Uid, nil
}

// authorizedHelperUID 判定对端 UID 是否允许下发指令：root 或安装时记录的属主。
func authorizedHelperUID(uid uint32, ownerUID int) bool {
	return uid == 0 || uid == uint32(ownerUID)
}

// RunHelperServer 以 root 运行 helper 服务端（由 launchd 系统域托管，
// `proxyd gateway-helper` 内部子命令入口，不应手工调用）。
//
// 参数：
//   - ctx: context.Context，取消后停止监听并尽力清除规则。
//   - logf: func(string, ...any)，操作日志（不含规则内容等敏感信息）。
//
// 返回值：
//   - error：非 root、缺属主记录或监听失败时返回；正常取消返回 nil。
//
// 错误情况：看门狗超时（默认 90s 无心跳）自动清除 pf 规则并恢复转发原值。
func RunHelperServer(ctx context.Context, logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("gateway helper 必须以 root 运行（经 launchd 系统域托管）")
	}
	ownerUID, err := helperOwnerUID()
	if err != nil {
		return err
	}
	ops := newDarwinHelperOps(logf)
	watchdog := newHelperWatchdog(time.Now, helperWatchdogTimeout)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go runHelperWatchdog(watchCtx, watchdog, helperWatchdogInterval, ops.Applied, func() {
		logf("[gateway-helper] 主进程失联超过 %s，自动清除网关规则", helperWatchdogTimeout)
		if err := ops.ClearPF(); err != nil {
			logf("[gateway-helper] 看门狗清除失败: %v", err)
		}
	})

	ln, err := listenHelperSocket(ownerUID)
	if err != nil {
		return err
	}
	defer ln.Close()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	logf("[gateway-helper] 已监听 %s（属主 UID %d）", helperSocketPath, ownerUID)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("helper 接受连接失败: %w", err)
		}
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			_ = conn.Close()
			continue
		}
		uid, err := peerUID(unixConn)
		if err != nil || !authorizedHelperUID(uid, ownerUID) {
			logf("[gateway-helper] 拒绝未授权连接（uid 校验失败）")
			_ = conn.Close()
			continue
		}
		go func() {
			defer conn.Close()
			serveHelperConn(conn, ops, watchdog.beat)
		}()
	}
}
