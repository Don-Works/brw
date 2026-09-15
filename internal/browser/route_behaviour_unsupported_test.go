package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Every behaviour brw refuses by name has to be refused by the DIRECT-CDP route
// path, not by a helper next to it: the refusal only means anything where a rule
// would otherwise be installed.
//
// The loop is over the table rather than over a literal list, so a behaviour
// added to UnsupportedRouteBehaviours without a refusal in buildRoute fails here
// instead of shipping as a rule that silently does nothing.
func TestUnsupportedRouteBehavioursAreRefusedByNameOnDirectCDP(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tabID := routeFixtureTab(t, m, ctx)

	// Every shape that can carry a behaviour, not just a well-formed add:
	// docs/install.md states the refusal with no qualification, and while the
	// check lived in buildRoute an add with no pattern and an action=replay
	// each reached a different error that pointed the caller at another call.
	shapes := []struct {
		name string
		opts RouteOptions
	}{
		{"add", RouteOptions{Action: "add", Pattern: "https://api.example.com/*"}},
		{"add with no pattern", RouteOptions{Action: "add"}},
		{"replay", RouteOptions{Action: "replay", HARArtifactID: "har-1"}},
		{"clear", RouteOptions{Action: "clear", Pattern: "https://api.example.com/*"}},
	}
	for behaviour, want := range UnsupportedRouteBehaviours {
		for _, shape := range shapes {
			t.Run(string(behaviour)+"/"+shape.name, func(t *testing.T) {
				// Spelled the way an agent would send it, and in the casing the
				// normaliser has to fold, so a check that compares the raw argument
				// cannot pass this.
				for _, spelling := range []string{string(behaviour), strings.ToUpper(string(behaviour)), " " + string(behaviour) + " "} {
					opts := shape.opts
					opts.TabID = tabID
					opts.Behaviour = spelling
					_, err := m.Route(ctx, opts)
					if !errors.Is(err, want) {
						t.Fatalf("Route(%s, behaviour=%q) error = %v, want %v", shape.name, spelling, err, want)
					}
				}
				if count := m.routes.count(tabID); count != 0 {
					t.Fatalf("a refused behaviour left %d routes installed", count)
				}
			})
		}
	}
}

// The refusal has to say why brw does not do this, not merely that it does not.
// An agent told "unsupported" retries on the other transport; one told the
// reason picks fulfill.
func TestRedirectRefusalNamesBothTransportsAndAnAlternative(t *testing.T) {
	message := ErrRouteRedirectUnsupported.Error()
	for _, want := range []string{"declarativeNetRequest", "host permissions", "initiator", "Fetch.continueRequest", "containment", "fulfill"} {
		if !strings.Contains(message, want) {
			t.Errorf("the redirect refusal never mentions %q: %s", want, message)
		}
	}
	if strings.HasSuffix(message, ".") {
		t.Errorf("error string ends with punctuation: %s", message)
	}
	if first := message[:1]; first != strings.ToLower(first) {
		t.Errorf("error string does not start lowercase: %s", message)
	}
}

// A behaviour in the refusal table must not also be one the rule builder can
// build; that combination is a rule installed for a behaviour whose refusal a
// reader believes in.
//
// Asked of buildRoute rather than of a literal list of the behaviours this file
// believes are implemented, because a list restates the table instead of
// checking it: it would stay green for a `case RouteRedirect:` added to the
// switch below it. The test above proves the same thing end to end and is the
// one that would catch a real regression; this one needs no browser, so it
// still runs where Chrome is absent and that one skips.
func TestUnsupportedRouteBehavioursBuildNoRule(t *testing.T) {
	for behaviour := range UnsupportedRouteBehaviours {
		t.Run(string(behaviour), func(t *testing.T) {
			route, err := buildRoute(RouteOptions{Pattern: "https://api.example.com/*", Behaviour: string(behaviour)})
			if err == nil {
				t.Fatalf("buildRoute turned %q into a rule (%+v); it is refused by name in UnsupportedRouteBehaviours", behaviour, route)
			}
		})
	}
}
