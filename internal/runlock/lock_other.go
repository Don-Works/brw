//go:build !unix

package runlock

import (
	"errors"
	"os"
)

// ErrUnsupported is returned on a platform where brw has no advisory file lock.
//
// It is a refusal, not a silent pass. A scheduled run that cannot serialise
// itself against the other runs on this profile is exactly the run that must not
// start: the failure it would produce is two runs interleaving on one tab, which
// looks like a flaky site rather than a missing lock.
var ErrUnsupported = errors.New("brw has no run lock on this platform, so a scheduled run cannot be serialised against another run on the same profile")

func tryLockFile(*os.File) (bool, error) { return false, ErrUnsupported }

func unlockFile(*os.File) error { return ErrUnsupported }
