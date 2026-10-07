// Package runlock serialises unattended brw runs against one browser profile.
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

// ErrBusy is the refusal when another run holds the profile and the caller was not willing to wait any longer.
var ErrBusy = errors.New("another brw run holds this profile")

// Unidentified is the key Key returns for a daemon that will not say which profile it drives.
const Unidentified = "unidentified"

// Key derives the lock identity from the profile a daemon drives.
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

func normalisePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return filepath.Clean(value)
}

// Lock is a held profile lock.
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

// Release drops the lock.
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

// Dir is the directory locks live in: one per user, outside any repository, and stable across daemon restarts because a lock that moved with the daemon would not serialise anything.
func Dir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve the user cache directory for the run lock: %w", err)
	}
	return filepath.Join(cache, "brw", "runlock"), nil
}

// Acquire takes the profile's lock, waiting up to wait for a run that already holds it.
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

const pollInterval = 200 * time.Millisecond
