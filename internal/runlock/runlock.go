// Package runlock serialises unattended brw runs against one browser profile.
//
// A scheduler fires on a clock, and clocks overlap: a nightly job that usually
// takes four minutes takes twenty on the night the site is slow, and the next
// morning's run starts while it is still going. Both drive the same Chrome
// profile, and their steps interleave on one tab — the second run's navigation
// lands under the first run's click, and the trace of each contains the other's
// actions. Nothing in either run reports it; they simply both fail oddly, or
// worse, one of them succeeds against the other's page.
//
// The lock is keyed by the PROFILE, not by the daemon. Two daemons can drive one
// profile — an --upstream-http MCP proxy in front of a bridge daemon is exactly
// that — so a lock held per daemon URL, per port, or per process would let those
// two interleave while looking perfectly serialised from each side.
package runlock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/brwidentity"
)

// ErrBusy is the refusal when another run holds the profile and the caller was
// not willing to wait any longer. It is a named error because "another run has
// this browser" and "this run failed" need different answers from a scheduler:
// the first is worth trying again on the next tick, the second is not.
var ErrBusy = errors.New("another brw run holds this profile")

// Unidentified is the key Key returns for a daemon that will not say which
// profile it drives. Every such daemon shares this one lock, which serialises
// them against each other but NOT against an identified daemon on the same
// browser: that one takes the profile's own key, and the two interleave on one
// tab while each reports a lock key. The key alone therefore cannot carry the
// guarantee, so `brw run` reports it — `"lock_shared": true` in the run object
// and a line on stderr — rather than claiming one it does not have. It is not
// a refusal: a daemon started without --workspace/--profile is the default
// install, and a scheduled job that cannot start at all is worse than one that
// says which guarantee it got. It is exported so that report is written against
// the same constant the key comes from.
const Unidentified = "unidentified"

// Key derives the lock identity from the profile a daemon drives.
//
// Only the four fields that name a browser profile take part. Mode, Transport,
// Headless and the certificate policy are properties of the daemon answering,
// not of the profile: a bridge daemon and the disposable proxy in front of it
// report different values for all four and drive the same Chrome, so including
// any of them would hand each a lock of its own.
func Key(identity brwidentity.Identity) string {
	fields := []string{
		strings.TrimSpace(identity.Workspace),
		strings.TrimSpace(identity.Profile),
		normalisePath(identity.UserDataDir),
		strings.TrimSpace(identity.ProfileDirectory),
	}
	if strings.TrimSpace(strings.Join(fields, "")) == "" {
		return Unidentified
	}
	digest := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return hex.EncodeToString(digest[:16])
}

// normalisePath makes two spellings of one directory the same key. A trailing
// separator, or a "." segment, is not a different profile.
func normalisePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return filepath.Clean(value)
}

// Lock is a held profile lock. Release is safe to call more than once.
type Lock struct {
	path     string
	key      string
	file     *os.File
	released bool
}

// Path is the lock file this lock holds, for a diagnostic line.
func (l *Lock) Path() string { return l.path }

// Key is the profile key this lock covers.
func (l *Lock) Key() string { return l.key }

// Release drops the lock. The file is left behind on purpose: removing it races
// another process that has already opened it and is waiting on the lock, which
// would hand the lock to two runs at once.
func (l *Lock) Release() error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	if err := unlockFile(l.file); err != nil {
		l.file.Close()
		return err
	}
	return l.file.Close()
}

// Dir is the directory locks live in: one per user, outside any repository, and
// stable across daemon restarts because a lock that moved with the daemon would
// not serialise anything.
func Dir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve the user cache directory for the run lock: %w", err)
	}
	return filepath.Join(cache, "brw", "runlock"), nil
}

// Acquire takes the profile's lock, waiting up to wait for a run that already
// holds it. A zero wait refuses immediately with ErrBusy. Cancelling ctx stops
// waiting.
func Acquire(ctx context.Context, dir, key string, wait time.Duration) (*Lock, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("run lock needs a profile key")
	}
	if dir == "" {
		resolved, err := Dir()
		if err != nil {
			return nil, err
		}
		dir = resolved
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the run lock directory: %w", err)
	}
	path := filepath.Join(dir, key+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the run lock %s: %w", path, err)
	}

	deadline := time.Now().Add(wait)
	for attempt := 0; ; attempt++ {
		locked, err := tryLockFile(file)
		if err != nil {
			file.Close()
			return nil, err
		}
		if locked {
			return &Lock{path: path, key: key, file: file}, nil
		}
		if attempt == 0 && wait <= 0 {
			file.Close()
			return nil, ErrBusy
		}
		if time.Now().After(deadline) {
			file.Close()
			return nil, fmt.Errorf("%w (waited %s for %s)", ErrBusy, wait, path)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// pollInterval is how often a waiting run re-tries the lock. A run takes
// minutes, so polling more often than this buys nothing.
const pollInterval = 200 * time.Millisecond
