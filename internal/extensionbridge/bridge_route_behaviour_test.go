package extensionbridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

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

					if errors.Is(err, ErrRouteResponseBodyUnsupported) || errors.Is(err, ErrRouteTimesUnsupported) {
						t.Fatalf("behaviour %q sent as %s answered with a transport capability error: %v", spelling, shape.name, err)
					}
				}
			})
		}
	}
}

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
