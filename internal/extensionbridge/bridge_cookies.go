package extensionbridge

import (
	"context"
	"errors"

	"github.com/Don-Works/brw/internal/browser"
)

// ErrCookiesUnsupported is returned by the extension-bridge transport for
// brw_cookies. The bridge drives the user's existing, signed-in Chrome, and the
// extension's security policy hard-blocks every cookie/storage CDP method — a
// rogue server that answered the extension's outbound socket must not be able
// to exfiltrate HttpOnly cookies or scrub the signed-in profile's auth state
// through brw. Cookie work needs a dedicated direct-CDP profile (or an
// incognito context there). Returning an explicit error instead of a silent
// no-op keeps the failure honest, exactly like OpenIncognito.
var ErrCookiesUnsupported = errors.New("cookie access is not supported on the extension-bridge transport; the extension's security policy blocks cookie CDP methods to protect the signed-in profile — use a direct-CDP profile (or an incognito context there) for brw_cookies")

// Cookies is unsupported on the extension bridge; see ErrCookiesUnsupported.
func (b *Bridge) Cookies(_ context.Context, _ browser.CookieParams) (browser.CookieResult, error) {
	return browser.CookieResult{}, ErrCookiesUnsupported
}
