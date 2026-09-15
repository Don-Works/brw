//go:build unix

package runlock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// tryLockFile takes an exclusive advisory lock without blocking.
//
// flock is the right primitive here rather than a lock file whose existence is
// the lock: the kernel drops it when the holding process dies, so a run killed
// mid-flight does not leave the profile permanently locked and needing a human
// to delete a file. It also associates the lock with the open file description
// rather than the process, so two runs inside one process contend exactly as two
// processes do — which is what makes this testable.
func tryLockFile(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false, nil
	default:
		return false, fmt.Errorf("lock %s: %w", file.Name(), err)
	}
}

func unlockFile(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlock %s: %w", file.Name(), err)
	}
	return nil
}
