package main

// serve 单实例守卫：对 state-dir/proxyd.pid 加排他文件锁。
//
// 旧方案只比对 pid 存活（kill(pid,0)），pid 文件在 kill -9/断电后残留，
// PID 被系统复用时会误判存活（stop 误杀无关进程、自启实例静默让位失效）。
// 文件锁随持锁进程退出由内核自动释放，天然免疫 stale pid 与 PID 复用；
// 同时在启动早期持锁，关闭「检查 pid 文件 → 登记」之间的 TOCTOU 窗口。
// 文件一旦创建永不删除：删除后新实例会创建新 inode 持有另一把锁，
// 导致基于旧 inode 的存活判断失效（unlink 竞态）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// instanceLock 持有 pid 文件上的排他锁；进程退出（含崩溃）时内核自动释放。
type instanceLock struct {
	file *os.File
}

// acquireInstanceLock 打开（必要时创建）pid 文件并尝试获取排他锁。
// acquired=false 表示已有实例持有锁（锁由内核保证互斥，与文件内容无关）。
func acquireInstanceLock(path string) (lock *instanceLock, acquired bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, err
	}
	ok, err := tryLockFile(f)
	if err != nil || !ok {
		_ = f.Close()
		if err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	return &instanceLock{file: f}, true, nil
}

// writePID 截断文件并写入当前实例 pid，供 stop/status 定位进程。
func (l *instanceLock) writePID(pid int) error {
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	if _, err := l.file.WriteString(strconv.Itoa(pid) + "\n"); err != nil {
		return err
	}
	return l.file.Sync()
}

// release 关闭文件句柄释放锁；进程异常退出时内核同样会释放，因此无需删除文件。
func (l *instanceLock) release() {
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

// readPIDFile 通过 pid 文件上的排他锁判断实例是否存活：锁可获取说明持锁进程
// 已退出（内核自动放锁），文件内容只是过期信息。存活时返回文件内容中的 pid；
// 锁机制不可用（如权限不足）时退化为 pid + kill(pid,0) 探测的旧行为。
func readPIDFile(path string) (pid int, alive bool) {
	lock, acquired, err := acquireInstanceLock(path)
	if err != nil {
		pid, _ = readPIDFileContent(path)
		return pid, pid > 0 && pidAlive(pid)
	}
	pid, _ = readPIDFileContent(path)
	if acquired {
		lock.release()
		return pid, false
	}
	return pid, true
}

// readPIDFileContent 只读解析 pid 文件内容；缺失/损坏时返回 0。
// 调用方必须结合文件锁判断存活，不能仅凭内容下结论。
func readPIDFileContent(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("pid 文件内容非法: %q", strings.TrimSpace(string(data)))
	}
	return pid, nil
}
