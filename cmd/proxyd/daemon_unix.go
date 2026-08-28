//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// spawnDaemon 派生 detached 子进程执行 serve：setsid 脱离终端，
// stdout/stderr 重定向到日志文件。返回子进程 pid。
func spawnDaemon(exe, cfgPath, logPath string) (int, error) {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return 0, err
	}
	defer devnull.Close()
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer logf.Close()

	proc, err := os.StartProcess(exe, []string{exe, "serve", "-c", cfgPath}, &os.ProcAttr{
		Env:   os.Environ(),
		Files: []*os.File{devnull, logf, logf},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		return 0, err
	}
	pid := proc.Pid
	if err := proc.Release(); err != nil {
		return 0, err
	}
	return pid, nil
}

// pidAlive 用 kill(pid, 0) 探测进程是否存在（EPERM 也算存在）。
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// terminate 发 SIGTERM 请求进程优雅退出。
func terminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
