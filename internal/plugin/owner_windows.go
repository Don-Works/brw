//go:build windows

package plugin

import "io/fs"

// fileOwner has no portable answer on Windows: os.FileInfo carries no uid and
// the ACL model does not map onto one. The ownership half of the trust boundary
// is therefore unenforced there, and docs/plugins.md says so.
func fileOwner(fs.FileInfo) (int, bool) { return 0, false }
