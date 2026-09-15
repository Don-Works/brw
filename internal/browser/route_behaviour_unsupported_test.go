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

	for behaviour, want := range UnsupportedRouteBehaviours {
		t.Run(string(behaviour), func(t *testing.T) {
			// Spelled the way an agent would send it, and in the casing the
			// normaliser has to fold, so a check that compares the raw argument
			// cannot pass this.
			for _, spelling := range []string{string(behaviour), strings.ToUpper(string(behaviour)), " " + string(behaviour) + " "} {
				_, err := m.Route(ctx, RouteOptions{
					Action: "add", TabID: tabID, Pattern: "https://api.example.com/*", Behaviour: spelling,
				})
				if !errors.Is(err, want) {
					t.Fatalf("Route(behaviour=%q) error = %v, want %v", spelling, err, want)
				}
			}
			if count := m.routes.count(tabID); count != 0 {
				t.Fatalf("a refused behaviour left %d routes installed", count)
			}
		})
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

// A behaviour cannot be in the refusal table and in the set the backend builds
// rules for; that combination is a rule installed for a behaviour whose refusal
// a reader believes in.
//
// The test above proves the same thing more directly and is the one that would
// catch a real regression. This one needs no browser, so it still runs where
// Chrome is absent and that one skips — which is where a mistyped table entry
// would otherwise go unnoticed until CI had a browser.
func TestUnsupportedRouteBehavioursAreNotAlsoImplemented(t *testing.T) {
	implemented := map[RouteBehaviour]bool{RouteAbort: true, RouteFulfill: true, RouteReplay: true}
	for behaviour := range UnsupportedRouteBehaviours {
		if implemented[behaviour] {
			t.Errorf("%q is both refused by name and implemented", behaviour)
		}
	}
}
