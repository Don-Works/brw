package extensionbridge

import "github.com/Don-Works/brw/internal/browser"

// CheckProfileSession answers yes.
func (b *Bridge) CheckProfileSession() error { return nil }

var _ browser.ProfileSessionController = (*Bridge)(nil)
