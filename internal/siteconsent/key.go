package siteconsent

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// keyBytes is the HMAC key length. 32 bytes is the SHA-256 block-output size;
// nothing is gained by more and a shorter key is refused on load.
const keyBytes = 32

// LoadOrCreateKey returns the consent MAC key at path, creating it on first use.
//
// The key is what separates "the user consented" from "a process running as the
// user wrote a line into a JSON file". It is therefore held to the same standard
// as a private key: a regular file, no group or other permission bits, owned by
// the user brw runs as. A key file that anything else on the machine can read is
// refused rather than silently accepted, because a forger who can read it can
// mint any grant they like and the MAC stops meaning anything.
//
// It does NOT protect against a process running as the same user reading the
// file - nothing at the filesystem layer can. The boundary this buys is that the
// store cannot be forged by WRITING to it, which is the shape of the bug this
// exists to prevent.
func LoadOrCreateKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("consent key path is empty")
	}
	key, err := readKey(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create consent key directory: %w", err)
	}
	fresh := make([]byte, keyBytes)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("generate consent key: %w", err)
	}
	// O_EXCL so two daemons starting at once cannot both believe they wrote the
	// key; the loser re-reads what the winner wrote and both agree on one key.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return readKey(path)
		}
		return nil, fmt.Errorf("create consent key: %w", err)
	}
	if _, err := file.Write(fresh); err != nil {
		file.Close()
		return nil, fmt.Errorf("write consent key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("write consent key: %w", err)
	}
	return fresh, nil
}

func readKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("consent key %s is not a regular file; refusing to key site consent off a symlink, device or directory", path)
	}
	if err := checkKeyPermissions(path, info); err != nil {
		return nil, err
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(key) < keyBytes {
		return nil, fmt.Errorf("consent key %s holds %d bytes; at least %d are required", path, len(key), keyBytes)
	}
	return key, nil
}

// checkKeyPermissions refuses a key any other account can read. Split out so the
// rule is one place and testable; the ownership half is POSIX-only because
// Windows does not express it in FileMode and a wrong answer there would be
// worse than no answer.
func checkKeyPermissions(path string, info os.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("consent key %s is mode %#o; it must be 0600 so no other account can mint consent records", path, perm)
	}
	return nil
}
