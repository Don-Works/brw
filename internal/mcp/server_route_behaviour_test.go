package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// routeSchemaEnum reads one of the enums brw_route advertises to an agent.
func routeSchemaEnum(t *testing.T, property string) []string {
	t.Helper()
	for _, entry := range tools() {
		if name, _ := entry["name"].(string); name != "brw_route" {
			continue
		}
		schema, ok := entry["inputSchema"].(map[string]any)
		if !ok {
			t.Fatal("brw_route has no input schema")
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatal("brw_route schema has no properties")
		}
		field, ok := properties[property].(map[string]any)
		if !ok {
			t.Fatalf("brw_route schema has no %s property", property)
		}
		values, ok := field["enum"].([]string)
		if !ok {
			t.Fatalf("brw_route %s enum is %T, not a string list", property, field["enum"])
		}
		return values
	}
	t.Fatal("brw_route is not a registered tool")
	return nil
}

// routeBehaviourEnum reads the behaviours brw_route advertises to an agent.
func routeBehaviourEnum(t *testing.T) []string {
	t.Helper()
	return routeSchemaEnum(t, "behaviour")
}

// The advertised enum and the refusal table have to be disjoint.
//
// Either half alone is survivable; the combination is not. A behaviour offered
// in the enum and refused by the backends is a tool that documents a capability
// every call for it rejects, and one refused by the backends while the
// description tells an agent to use it is the same failure read from the other
// end. Enumerating the table rather than checking for "redirect" by hand is what
// makes this hold for the next entry as well.
func TestAdvertisedRouteBehavioursExcludeTheRefusedOnes(t *testing.T) {
	advertised := map[string]bool{}
	for _, value := range routeBehaviourEnum(t) {
		advertised[value] = true
	}
	if len(advertised) == 0 {
		t.Fatal("brw_route advertises no behaviours at all")
	}
	for behaviour := range browser.UnsupportedRouteBehaviours {
		if advertised[string(behaviour)] {
			t.Errorf("brw_route advertises behaviour %q, which both transports refuse by name", behaviour)
		}
	}
}

// errRouteBackendReached is what a stand-in transport answers with. No refusal
// looks like it, so a call that got past this surface is visible in the
// assertion as well as in the call counter.
var errRouteBackendReached = errors.New("the transport was asked to install the route")

// countingRouteController is a transport that installs rules and counts how
// often brw_route dispatched to it.
type countingRouteController struct {
	fakeController
	routeCalls int
}

func (c *countingRouteController) Route(context.Context, browser.RouteOptions) (browser.RouteResult, error) {
	c.routeCalls++
	return browser.RouteResult{}, errRouteBackendReached
}

func (c *countingRouteController) calls() int { return c.routeCalls }

// bridgeLikeRouteController refuses a replay before the HAR is read, the way
// the extension bridge does: declarativeNetRequest can refuse a request but
// never supply one.
type bridgeLikeRouteController struct{ countingRouteController }

func (c *bridgeLikeRouteController) CheckRouteReplay() error { return errNoResponseBodies }

// cdpLikeRouteController accepts a replay, the way direct CDP does, so the HAR
// read is the next thing this surface would do.
type cdpLikeRouteController struct{ countingRouteController }

func (c *cdpLikeRouteController) CheckRouteReplay() error { return nil }

// routeCallCounter is a controller that reports whether brw_route reached it.
type routeCallCounter interface {
	browser.Controller
	calls() int
}

// routeTransports enumerates the transport shapes brw_route dispatches to, by
// the only property that changes what this surface does before it dispatches:
// what the transport says about a replay.
//
// Three states, and a transport can only be in one of them, so a transport
// added later is covered here without an edit — which is the point, because the
// answer to a behaviour brw refuses by name must not depend on which transport
// the daemon happens to be driving.
func routeTransports() []struct {
	name string
	make func() routeCallCounter
} {
	return []struct {
		name string
		make func() routeCallCounter
	}{
		{"no replay capability", func() routeCallCounter { return &countingRouteController{} }},
		{"refuses replay (extension bridge)", func() routeCallCounter { return &bridgeLikeRouteController{} }},
		{"accepts replay (direct CDP)", func() routeCallCounter { return &cdpLikeRouteController{} }},
	}
}

type routeShape struct {
	name string
	args map[string]any
}

// routeArgumentShapes returns the argument shapes one advertised action can
// arrive in.
//
// The switch is driven by the action enum rather than by a literal list, so an
// action brw_route starts advertising and nobody classified here fails the test
// instead of going unchecked. That gap is exactly how action=replay became a
// second route to the wrong answer: it was the one shape neither backend test
// could see, because this surface answered it first.
//
// The missing-pattern and missing-artifact-id rows answer the same today,
// because this surface validates neither. They are here for the next pre-check
// somebody adds above the dispatch — a pattern check, an artifact lookup — which
// has to land below the behaviour check or this test goes red for that shape
// alone, the way action=replay did.
func routeArgumentShapes(t *testing.T, action string) []routeShape {
	t.Helper()
	switch action {
	case "add":
		return []routeShape{
			{"with a pattern", map[string]any{"action": action, "pattern": "https://api.example.com/*"}},
			{"with no pattern", map[string]any{"action": action}},
		}
	case "replay":
		return []routeShape{
			{"with a har artifact id", map[string]any{"action": action, "har_artifact_id": "har-1"}},
			{"with no har artifact id", map[string]any{"action": action}},
		}
	case "list":
		return []routeShape{{"plain", map[string]any{"action": action}}}
	case "clear":
		return []routeShape{
			{"with a pattern", map[string]any{"action": action, "pattern": "https://api.example.com/*"}},
			{"with no pattern", map[string]any{"action": action}},
		}
	default:
		t.Fatalf("brw_route advertises action %q, which this test does not classify; add its argument shapes here, because a behaviour brw refuses by name must reach the same refusal through every shape the word can arrive in", action)
		return nil
	}
}

// A behaviour brw refuses by name has to be refused at the tool surface, for
// every shape and every transport.
//
// docs/install.md states the refusal with no qualification: the behaviour is
// checked at the tool surface, before the action, the pattern, the tab and the
// HAR a replay names. The backends check it before their own action switch, but
// brw_route ran its own pre-checks first, so {action:"replay",
// behaviour:"redirect"} never reached a backend at all — on the extension bridge
// it came back as "use a direct-CDP profile", for a behaviour no profile has,
// and on direct CDP as an artifact-store error. A green backend test is at the
// wrong layer for that, because no agent calls a backend.
//
// The call counter is the half that makes this a wiring test rather than a
// restatement: the refusal must come from this surface, before dispatch, so
// moving the check back into the transports fails here even though each
// transport still answers correctly on its own.
//
// The tab rows are the two halves of callTool's tab handling: an explicit
// tab_id is pinned onto the context, an absent one sends the call through
// pinActiveTabForTool first. Either way the refusal has to be the same answer.
func TestRefusedRouteBehavioursAnswerTheSameThroughEveryRouteToolShape(t *testing.T) {
	actions := routeSchemaEnum(t, "action")
	if len(actions) == 0 {
		t.Fatal("brw_route advertises no actions at all")
	}
	if len(browser.UnsupportedRouteBehaviours) == 0 {
		t.Fatal("no refused behaviours to check")
	}
	for behaviour, want := range browser.UnsupportedRouteBehaviours {
		for _, action := range actions {
			for _, shape := range routeArgumentShapes(t, action) {
				for _, tab := range []struct{ name, id string }{{"tab given", "tab-1"}, {"no tab", ""}} {
					for _, transport := range routeTransports() {
						name := strings.Join([]string{string(behaviour), action, shape.name, tab.name, transport.name}, "/")
						t.Run(name, func(t *testing.T) {
							// In the casings the normaliser has to fold as well
							// as the plain word, so a surface check that compares
							// the raw argument cannot pass this.
							for _, spelling := range []string{string(behaviour), strings.ToUpper(string(behaviour)), " " + string(behaviour) + " "} {
								ctrl := transport.make()
								args := map[string]any{"behaviour": spelling}
								for key, value := range shape.args {
									args[key] = value
								}
								if tab.id != "" {
									args["tab_id"] = tab.id
								}
								text := callRouteTool(t, ctrl, args)
								if text != want.Error() {
									t.Fatalf("brw_route(behaviour=%q) answered %q, want the named refusal %q", spelling, text, want)
								}
								if ctrl.calls() != 0 {
									t.Fatalf("brw_route dispatched to the transport %d time(s) for a behaviour brw refuses by name; the refusal has to be this surface's answer, not one transport's", ctrl.calls())
								}
							}
						})
					}
				}
			}
		}
	}
}
