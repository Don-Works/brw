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
		for _, shape := range routeCallShapes() {
			t.Run(string(behaviour)+"/"+shape.name, func(t *testing.T) {
				b := New("", time.Second, "fake")
				for _, spelling := range []string{string(behaviour), strings.ToUpper(string(behaviour)), " " + string(behaviour) + " "} {
					opts := shape.opts
					opts.Behaviour = spelling
					_, err := b.Route(context.Background(), opts)
					if !errors.Is(err, want) {
						t.Fatalf("Route(%s, behaviour=%q) error = %v, want %v", shape.name, spelling, err, want)
					}
					// The transport-specific refusals must not answer for it: those
					// say "use a direct-CDP profile", and there is no profile where
					// this behaviour works.
					if errors.Is(err, ErrRouteResponseBodyUnsupported) || errors.Is(err, ErrRouteTimesUnsupported) {
						t.Fatalf("behaviour %q sent as %s answered with a transport capability error: %v", spelling, shape.name, err)
					}
				}
			})
		}
	}
}

// routeCallShapes is the set of brw_route calls that carry a behaviour.
//
// docs/install.md states the refusal as a property of `brw_route
// {behaviour:"redirect"}` with no qualification, so every shape that can carry
// the word has to reach it. The three beyond a well-formed add are the ones
// that used to reach something else while the check lived in the rule builders:
// an add with no pattern was told to supply one, action=replay was told the
// behaviour does not apply to replay, and a call with no tab was told to open
// one — three different answers to one question.
func routeCallShapes() []struct {
	name string
	opts browser.RouteOptions
} {
	return []struct {
		name string
		opts browser.RouteOptions
	}{
		{"add", browser.RouteOptions{Action: "add", TabID: "42", Pattern: "https://api.example.com/*"}},
		{"add with no pattern", browser.RouteOptions{Action: "add", TabID: "42"}},
		{"replay", browser.RouteOptions{Action: "replay", TabID: "42", HARArtifactID: "har-1"}},
		{"no tab", browser.RouteOptions{Action: "add", Pattern: "https://api.example.com/*"}},
	}
}
