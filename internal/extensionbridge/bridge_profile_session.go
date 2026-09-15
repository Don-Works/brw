package extensionbridge

import "github.com/Don-Works/brw/internal/browser"

// CheckProfileSession answers yes. The extension bridge drives the Chrome the
// user is personally signed into, which is the single case a recipe declaring
// it needs the installed profile was written for.
//
// Implemented rather than left absent so the capability is decided by a
// transport that says so, not by a type assertion failing. A recipe runner that
// treats a missing method as "probably fine" is the silent downgrade this
// capability exists to prevent.
func (b *Bridge) CheckProfileSession() error { return nil }

var _ browser.ProfileSessionController = (*Bridge)(nil)
