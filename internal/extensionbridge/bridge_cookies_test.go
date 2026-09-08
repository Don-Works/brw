package extensionbridge

import (
	"context"
	"errors"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// The bridge must refuse brw_cookies with the explicit transport-limitation
// error for every action — the extension's cookie/storage CDP denylist is a
// security boundary protecting the signed-in profile, so a silent no-op or a
// vague failure would be worse than an honest "not here".
func TestBridgeCookiesUnsupportedForAllActions(t *testing.T) {
	b := &Bridge{}
	for _, action := range []string{"list", "set", "delete"} {
		_, err := b.Cookies(context.Background(), browser.CookieParams{Action: action, Name: "sid", Value: "v"})
		if !errors.Is(err, ErrCookiesUnsupported) {
			t.Fatalf("action %q: error = %v, want ErrCookiesUnsupported", action, err)
		}
	}
}
