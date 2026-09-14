package mcp

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// errNoResponseBodies stands in for a transport that can refuse a request but
// never supply one, which is what declarativeNetRequest gives the extension
// bridge.
var errNoResponseBodies = errors.New("answering a request from a body is not supported on this transport")

type refusingReplayController struct {
	fakeController
	routeCalls int
}

func (c *refusingReplayController) Route(context.Context, browser.RouteOptions) (browser.RouteResult, error) {
	c.routeCalls++
	return browser.RouteResult{}, errNoResponseBodies
}

func (c *refusingReplayController) CheckRouteReplay() error { return errNoResponseBodies }

func callRouteTool(t *testing.T, ctrl browser.Controller, args map[string]any) string {
	t.Helper()
	input := lineJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": "brw_route", "arguments": args},
	})
	var output bytes.Buffer
	if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	result, ok := parseLineResponse(t, output.Bytes())["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in the reply to brw_route")
	}
	if isError, _ := result["isError"].(bool); !isError {
		t.Fatalf("brw_route succeeded on a transport that cannot replay: %#v", result)
	}
	return result["content"].([]any)[0].(map[string]any)["text"].(string)
}

// A replay on a transport that cannot supply a response body has to report the
// capability, not whatever the artifact read happened to produce. The HAR is up
// to 32 MiB and the artifact store may not even be configured on this side, so
// asking the transport first is also what stops a bad artifact id from
// masquerading as the reason.
func TestRouteReplayReportsTheCapabilityBeforeReadingTheHAR(t *testing.T) {
	tests := []struct {
		name       string
		artifactID string
	}{
		{name: "an artifact id that does not resolve", artifactID: "no-such-artifact"},
		{name: "no artifact id at all"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := &refusingReplayController{}
			args := map[string]any{"action": "replay", "tab_id": "tab-1"}
			if tt.artifactID != "" {
				args["har_artifact_id"] = tt.artifactID
			}
			text := callRouteTool(t, ctrl, args)
			if text != errNoResponseBodies.Error() {
				t.Fatalf("error = %q, want the transport's named capability error %q", text, errNoResponseBodies)
			}
			if ctrl.routeCalls != 0 {
				t.Fatalf("the transport was asked to install the route %d time(s) after refusing the capability", ctrl.routeCalls)
			}
		})
	}
}
