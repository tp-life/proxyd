package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"proxyd/internal/config"
	"proxyd/internal/remote"
)

// 本文件承载「远程连接」（remote/tailcat 隧道）周边模块的编排逻辑。
// 它与代理数据面完全独立：不触发 mihomo 配置重生成，只做配置变更 →
// remote.Manager 调和 → 持久化，失败时回滚到旧配置。

// initRemote 创建远程连接管理器；在 New 中调用。
func (a *App) initRemote() {
	a.remote = remote.NewManager(a.cfg.StateDir, func(format string, args ...any) {
		log.Printf(format, args...)
	})
}

// startRemote 在 Run 启动阶段按当前配置应用一次远程连接状态；失败仅记录日志，
// 不影响代理主功能启动。
func (a *App) startRemote() {
	a.mu.RLock()
	cfg := a.cfg.Remote.Clone()
	a.mu.RUnlock()
	if err := a.remote.Apply(cfg); err != nil {
		log.Printf("[remote] 启动应用配置失败: %v", err)
	}
}

// stopRemote 停止远程连接模块（隧道服务端与全部本地转发）。
func (a *App) stopRemote() {
	a.remote.Close()
}

// RemoteStatus 返回远程连接模块的运行时状态快照。
func (a *App) RemoteStatus() remote.Status {
	return a.remote.Status()
}

// RemoteAPIExposure 返回 Web 终端安全提示所需的管理 API 监听信息。
//
// 参数说明：无。
//
// 返回值说明：string 和 bool，分别为当前 api-listen 原值及其是否为明确回环地址。
//
// 错误情况：无；格式非法或通配监听由 config.IsLoopbackAPIListen 保守判为 false。
func (a *App) RemoteAPIExposure() (string, bool) {
	a.mu.RLock()
	listen := a.cfg.APIListen
	a.mu.RUnlock()
	return listen, config.IsLoopbackAPIListen(listen)
}

// PingRemote 探测一个已保存远端或完整 token 的在线状态、RTT 与连接路径。
//
// 参数说明：
//   - ctx: context.Context，控制 DERP 建连、disco ping 与超时取消。
//   - nameOrToken: string，remotes 中的名称或 tc... 完整 token。
//
// 返回值说明：remote.ProbeResult，成功时包含在线状态、毫秒 RTT 和 direct/derp 路径。
//
// 错误情况：远端名称不存在、token 非法、授权拒绝或网络超时时返回错误。
func (a *App) PingRemote(ctx context.Context, nameOrToken string) (remote.ProbeResult, error) {
	a.mu.RLock()
	remotes := append([]config.RemotePeer(nil), a.cfg.Remote.Remotes...)
	a.mu.RUnlock()
	token, err := remote.ResolveToken(remotes, nameOrToken)
	if err != nil {
		return remote.ProbeResult{}, err
	}
	return a.remote.ProbeRemote(ctx, token)
}

// RemoteAudit 返回 remote 专用连接审计环的最近记录。
//
// 参数说明：
//   - tail: int，期望返回的最大记录数；API/CLI 层负责设置用户输入上限。
//
// 返回值说明：[]remote.AuditEntry，按发生时间从旧到新排列。
//
// 错误情况：无；审计环为空时返回空切片。
func (a *App) RemoteAudit(tail int) []remote.AuditEntry {
	return a.remote.Audit(tail)
}

// OpenRemoteWebTerminal 创建一个已经进入交互 shell 的进程内 SSH/PTY 会话。
//
// 参数说明：
//   - ctx: context.Context，WebSocket 请求生命周期；取消后关闭 shell 与底层传输。
//   - size: remote.TerminalSize，浏览器首次打开时的列数和行数。
//
// 返回值说明：*remote.TerminalSession 和 error；应用层不解释终端字节，只转交 API 适配层。
//
// 错误情况：Web 终端/builtin-ssh 未开启、服务端未运行、平台不支持或 SSH/PTY 初始化失败时返回错误。
func (a *App) OpenRemoteWebTerminal(ctx context.Context, size remote.TerminalSize) (*remote.TerminalSession, error) {
	return a.remote.OpenWebTerminal(ctx, size)
}

// mutateRemote 是远程配置变更的统一事务入口。
//
// 参数说明：
//   - mutate: func(*config.RemoteConfig) error，只修改克隆配置的领域操作。
//
// 返回值说明：error，配置、运行态和磁盘全部提交成功时返回 nil。
//
// 错误情况：领域校验、运行态调和或持久化任一步失败时返回错误；已经应用的内存与
// 运行态会回滚。remoteMutationMu 覆盖整个事务，避免并发请求互相覆盖。
func (a *App) mutateRemote(mutate func(r *config.RemoteConfig) error) error {
	a.remoteMutationMu.Lock()
	defer a.remoteMutationMu.Unlock()
	return a.mutateRemoteLocked(mutate)
}

// mutateRemoteLocked 执行已串行化的远程配置事务；调用方必须持有 remoteMutationMu。
//
// 参数说明：
//   - mutate: func(*config.RemoteConfig) error，只作用于当前配置的独立克隆。
//
// 返回值说明：error，事务提交成功时为 nil。
//
// 错误情况：变更函数、Manager.Apply 或配置落盘失败时返回错误，并尽力恢复旧运行态。
func (a *App) mutateRemoteLocked(mutate func(r *config.RemoteConfig) error) error {
	a.mu.Lock()
	old := a.cfg.Remote.Clone()
	next := old.Clone()
	if err := mutate(&next); err != nil {
		a.mu.Unlock()
		return err
	}
	a.cfg.Remote = next
	applyCfg := next.Clone()
	a.mu.Unlock()

	if err := a.remote.Apply(applyCfg); err != nil {
		return a.rollbackRemote(old, err)
	}

	a.mu.Lock()
	err := a.persistLocked()
	a.mu.Unlock()
	if err != nil {
		return a.rollbackRemote(old, err)
	}
	return nil
}

// rollbackRemote 恢复旧远程配置并重新调和运行态。
//
// 参数说明：
//   - old: config.RemoteConfig，事务开始前的完整配置快照。
//   - cause: error，触发回滚的原始错误。
//
// 返回值说明：error，保留原始错误；若运行态恢复也失败，则合并两个错误。
//
// 错误情况：Manager.Apply 可能因服务端重启失败而产生附加错误。调用方必须持有
// remoteMutationMu，确保回滚期间不会插入另一笔 remote 配置事务。
func (a *App) rollbackRemote(old config.RemoteConfig, cause error) error {
	a.mu.Lock()
	a.cfg.Remote = old
	a.mu.Unlock()
	if rollbackErr := a.remote.Apply(old); rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("恢复旧远程连接运行态失败: %w", rollbackErr))
	}
	return cause
}

// SetRemoteEnabled 热切换远程连接隧道服务端总开关，并把内嵌 SSH 同步到相同状态后持久化。
//
// 参数说明：
//   - enabled: bool，true 同时开启 remote 与 builtin-ssh，false 同时关闭两者。
//
// 返回值说明：error，运行态与配置文件全部提交成功时为 nil。
//
// 错误情况：隧道调和或持久化失败时返回错误，并由统一事务恢复两个开关的旧值。
func (a *App) SetRemoteEnabled(enabled bool) error {
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		setRemoteServiceEnabled(r, enabled)
		return nil
	})
}

// setRemoteServiceEnabled 应用 remote 总开关的联动规则。
//
// 参数说明：
//   - remoteConfig: *config.RemoteConfig，本次配置事务正在修改的独立副本。
//   - enabled: bool，remote 与 builtin-ssh 的共同目标状态。
//
// 返回值说明：无；直接修改传入的事务副本。
//
// 错误情况：无；nil 指针只可能来自编程错误，所有调用点都必须传入有效配置。
func setRemoteServiceEnabled(remoteConfig *config.RemoteConfig, enabled bool) {
	remoteConfig.Enabled = enabled
	remoteConfig.BuiltinSSH = enabled
}

// SetRemoteServe 修改经隧道暴露的本机端口列表并持久化（重建隧道，token 不变）。
func (a *App) SetRemoteServe(ports []int) error {
	if err := config.ValidateRemoteServe(ports); err != nil {
		return err
	}
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		r.Serve = append([]int(nil), ports...)
		return nil
	})
}

// SetRemoteAllow 整体替换客户端授权并持久化；空列表表示用户明确恢复开放模式。
// 条目别名仅用于管理展示；公钥、TTL 和端口范围在提交前完整校验。
func (a *App) SetRemoteAllow(entries []config.RemoteAllowEntry) error {
	if err := config.ValidateRemoteAllow(entries); err != nil {
		return err
	}
	for _, e := range entries {
		if _, err := remote.ValidateClientKey(e.Key); err != nil {
			return err
		}
	}
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		// 深拷贝 Ports/ExpiresAt，避免 HTTP 解码对象在事务提交后仍能修改运行配置。
		r.Allow = config.RemoteConfig{Allow: entries}.Clone().Allow
		r.AllowRestricted = len(entries) > 0
		return nil
	})
}

// pruneExpiredRemoteAllow 清扫过期客户端授权并以完整事务持久化。
//
// 参数说明：
//   - now: time.Time，本轮清扫采用的统一时间边界。
//
// 返回值说明：int 和 error，分别为删除条数及事务错误；没有过期项时不触发落盘。
//
// 错误情况：运行态调和或配置持久化失败时返回错误并回滚。清扫后即使列表为空，
// AllowRestricted 仍保持 true，防止“最后一个授权过期”意外恢复全开放。
func (a *App) pruneExpiredRemoteAllow(now time.Time) (int, error) {
	a.remoteMutationMu.Lock()
	defer a.remoteMutationMu.Unlock()

	a.mu.RLock()
	entries := config.RemoteConfig{Allow: a.cfg.Remote.Allow}.Clone().Allow
	a.mu.RUnlock()
	removed := 0
	for _, entry := range entries {
		if entry.IsExpired(now) {
			removed++
		}
	}
	if removed == 0 {
		return 0, nil
	}
	err := a.mutateRemoteLocked(func(r *config.RemoteConfig) error {
		kept := make([]config.RemoteAllowEntry, 0, len(r.Allow)-removed)
		for _, entry := range r.Allow {
			if !entry.IsExpired(now) {
				kept = append(kept, entry.Clone())
			}
		}
		r.Allow = kept
		r.AllowRestricted = true
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// SetRemoteKeyFile 修改自定义服务端密钥文件路径并持久化；空字符串恢复内置托管密钥。
// 已存在的文件会先经 remote.ValidateKeyFile 校验；切换密钥即更换身份，token 随之改变。
func (a *App) SetRemoteKeyFile(path string) error {
	path = strings.TrimSpace(path)
	if path != "" {
		if err := remote.ValidateKeyFile(path); err != nil {
			return err
		}
	}
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		r.KeyFile = path
		return nil
	})
}

// ExportRemoteServerKey 导出当前实际使用的服务端身份密钥。
//
// 参数说明：无。
//
// 返回值说明：[]byte 和 error，成功时为可直接供 tailcat 使用的完整私钥 JSON。
//
// 错误情况：托管密钥首次生成、文件读取或格式校验失败时返回错误。返回内容属于敏感
// 凭据，只应由专用 API 按用户动作下载，不能混入 RemoteStatus。
func (a *App) ExportRemoteServerKey() ([]byte, error) {
	a.mu.RLock()
	cfg := a.cfg.Remote.Clone()
	a.mu.RUnlock()
	return a.remote.ExportServerKey(cfg)
}

// ImportRemoteServerKey 把 tailcat 私钥导入内置托管位置并立即切换运行身份。
//
// 参数说明：
//   - data: []byte，上传文件的完整 JSON 内容。
//
// 返回值说明：error，密钥文件、内存配置、运行态和配置文件全部提交成功时返回 nil。
//
// 错误情况：内容非法时写盘前返回；文件替换、隧道重建或配置持久化失败时，会恢复
// 原托管文件、原 key-file 配置和原运行身份，避免留下半导入状态。
func (a *App) ImportRemoteServerKey(data []byte) error {
	if err := remote.ValidateServerKeyData(data); err != nil {
		return err
	}
	a.remoteMutationMu.Lock()
	defer a.remoteMutationMu.Unlock()

	managedPath := a.remote.ManagedServerKeyPath()
	oldData, readErr := os.ReadFile(managedPath)
	oldExists := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("备份现有托管密钥失败: %w", readErr)
	}
	if err := remote.WriteServerKeyData(managedPath, data); err != nil {
		return err
	}

	a.mu.Lock()
	oldCfg := a.cfg.Remote.Clone()
	next := oldCfg.Clone()
	// 导入的目标始终是内置托管密钥；清空自定义 key-file 才能让新身份立即生效。
	next.KeyFile = ""
	a.cfg.Remote = next
	a.mu.Unlock()
	if err := a.remote.ReloadServerKey(next); err != nil {
		return a.rollbackRemoteKeyImport(oldCfg, managedPath, oldData, oldExists, err)
	}
	a.mu.Lock()
	persistErr := a.persistLocked()
	a.mu.Unlock()
	if persistErr != nil {
		return a.rollbackRemoteKeyImport(oldCfg, managedPath, oldData, oldExists, persistErr)
	}
	return nil
}

// rollbackRemoteKeyImport 恢复一次失败导入涉及的密钥文件、配置与运行身份。
//
// 参数说明：
//   - oldCfg: config.RemoteConfig，导入前配置。
//   - managedPath: string，内置托管密钥路径。
//   - oldData: []byte，原密钥内容；oldExists=false 时可为空。
//   - oldExists: bool，导入前是否已有托管密钥。
//   - cause: error，触发回滚的原始错误。
//
// 返回值说明：error，原始错误与任何回滚错误的组合。
//
// 错误情况：旧文件恢复或旧隧道身份重建也可能失败；这些错误会被 errors.Join 保留，
// 不会掩盖最初失败原因。调用方必须持有 remoteMutationMu。
func (a *App) rollbackRemoteKeyImport(oldCfg config.RemoteConfig, managedPath string, oldData []byte, oldExists bool, cause error) error {
	errorsToReturn := []error{cause}
	if oldExists {
		if err := remote.WriteServerKeyData(managedPath, oldData); err != nil {
			errorsToReturn = append(errorsToReturn, fmt.Errorf("恢复原托管密钥失败: %w", err))
		}
	} else if err := os.Remove(managedPath); err != nil && !os.IsNotExist(err) {
		errorsToReturn = append(errorsToReturn, fmt.Errorf("移除导入的托管密钥失败: %w", err))
	}
	a.mu.Lock()
	a.cfg.Remote = oldCfg
	a.mu.Unlock()
	if err := a.remote.ReloadServerKey(oldCfg); err != nil {
		errorsToReturn = append(errorsToReturn, fmt.Errorf("恢复原远程连接运行态失败: %w", err))
	}
	return errors.Join(errorsToReturn...)
}

// SetRemoteBuiltinSSH 热切换内嵌免密 SSH 服务并持久化（重建隧道，token 不变）。
// 开启后隧道 22 端口由进程内 SSH 服务器处理（隧道即认证），不再转发系统 sshd。
func (a *App) SetRemoteBuiltinSSH(enabled bool) error {
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		r.BuiltinSSH = enabled
		return nil
	})
}

// SetRemoteWebTerminal 热切换浏览器终端总开关，并对非回环管理地址执行强制确认门。
//
// 参数说明：
//   - enabled: bool，目标开关状态；false 始终允许，用于紧急关闭。
//   - exposureAcknowledged: bool，调用方是否已向用户明确展示“远程获得本机 shell”风险并取得确认。
//
// 返回值说明：error，安全门通过且配置、运行态、磁盘全部提交成功时返回 nil。
//
// 错误情况：api-listen 不是明确回环地址且未确认时拒绝开启；配置落盘失败时沿用
// remote 统一事务回滚，关闭动作不会被安全门阻止。
func (a *App) SetRemoteWebTerminal(enabled, exposureAcknowledged bool) error {
	a.mu.RLock()
	apiListen := a.cfg.APIListen
	a.mu.RUnlock()
	if enabled && !config.IsLoopbackAPIListen(apiListen) && !exposureAcknowledged {
		return fmt.Errorf("api-listen %s 不是回环地址；Web 终端等价于本机 shell，必须显式确认暴露风险", apiListen)
	}
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		r.WebTerminal = enabled
		return nil
	})
}

// RemoteTempKeyPair 返回临时身份的完整密钥对（公钥, 私钥）。
// 未生成时返回错误，提示用户先重置生成。私钥是连接凭据，只应经专用端点透出。
func (a *App) RemoteTempKeyPair() (pub string, priv string, err error) {
	a.mu.RLock()
	stateDir := a.cfg.StateDir
	a.mu.RUnlock()
	priv, pub, err = remote.LoadTempKey(stateDir)
	return pub, priv, err
}

// ResetRemoteTempKey 生成全新临时身份：私钥覆盖落盘（state-dir），公钥经事务
// 写入配置 temp-key（与白名单叠加生效，隧道服务端随之重建）。旧私钥立即失效，
// 手动添加的白名单条目不受影响。返回新公钥。
func (a *App) ResetRemoteTempKey() (string, error) {
	a.mu.RLock()
	stateDir := a.cfg.StateDir
	a.mu.RUnlock()
	pub, err := remote.ResetTempKey(stateDir)
	if err != nil {
		return "", err
	}
	if err := a.mutateRemote(func(r *config.RemoteConfig) error {
		r.TempKey = pub
		return nil
	}); err != nil {
		return "", err
	}
	return pub, nil
}

// AddRemotePeer 新增保存的远端（名称 → token）。
func (a *App) AddRemotePeer(peer config.RemotePeer) error {
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		for _, e := range r.Remotes {
			if e.Name == peer.Name {
				return fmt.Errorf("远端 %q 已存在", peer.Name)
			}
		}
		if err := remote.ValidateToken(peer.Token); err != nil {
			return err
		}
		r.Remotes = append(r.Remotes, peer)
		return nil
	})
}

// DelRemotePeer 删除保存的远端；引用它的转发会在调和时停止并记录日志。
func (a *App) DelRemotePeer(name string) error {
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		for i, e := range r.Remotes {
			if e.Name == name {
				r.Remotes = append(r.Remotes[:i], r.Remotes[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("远端 %q 不存在", name)
	})
}

// AddRemoteForward 新增本地转发并立即生效，返回实际落盘的转发条目。
// Listen 为空或 "auto" 时自动分配候选段内空闲的回环端口（先分配再校验，
// 配置文件里始终保存具体地址）。
func (a *App) AddRemoteForward(f config.RemoteForward) (config.RemoteForward, error) {
	err := a.mutateRemote(func(r *config.RemoteConfig) error {
		for _, e := range r.Forwards {
			if e.Name == f.Name {
				return fmt.Errorf("转发 %q 已存在", f.Name)
			}
		}
		if strings.HasPrefix(f.Remote, "tc") {
			if err := remote.ValidateToken(f.Remote); err != nil {
				return err
			}
		} else {
			found := false
			for _, p := range r.Remotes {
				if p.Name == f.Remote {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("remote %q 不是已保存的远端名称，也不是 tc... token", f.Remote)
			}
		}
		if f.Listen == "" || f.Listen == "auto" {
			listen, err := assignRemoteForwardPort(r.Forwards)
			if err != nil {
				return err
			}
			f.Listen = listen
		}
		if err := checkRemoteForwardPrivileged(f.Listen); err != nil {
			return err
		}
		r.Forwards = append(r.Forwards, f)
		return nil
	})
	return f, err
}

// checkRemoteForwardPrivileged 拦截需要 root 才能监听的特权端口（<1024，
// Windows 无此限制），在创建入口直接给出可操作提示，避免运行时 bind 失败。
func checkRemoteForwardPrivileged(listen string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	norm, err := config.NormalizeRemoteListen(listen)
	if err != nil {
		return err
	}
	_, portStr, err := net.SplitHostPort(norm)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return err
	}
	if port < 1024 {
		return fmt.Errorf("监听端口 %d 是特权端口（<1024），需要 root 权限；请改用 ≥1024 的端口（如 2222）", port)
	}
	return nil
}

// assignRemoteForwardPort 在候选段 10022-10121 中挑选未被现有转发占用、
// 且本机当前可监听的回环端口，返回规范化监听地址；全部不可用时返回错误。
func assignRemoteForwardPort(existing []config.RemoteForward) (string, error) {
	used := map[string]bool{}
	for _, e := range existing {
		if listen, err := config.NormalizeRemoteListen(e.Listen); err == nil {
			used[listen] = true
		}
	}
	for port := 10022; port <= 10121; port++ {
		listen := fmt.Sprintf("127.0.0.1:%d", port)
		if used[listen] {
			continue
		}
		ln, err := net.Listen("tcp", listen)
		if err != nil {
			continue
		}
		_ = ln.Close()
		return listen, nil
	}
	return "", fmt.Errorf("没有可自动分配的空闲端口（候选 10022-10121 均被占用），请显式指定 listen")
}

// SetRemoteForwardEnabled 启停单条转发（配置保留，监听随开关启停）。
func (a *App) SetRemoteForwardEnabled(name string, enabled bool) error {
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		for i := range r.Forwards {
			if r.Forwards[i].Name == name {
				r.Forwards[i].Enabled = &enabled
				return nil
			}
		}
		return fmt.Errorf("转发 %q 不存在", name)
	})
}

// DelRemoteForward 删除转发并停止对应监听。
func (a *App) DelRemoteForward(name string) error {
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		for i, e := range r.Forwards {
			if e.Name == name {
				r.Forwards = append(r.Forwards[:i], r.Forwards[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("转发 %q 不存在", name)
	})
}
