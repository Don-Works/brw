//go:build !windows

package plugin

import (
	"io/fs"
	"syscall"
)

// fileOwner reports the uid that owns info. The second return is false when the
// platform does not report one, in which case the caller has only the mode to
// go on.
func fileOwner(info fs.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}
