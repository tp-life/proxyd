//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// tryLockFile 对文件加非阻塞排他 flock；已被其他进程持有时返回 locked=false。
func tryLockFile(f *os.File) (locked bool, err error) {
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return false, err
}
