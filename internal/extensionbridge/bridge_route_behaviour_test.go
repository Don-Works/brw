package extensionbridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// The same table, enumerated against the OTHER backend.
//
// A refusal both transports are supposed to give is exactly the kind that gets
// implemented on one of them: the bridge already answers some behaviours with a
// capability error of its own, so a redirect arriving here could easily come
// back as "not on this transport" — which tells an agent to go and find a
// direct-CDP profile for something that does not exist there either.
func TestUnsupportedRouteBehavioursAreRefusedByNameOnTheExtensionBridge(t *testing.T) {
	for behaviour, want := range browser.UnsupportedRouteBehaviours {
		t.Run(string(behaviour), func(t *testing.T) {
			b := New("", time.Second, "fake")
			for _, spelling := range []string{string(behaviour), strings.ToUpper(string(behaviour)), " " + string(behaviour) + " "} {
				_, err := b.Route(context.Background(), browser.RouteOptions{
					Action: "add", TabID: "42", Pattern: "https://api.example.com/*", Behaviour: spelling,
				})
				if !errors.Is(err, want) {
					t.Fatalf("Route(behaviour=%q) error = %v, want %v", spelling, err, want)
				}
				// The transport-specific refusals must not answer for it: those
				// say "use a direct-CDP profile", and there is no profile where
				// this behaviour works.
				if errors.Is(err, ErrRouteResponseBodyUnsupported) || errors.Is(err, ErrRouteTimesUnsupported) {
					t.Fatalf("behaviour %q answered with a transport capability error: %v", spelling, err)
				}
			}
		})
	}
}
