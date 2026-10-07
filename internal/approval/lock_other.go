//go:build !darwin && !linux

package approval

import (
	"errors"
	"os"
)

func acquireLock(f *os.File) error {
	return errors.New("approval store requires Linux or macOS file locking")
}
func releaseLock(f *os.File) error   { return nil }
func syncDirectory(f *os.File) error { return f.Sync() }
