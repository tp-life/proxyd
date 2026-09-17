//go:build !darwin && !linux && !windows

package autostart

func on(Options) error      { return ErrUnsupported }
func off() error            { return ErrUnsupported }
func status() (bool, error) { return false, ErrUnsupported }

// RootDaemonPlistInstalled 在不支持的平台上恒为 false。
func RootDaemonPlistInstalled() bool { return false }
