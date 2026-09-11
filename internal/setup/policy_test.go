package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/profilepolicy"
)

func bridgeRequest() PolicyRequest {
	return PolicyRequest{
		Workspace: "brw-chrome-profile",
		Profile:   "chrome-profile",
		Browser:   BrowserChrome,
		Transport: TransportBridge,
		BRWDPath:  "/opt/brw/bin/brwd",
		HTTPPort:  DefaultHTTPPort,
		GOOS:      "darwin",
	}
}

// TestMergeFromZeroConfigResolves is the first-time-user case: nothing on disk,
// and the generated policy must satisfy the two resolutions that previously
// failed with "--transport is required when workspace has no default_transport"
// and "transport \"local\" is not allowed by workspace policy".
func TestMergeFromZeroConfigResolves(t *testing.T) {
	req := bridgeRequest()
	policy, changes := Merge(profilepolicy.Policy{}, req)
	if !Changed(changes) {
		t.Fatal("merging into an empty policy must report edits")
	}

	profile, err := policy.ResolveProfile(req.Workspace, "")
	if err != nil {
		t.Fatalf("ResolveProfile with no explicit profile: %v", err)
	}
	if profile.Name != req.Profile {
		t.Fatalf("resolved profile = %q, want %q", profile.Name, req.Profile)
	}
	if !profile.ExtensionBridgeAllowed || profile.DirectCDPAllowed {
		t.Fatalf("bridge profile lanes wrong: bridge=%v direct=%v", profile.ExtensionBridgeAllowed, profile.DirectCDPAllowed)
	}
	if profile.BridgeExtensionID != profilepolicy.DefaultBridgeExtensionID {
		t.Fatalf("bridge_extension_id = %q, want the published id", profile.BridgeExtensionID)
	}
	if profile.BridgeHTTPAddr != "127.0.0.1:17310" || profile.BridgeWSAddr != "127.0.0.1:17311" {
		t.Fatalf("bridge addrs = %q / %q", profile.BridgeHTTPAddr, profile.BridgeWSAddr)
	}

	transport, err := policy.ResolveTransport(req.Workspace, "")
	if err != nil {
		t.Fatalf("ResolveTransport with no explicit transport: %v", err)
	}
	if transport.Name != LocalTransportName || transport.Kind != "stdio" {
		t.Fatalf("resolved transport = %+v", transport)
	}
	if transport.Command != req.BRWDPath {
		t.Fatalf("transport command = %q, want the absolute brwd path %q", transport.Command, req.BRWDPath)
	}
}

// TestMergeIsIdempotent locks the re-run contract: a second merge of the same
// request reports no edits and produces byte-identical JSON.
func TestMergeIsIdempotent(t *testing.T) {
	req := bridgeRequest()
	first, _ := Merge(profilepolicy.Policy{}, req)
	second, changes := Merge(first, req)
	if Changed(changes) {
		t.Fatalf("re-running setup edited an already-complete policy: %+v", changes)
	}
	firstJSON, err := EncodePolicy(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := EncodePolicy(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("idempotent merge changed the policy:\n%s\n---\n%s", firstJSON, secondJSON)
	}
}

func TestMergeExistingPolicy(t *testing.T) {
	// A hand-written policy in exactly the shape that made mcp-config fail:
	// bindings that name a profile but no transport, and no transports array.
	handWritten := profilepolicy.Policy{
		WorkspaceBindings: []profilepolicy.WorkspaceBinding{{
			Workspace:       "brw-chromium",
			DefaultProfile:  "chromium-profile",
			AllowedProfiles: []string{"chromium-profile"},
		}},
		Profiles: []profilepolicy.Profile{{
			Name:                   "chromium-profile",
			Description:            "hand written",
			UserDataDir:            "~/Library/Application Support/Chromium",
			ProfileDirectory:       "Profile 1",
			ExtensionBridgeAllowed: true,
			BridgeHTTPAddr:         "127.0.0.1:17410",
			BridgeWSAddr:           "127.0.0.1:17411",
		}},
	}

	cases := []struct {
		name        string
		request     PolicyRequest
		wantEdits   bool
		wantDetails []string
		check       func(t *testing.T, got profilepolicy.Policy)
	}{
		{
			name: "fills the missing transport without touching the profile",
			request: PolicyRequest{
				Workspace: "brw-chromium",
				Profile:   "chromium-profile",
				Browser:   BrowserChromium,
				Transport: TransportBridge,
				BRWDPath:  "/opt/brw/bin/brwd",
				HTTPPort:  17410,
				GOOS:      "darwin",
			},
			wantEdits: true,
			wantDetails: []string{
				`add stdio transport "local" running /opt/brw/bin/brwd`,
				`profile "chromium-profile" already defined`,
				`set workspace "brw-chromium" default_transport to "local"`,
			},
			check: func(t *testing.T, got profilepolicy.Policy) {
				profile, err := got.Find("chromium-profile")
				if err != nil {
					t.Fatal(err)
				}
				if profile.Description != "hand written" || profile.ProfileDirectory != "Profile 1" {
					t.Fatalf("existing profile was rewritten: %+v", profile)
				}
				if profile.BridgeHTTPAddr != "127.0.0.1:17410" {
					t.Fatalf("existing bridge addr was rewritten: %q", profile.BridgeHTTPAddr)
				}
				if _, err := got.ResolveTransport("brw-chromium", ""); err != nil {
					t.Fatalf("workspace still cannot resolve a transport: %v", err)
				}
			},
		},
		{
			name: "a second browser is added alongside the first",
			request: PolicyRequest{
				Workspace: "brw-chrome-profile",
				Profile:   "chrome-profile",
				Browser:   BrowserChrome,
				Transport: TransportBridge,
				BRWDPath:  "/opt/brw/bin/brwd",
				HTTPPort:  DefaultHTTPPort,
				GOOS:      "darwin",
			},
			wantEdits: true,
			check: func(t *testing.T, got profilepolicy.Policy) {
				if len(got.Profiles) != 2 {
					t.Fatalf("profiles = %d, want the original plus the new one", len(got.Profiles))
				}
				if len(got.WorkspaceBindings) != 2 {
					t.Fatalf("bindings = %d, want the original plus the new one", len(got.WorkspaceBindings))
				}
				if _, err := got.ResolveProfile("brw-chromium", ""); err != nil {
					t.Fatalf("original workspace stopped resolving: %v", err)
				}
			},
		},
		{
			name: "a direct-CDP lane gets its own brw-owned profile directory",
			request: PolicyRequest{
				Workspace: "brw-chrome-agent",
				Profile:   "chrome-agent",
				Browser:   BrowserChrome,
				Transport: TransportDirectCDP,
				BRWDPath:  "/opt/brw/bin/brwd",
				GOOS:      "darwin",
			},
			wantEdits: true,
			check: func(t *testing.T, got profilepolicy.Policy) {
				profile, err := got.Find("chrome-agent")
				if err != nil {
					t.Fatal(err)
				}
				if !profile.DirectCDPAllowed || profile.ExtensionBridgeAllowed {
					t.Fatalf("direct-cdp profile lanes wrong: %+v", profile)
				}
				if profile.UserDataDir != "~/.brw/chrome-agent" {
					t.Fatalf("direct-cdp profile must not point at the user's real browser dir: %q", profile.UserDataDir)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changes := Merge(handWritten, tc.request)
			if Changed(changes) != tc.wantEdits {
				t.Fatalf("Changed = %v, want %v (%+v)", Changed(changes), tc.wantEdits, changes)
			}
			for _, want := range tc.wantDetails {
				found := false
				for _, change := range changes {
					if change.Detail == want {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("missing change %q in %+v", want, changes)
				}
			}
			// The input must never be mutated: setup reports a plan before it
			// writes, and a shared slice would make the plan lie.
			if len(handWritten.Transports) != 0 {
				t.Fatal("Merge mutated the policy it was given")
			}
			tc.check(t, got)
		})
	}
}

func TestWritePolicyBacksUpAndStaysOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brw", "browser-profiles.json")
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

	first, _ := Merge(profilepolicy.Policy{}, bridgeRequest())
	backup, err := WritePolicy(path, first, now)
	if err != nil {
		t.Fatal(err)
	}
	if backup != "" {
		t.Fatalf("writing a policy where none existed must not create a backup, got %q", backup)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("policy mode = %v, want 0600", got)
	}

	second := first
	second.Profiles = append(append([]profilepolicy.Profile(nil), second.Profiles...), profilepolicy.Profile{Name: "extra"})
	backup, err = WritePolicy(path, second, now)
	if err != nil {
		t.Fatal(err)
	}
	if backup != BackupPath(path, now) {
		t.Fatalf("backup = %q, want %q", backup, BackupPath(path, now))
	}
	saved, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	var restored profilepolicy.Policy
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored.Profiles) != len(first.Profiles) {
		t.Fatalf("backup holds %d profiles, want the pre-edit %d", len(restored.Profiles), len(first.Profiles))
	}
}

func TestLoadPolicyFileToleratesAbsenceAndReportsBadJSON(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.json")
	policy, found, err := LoadPolicyFile(missing)
	if err != nil || found || len(policy.Profiles) != 0 {
		t.Fatalf("absent policy: policy=%+v found=%v err=%v", policy, found, err)
	}

	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := LoadPolicyFile(broken); err == nil || !found {
		t.Fatalf("malformed policy must be reported, not silently replaced (found=%v err=%v)", found, err)
	}

	// The saved form keeps ~/ unexpanded, unlike profilepolicy.Load, so a
	// merge-and-write never bakes this machine's home into the file.
	written := filepath.Join(dir, "written.json")
	merged, _ := Merge(profilepolicy.Policy{}, bridgeRequest())
	if _, err := WritePolicy(written, merged, time.Now()); err != nil {
		t.Fatal(err)
	}
	reloaded, found, err := LoadPolicyFile(written)
	if err != nil || !found {
		t.Fatalf("reload: found=%v err=%v", found, err)
	}
	profile, err := reloaded.Find("chrome-profile")
	if err != nil {
		t.Fatal(err)
	}
	if profile.UserDataDir != "~/Library/Application Support/Google/Chrome" {
		t.Fatalf("user_data_dir was expanded on write: %q", profile.UserDataDir)
	}
}
