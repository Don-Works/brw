package extensionbridge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// The bridge refuses session snapshots as POLICY, so the refusal has to name
// the reason and point at the transport that does support it. A silent empty
// result would read as "this profile has no sessions worth sealing".
func TestBridgeRefusesSessionStateByName(t *testing.T) {
	b := New("127.0.0.1:0", 0, "")
	for _, action := range []string{"save", "restore", "list", "delete"} {
		result, err := b.SessionState(context.Background(), browser.SessionStateOptions{Action: action})
		if !errors.Is(err, ErrSessionStateUnsupported) {
			t.Fatalf("action %q: err = %v, want ErrSessionStateUnsupported", action, err)
		}
		if result.Action != "" || result.Snapshot != nil || len(result.Snapshots) != 0 {
			t.Fatalf("action %q returned a partial result alongside the refusal: %+v", action, result)
		}
	}
	message := ErrSessionStateUnsupported.Error()
	for _, want := range []string{"extension-bridge transport", "direct-CDP profile"} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal does not mention %q: %s", want, message)
		}
	}
}
