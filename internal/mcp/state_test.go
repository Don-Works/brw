package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/sessionstate"
)

// Fabricated fixture material, low entropy on purpose.
const (
	fixtureStateKey         = "fixture-session-state-key-for-tests-0001"
	fixtureStateCookieName  = "fixture-session-cookie-one"
	fixtureStateCookieValue = "fixture-session-value-one"
)

func seededStateManager(t *testing.T) (*browser.Manager, string) {
	t.Helper()
	store, err := sessionstate.NewStore(sessionstate.Config{
		Root: filepath.Join(t.TempDir(), "state"),
		Key:  []byte(fixtureStateKey),
	})
	if err != nil {
		t.Fatalf("sessionstate.NewStore: %v", err)
	}
	meta, err := store.Save(sessionstate.Snapshot{
		Origins: []string{"https://app.example.test"},
		Cookies: []sessionstate.Cookie{{
			Name: fixtureStateCookieName, Value: fixtureStateCookieValue,
			Domain: "app.example.test", Path: "/",
		}},
	}, sessionstate.SaveOptions{})
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	manager := &browser.Manager{}
	manager.SetSessionStateStore(store)
	return manager, meta.ID
}

// The extension bridge drives the browser the user is personally signed into.
// brw_state must not be offered there at all.
func TestStateToolIsHiddenOnTheExtensionBridge(t *testing.T) {
	for _, tc := range []struct {
		transport string
		want      bool
	}{
		{brwidentity.TransportExtensionBridge, false},
		{brwidentity.TransportDirectCDP, true},
	} {
		t.Run(tc.transport, func(t *testing.T) {
			s := NewWithToolProfile(nil, "all")
			s.SetIdentity(brwidentity.Identity{Transport: tc.transport})
			if got := advertisedNames(s)[stateToolName]; got != tc.want {
				t.Fatalf("transport %q advertises brw_state = %v, want %v", tc.transport, got, tc.want)
			}
		})
	}
}

// The catalogue entry has to advertise what the handler reads, or an agent
// cannot use the parameters that make the guarantees hold.
func TestStateToolAdvertisesItsParameters(t *testing.T) {
	props := toolProperties(t, stateToolName)
	for _, param := range []string{"action", "snapshot_id", "origins", "redact", "ttl_seconds", "context_id"} {
		if _, ok := props[param]; !ok {
			t.Errorf("brw_state does not advertise %q, so an agent cannot pass it", param)
		}
	}
	schema, _ := toolByName(t, stateToolName)["inputSchema"].(map[string]any)
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "action" {
		t.Fatalf("required = %v, want exactly [action]", required)
	}
	// There must be no verb that reads a snapshot back out; the absence of one
	// is what the security argument in docs/auth-model.md rests on.
	actions, _ := props["action"].(map[string]any)
	enum, _ := actions["enum"].([]string)
	allowed := map[string]bool{"save": true, "restore": true, "list": true, "delete": true}
	if len(enum) != len(allowed) {
		t.Fatalf("action enum = %v, want exactly save/restore/list/delete", enum)
	}
	for _, action := range enum {
		if !allowed[action] {
			t.Fatalf("brw_state advertises action %q; a read/export verb would make brw the extraction tool it says it is not", action)
		}
	}
}

// End to end through the real MCP dispatch, a real controller and a real sealed
// store: the response an agent sees carries the handle and the counts, never the
// cookie.
func TestStateToolResponseCarriesNoCookieMaterial(t *testing.T) {
	manager, snapshotID := seededStateManager(t)
	s := New(manager)
	result, rpcErr := s.callTool(context.Background(), stateToolName, json.RawMessage(`{"action":"list"}`))
	if rpcErr != nil {
		t.Fatalf("callTool: %+v", rpcErr)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(encoded)
	if strings.Contains(body, `"isError":true`) {
		t.Fatalf("list returned a tool error: %s", body)
	}
	if !strings.Contains(body, snapshotID) {
		t.Fatalf("the response must name the snapshot so a restore can use it: %s", body)
	}
	for _, secret := range []string{fixtureStateCookieName, fixtureStateCookieValue} {
		if strings.Contains(body, secret) {
			t.Fatalf("the brw_state response emitted %q: %s", secret, body)
		}
	}
}

// A daemon started without a key must refuse by name rather than silently
// behaving as though it had no snapshots.
func TestStateToolRefusesWhenSnapshotsAreNotEnabled(t *testing.T) {
	s := New(&browser.Manager{})
	result, rpcErr := s.callTool(context.Background(), stateToolName, json.RawMessage(`{"action":"list"}`))
	if rpcErr != nil {
		t.Fatalf("callTool: %+v", rpcErr)
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), "--state-key-file") {
		t.Fatalf("the refusal must name the flag that enables snapshots: %s", encoded)
	}
}

// A controller with no session-state capability at all is a different failure
// from one whose store is off, and the message has to say which.
func TestStateToolRefusesATransportWithoutTheCapability(t *testing.T) {
	s := New(stateless{})
	result, rpcErr := s.callTool(context.Background(), stateToolName, json.RawMessage(`{"action":"list"}`))
	if rpcErr != nil {
		t.Fatalf("callTool: %+v", rpcErr)
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), "does not support session snapshots") {
		t.Fatalf("error = %s, want a named capability refusal", encoded)
	}
}

// stateless satisfies browser.Controller by embedding the interface and
// implements no optional capability.
type stateless struct{ browser.Controller }

const stateToolName = "brw_state"
