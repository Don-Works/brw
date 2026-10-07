//go:build darwin || linux

package approval

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func acquireLock(f *os.File) error {
	return lockError(unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB))
}
func releaseLock(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
func syncDirectory(f *os.File) error {
	err := f.Sync()
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	return err
}
