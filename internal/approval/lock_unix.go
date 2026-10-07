//go:build darwin || linux

package approval

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func acquireLock(f *os.File) error {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("exclusive approval store lock: %w", err)
	}
	return nil
}
func releaseLock(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
func syncDirectory(f *os.File) error {
	err := f.Sync()
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	return err
}
