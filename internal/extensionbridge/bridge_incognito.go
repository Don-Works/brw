package extensionbridge

import (
	"context"
	"errors"

	"github.com/Don-Works/brw/internal/browser"
)

// ErrIncognitoUnsupported is returned by the extension-bridge transport, which drives the user's existing, signed-in Chrome via the debugger protocol.
var ErrIncognitoUnsupported = errors.New("incognito browser contexts are not supported on the extension-bridge transport; use a direct-CDP profile for incognito")

// OpenIncognito is unsupported on the extension bridge; see ErrIncognitoUnsupported.
func (b *Bridge) OpenIncognito(_ context.Context, _ string) (browser.OpenResult, error) {
	return browser.OpenResult{}, ErrIncognitoUnsupported
}

// CloseContext is unsupported on the extension bridge; see ErrIncognitoUnsupported.
func (b *Bridge) CloseContext(_ context.Context, _ string) error {
	return ErrIncognitoUnsupported
}
