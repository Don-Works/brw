package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

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

func routeBehaviourEnum(t *testing.T) []string {
	t.Helper()
	return routeSchemaEnum(t, "behaviour")
}

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

var errRouteBackendReached = errors.New("the transport was asked to install the route")

type countingRouteController struct {
	fakeController
	routeCalls int
}

func (c *countingRouteController) Route(context.Context, browser.RouteOptions) (browser.RouteResult, error) {
	c.routeCalls++
	return browser.RouteResult{}, errRouteBackendReached
}

func (c *countingRouteController) calls() int { return c.routeCalls }

type bridgeLikeRouteController struct{ countingRouteController }

func (c *bridgeLikeRouteController) CheckRouteReplay() error { return errNoResponseBodies }

type cdpLikeRouteController struct{ countingRouteController }

func (c *cdpLikeRouteController) CheckRouteReplay() error { return nil }

type routeCallCounter interface {
	browser.Controller
	calls() int
}

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
