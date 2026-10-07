package siteconsent

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const keyBytes = 32

// LoadOrCreateKey returns the consent MAC key at path, creating it on first use.
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

func checkKeyPermissions(path string, info os.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("consent key %s is mode %#o; it must be 0600 so no other account can mint consent records", path, perm)
	}
	return nil
}
