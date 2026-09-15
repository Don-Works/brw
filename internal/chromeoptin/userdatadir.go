package chromeoptin

import (
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

// DefaultUserDataDir resolves where a browser keeps the profile its user is
// signed into, expanded for this machine. It is the same table `brwctl setup`
// binds a bridge profile to, so the opt-in lane and the bridge lane look in the
// same place rather than in two tables that can drift.
//
// An empty result means brw does not know where that browser keeps profiles on
// that OS; the caller has to be told the directory.
func DefaultUserDataDir(goos, browser string) string {
	return profilepolicy.ExpandPath(setup.BrowserUserDataDir(goos, browser))
}
