//go:build windows

package plugin

import "io/fs"

func fileOwner(fs.FileInfo) (int, bool) { return 0, false }
