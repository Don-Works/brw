package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
)

func decodeToolJSON(t *testing.T, result any) map[string]any {
	t.Helper()
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", result)
	}
	content, ok := m["content"].([]toolContent)
	if !ok || len(content) == 0 {
		t.Fatalf("result has no toolContent: %+v", m)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(content[0].Text), &out); err != nil {
		t.Fatalf("payload is not JSON: %v (%q)", err, content[0].Text)
	}
	return out
}

func TestBrwIdentityReportsProfile(t *testing.T) {
	srv := New(fakeController{})
	srv.SetIdentity(brwidentity.Identity{
		Workspace:        "brw-example",
		Profile:          "example-profile",
		UserDataDir:      "/fixture/Chromium",
		ProfileDirectory: "Default",
		Mode:             "bridge",
	})

	result, rpcErr := srv.callTool(context.Background(), "brw_identity", json.RawMessage(`{}`))
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	payload := decodeToolJSON(t, result)

	if payload["connected"] != true {
		t.Errorf("connected = %v, want true", payload["connected"])
	}
	if payload["version"] == "" || payload["version"] == nil {
		t.Errorf("version missing: %v", payload["version"])
	}
	id, ok := payload["identity"].(map[string]any)
	if !ok {
		t.Fatalf("identity missing or wrong type: %+v", payload["identity"])
	}
	for field, want := range map[string]string{
		"workspace":         "brw-example",
		"profile":           "example-profile",
		"profile_directory": "Default",
		"mode":              "bridge",
	} {
		if got := id[field]; got != want {
			t.Errorf("identity.%s = %v, want %q", field, got, want)
		}
	}
}

func TestBrwIdentityWorksWithoutBridge(t *testing.T) {
	if !tabAgnosticTools["brw_identity"] {
		t.Fatal("brw_identity must be tab-agnostic so it never blocks on the bridge")
	}
	srv := New(identityPanicController{})
	result, rpcErr := srv.callTool(context.Background(), "brw_identity", json.RawMessage(`{}`))
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	payload := decodeToolJSON(t, result)

	if payload["connected"] != false {
		t.Errorf("connected = %v, want false for empty identity", payload["connected"])
	}
}

func TestBrwIdentityIsAlwaysAdvertised(t *testing.T) {
	for _, profile := range []string{"all", "core"} {
		srv := NewWithToolProfile(fakeController{}, profile)
		found := false
		for _, tl := range srv.advertisedTools() {
			if tl["name"] == "brw_identity" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("brw_identity missing from %q tool profile — an agent could not discover it", profile)
		}
	}
}

type identityPanicController struct {
	fakeController
}

func (identityPanicController) ResolveActiveTabID(context.Context) string {
	panic("brw_identity must not resolve the active tab")
}
