package app

import (
	"errors"
	"fmt"
	"log"
	"net"
	"runtime"
	"strconv"
	"strings"

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

// mutateRemote 是远程配置变更的统一路径：克隆旧配置 → 应用变更 → 校验 →
// 调和运行态 → 持久化；任一步失败都回滚到旧配置并重新调和。
func (a *App) mutateRemote(mutate func(r *config.RemoteConfig) error) error {
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
func (a *App) rollbackRemote(old config.RemoteConfig, cause error) error {
	a.mu.Lock()
	a.cfg.Remote = old
	a.mu.Unlock()
	if rollbackErr := a.remote.Apply(old); rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("恢复旧远程连接运行态失败: %w", rollbackErr))
	}
	return cause
}

// SetRemoteEnabled 热切换远程连接隧道服务端开关并持久化。
func (a *App) SetRemoteEnabled(enabled bool) error {
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		r.Enabled = enabled
		return nil
	})
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

// SetRemoteAllow 整体替换客户端公钥白名单并持久化；空列表恢复为放行所有客户端。
// 条目别名仅用于管理展示；每个公钥都经 remote.ValidateClientKey 严格解析（nodekey: 形式）。
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
		r.Allow = append([]config.RemoteAllowEntry(nil), entries...)
		return nil
	})
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

// SetRemoteBuiltinSSH 热切换内嵌免密 SSH 服务并持久化（重建隧道，token 不变）。
// 开启后隧道 22 端口由进程内 SSH 服务器处理（隧道即认证），不再转发系统 sshd。
func (a *App) SetRemoteBuiltinSSH(enabled bool) error {
	return a.mutateRemote(func(r *config.RemoteConfig) error {
		r.BuiltinSSH = enabled
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
