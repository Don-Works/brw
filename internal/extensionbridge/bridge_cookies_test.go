package extensionbridge

import (
	"context"
	"errors"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

func TestBridgeCookiesUnsupportedForAllActions(t *testing.T) {
	b := &Bridge{}
	for _, action := range []string{"list", "set", "delete"} {
		_, err := b.Cookies(context.Background(), browser.CookieParams{Action: action, Name: "sid", Value: "v"})
		if !errors.Is(err, ErrCookiesUnsupported) {
			t.Fatalf("action %q: error = %v, want ErrCookiesUnsupported", action, err)
		}
	}
}
