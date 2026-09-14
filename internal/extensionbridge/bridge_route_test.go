package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Don-Works/brw/internal/browser"
)

// The bridge drives the user's real signed-in Chrome, where an interception rule
// can refuse a request but is never handed the response. Anything that needs a
// body has to say so by name: a silent passthrough would let an agent believe a
// page was running against a mock while it hit production.
func TestBridgeRouteRefusesBodyBackedRoutesByName(t *testing.T) {
	tests := []struct {
		name string
		opts browser.RouteOptions
		want error
	}{
		{
			name: "explicit fulfill",
			opts: browser.RouteOptions{Action: "add", TabID: "42", Pattern: "https://api.test/*", Behaviour: "fulfill", Body: "{}"},
			want: ErrRouteResponseBodyUnsupported,
		},
		{
			name: "an omitted behaviour means fulfill",
			opts: browser.RouteOptions{Action: "add", TabID: "42", Pattern: "https://api.test/*", Body: "{}"},
			want: ErrRouteResponseBodyUnsupported,
		},
		{
			name: "har replay",
			opts: browser.RouteOptions{
				Action: "replay", TabID: "42", HARArtifactID: "art-1",
				HAR: []browser.HAREntry{{Method: "GET", URL: "https://api.test/a", Status: 200, Body: "{}"}},
			},
			want: ErrRouteResponseBodyUnsupported,
		},
		{
			name: "a match-count limit cannot be enforced declaratively",
			opts: browser.RouteOptions{Action: "add", TabID: "42", Pattern: "https://api.test/*", Behaviour: "abort", Times: 1},
			want: ErrRouteTimesUnsupported,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New("", time.Second, "fake")
			_, err := b.Route(context.Background(), tt.opts)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if !strings.Contains(err.Error(), "extension-bridge transport") {
				t.Fatalf("the refusal does not name the transport: %v", err)
			}
		})
	}
}

// The surface that owns the artifact store asks before it reads: a 32 MiB HAR
// decoded only to be refused is wasted, and an unreachable store would otherwise
// answer a capability question with an artifact error.
func TestBridgeAnswersTheReplayCapabilityWithoutBeingHandedAHAR(t *testing.T) {
	b := New("", time.Second, "fake")
	var replayer browser.RouteReplayer = b
	err := replayer.CheckRouteReplay()
	if !errors.Is(err, ErrRouteResponseBodyUnsupported) {
		t.Fatalf("error = %v, want %v", err, ErrRouteResponseBodyUnsupported)
	}
	// The same error the installation path gives, so an agent never sees two
	// spellings of one gap.
	_, routeErr := b.Route(context.Background(), browser.RouteOptions{Action: "replay", TabID: "42", HARArtifactID: "art-1"})
	if routeErr.Error() != err.Error() {
		t.Fatalf("the capability check says %q and the install says %q", err, routeErr)
	}
}

// routeFakeExtension records the declarativeNetRequest rule sets the daemon
// pushes, which is the only observable the extension side has: the rules live in
// Chrome, not in the daemon.
type routeFakeExtension struct {
	pushes [][]map[string]any
}

func (f *routeFakeExtension) serve(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			ID     string         `json:"id"`
			Type   string         `json:"type"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		result := map[string]any{}
		if msg.Type == "set_routes" {
			rules := []map[string]any{}
			for _, raw := range msg.Params["rules"].([]any) {
				rules = append(rules, raw.(map[string]any))
			}
			f.pushes = append(f.pushes, rules)
			result = map[string]any{"tabId": msg.Params["tabId"], "count": len(rules)}
		}
		reply, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": true, "result": result})
		_ = conn.Write(ctx, websocket.MessageText, reply)
	}
}

// An abort route is the one interception declarativeNetRequest can express, so
// it has to reach Chrome as a real rule carrying the pattern the caller asked
// for — and the tab's complete set has to be re-sent on every change, because
// the extension replaces rather than merges.
func TestBridgeRouteInstallsAndClearsDeclarativeRules(t *testing.T) {
	b := New("", 5*time.Second, "")
	fake, cleanup := connectRouteFakeExtension(t, b)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	added, err := b.Route(ctx, browser.RouteOptions{
		Action: "add", TabID: "12", Pattern: "https://tracker.test/*", Behaviour: "abort",
	})
	if err != nil {
		t.Fatalf("add abort route: %v", err)
	}
	if added.Count != 1 || added.Routes[0].Behaviour != browser.RouteAbort {
		t.Fatalf("unexpected add result: %+v", added)
	}
	if len(fake.pushes) != 1 || len(fake.pushes[0]) != 1 {
		t.Fatalf("the rule never reached the extension: %+v", fake.pushes)
	}
	rule := fake.pushes[0][0]
	if rule["behaviour"] != "abort" {
		t.Fatalf("rule behaviour = %v, want abort", rule["behaviour"])
	}
	if want := browser.RoutePatternRegex("https://tracker.test/*"); rule["regex"] != want {
		t.Fatalf("rule regex = %v, want the shared translation %q", rule["regex"], want)
	}

	second, err := b.Route(ctx, browser.RouteOptions{
		Action: "add", TabID: "12", Pattern: "https://ads.test/*", Behaviour: "abort",
	})
	if err != nil {
		t.Fatalf("add second route: %v", err)
	}
	if second.Count != 2 {
		t.Fatalf("second add reports %d routes, want 2", second.Count)
	}
	if len(fake.pushes) != 2 || len(fake.pushes[1]) != 2 {
		t.Fatalf("the second push did not carry the tab's whole rule set: %+v", fake.pushes)
	}

	listed, err := b.Route(ctx, browser.RouteOptions{Action: "list", TabID: "12"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if listed.Count != 2 {
		t.Fatalf("list reports %d routes, want 2", listed.Count)
	}
	if other, err := b.Route(ctx, browser.RouteOptions{Action: "list", TabID: "13"}); err != nil || other.Count != 0 {
		t.Fatalf("routes leaked to another tab: %+v (%v)", other, err)
	}

	cleared, err := b.Route(ctx, browser.RouteOptions{Action: "clear", TabID: "12", Pattern: "https://ads.test/*"})
	if err != nil {
		t.Fatalf("clear one: %v", err)
	}
	if cleared.Count != 1 || cleared.Routes[0].Pattern != "https://tracker.test/*" {
		t.Fatalf("clearing one pattern left %+v", cleared)
	}
	all, err := b.Route(ctx, browser.RouteOptions{Action: "clear", TabID: "12"})
	if err != nil {
		t.Fatalf("clear all: %v", err)
	}
	if all.Count != 0 {
		t.Fatalf("clear-all left %d routes", all.Count)
	}
	if last := fake.pushes[len(fake.pushes)-1]; len(last) != 0 {
		t.Fatalf("clear-all did not push an empty rule set: %+v", last)
	}
}

// Chrome reuses numeric tab ids, and the extension drops a closed tab's rules.
// A daemon copy that outlived the tab would be re-pushed by the next brw_route
// on the replacement tab, blocking requests for an agent that asked for nothing.
func TestBridgeRouteTableIsDroppedWhenTheTabGoes(t *testing.T) {
	b := New("", 5*time.Second, "")
	_, cleanup := connectRouteFakeExtension(t, b)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := b.Route(ctx, browser.RouteOptions{
		Action: "add", TabID: "12", Pattern: "https://tracker.test/*", Behaviour: "abort",
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	b.invalidateTabState("12")
	listed, err := b.Route(ctx, browser.RouteOptions{Action: "list", TabID: "12"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if listed.Count != 0 {
		t.Fatalf("a closed tab still reports %d routes: %+v", listed.Count, listed.Routes)
	}
}

func connectRouteFakeExtension(t *testing.T, b *Bridge) (*routeFakeExtension, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
	})
	if err != nil {
		srv.Close()
		t.Fatalf("dial bridge: %v", err)
	}
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})

	fake := &routeFakeExtension{}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go fake.serve(serveCtx, conn)
	return fake, func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
}
