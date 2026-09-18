//go:build windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile 对文件首字节加非阻塞排他锁（LockFileEx）；
// 已被其他进程持有时返回 locked=false。关闭句柄即释放锁，无需显式解锁。
func tryLockFile(f *os.File) (locked bool, err error) {
	var overlapped windows.Overlapped
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}
