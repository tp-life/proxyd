//go:build !darwin && !linux && !windows

package autostart

func on(Options) error      { return ErrUnsupported }
func off() error            { return ErrUnsupported }
func status() (bool, error) { return false, ErrUnsupported }
