//go:build !darwin && !linux && !windows

package sysproxy

func on(string, int) error             { return ErrUnsupported }
func off() error                       { return ErrUnsupported }
func status(string, int) (bool, error) { return false, ErrUnsupported }
