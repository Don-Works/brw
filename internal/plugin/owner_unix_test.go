//go:build !windows

package plugin

import (
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
)

// ownerInfo reports a chosen uid. Creating a file owned by another user needs
// root, so the ownership half of the trust boundary is tested against the
// stat the kernel would have returned.
type ownerInfo struct {
	fs.FileInfo
	sys any
}

func (o ownerInfo) Sys() any { return o.sys }

func TestRefuseForeignOwner(t *testing.T) {
	mine, root := os.Getuid(), 0
	foreign := mine + 1
	if foreign == root {
		foreign = mine + 2
	}
	for name, test := range map[string]struct {
		sys     any
		wantErr bool
	}{
		"the daemon's own user": {sys: &syscall.Stat_t{Uid: uint32(mine)}},
		"root":                  {sys: &syscall.Stat_t{Uid: uint32(root)}},
		"another local user":    {sys: &syscall.Stat_t{Uid: uint32(foreign)}, wantErr: true},
		// A platform that reports no owner leaves the mode as the only check,
		// which is what owner_windows.go does.
		"no owner reported": {sys: nil},
	} {
		t.Run(name, func(t *testing.T) {
			err := refuseForeignOwner(ownerInfo{sys: test.sys}, "plugin directory")
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "rather than by the daemon's user or root") {
					t.Fatalf("refuseForeignOwner = %v, want a refusal naming the owner", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("refuseForeignOwner = %v, want it accepted", err)
			}
		})
	}
}
