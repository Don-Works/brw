package extensionbridge

import (
	"context"
	"errors"

	"github.com/Don-Works/brw/internal/browser"
)

// ErrCookiesUnsupported is returned by the extension-bridge transport for brw_cookies.
var ErrCookiesUnsupported = errors.New("cookie access is not supported on the extension-bridge transport; the extension's security policy blocks cookie CDP methods to protect the signed-in profile — use a direct-CDP profile (or an incognito context there) for brw_cookies")

// Cookies is unsupported on the extension bridge; see ErrCookiesUnsupported.
func (b *Bridge) Cookies(_ context.Context, _ browser.CookieParams) (browser.CookieResult, error) {
	return browser.CookieResult{}, ErrCookiesUnsupported
}
