//go:build !unix

package runlock

import (
	"errors"
	"os"
)

// ErrUnsupported is returned on a platform where brw has no advisory file lock.
var ErrUnsupported = errors.New("brw has no run lock on this platform, so a scheduled run cannot be serialised against another run on the same profile")

func tryLockFile(*os.File) (bool, error) { return false, ErrUnsupported }

func unlockFile(*os.File) error { return ErrUnsupported }
