package chromeoptin

import (
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

// DefaultUserDataDir resolves where a browser keeps the profile its user is signed into, expanded for this machine.
func DefaultUserDataDir(goos, browser string) string {
	return profilepolicy.ExpandPath(setup.BrowserUserDataDir(goos, browser))
}
