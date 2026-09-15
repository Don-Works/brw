package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/profilepolicy"
)

func TestResolveBaseURL(t *testing.T) {
	reachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer reachable.Close()

	onlyProfile := writePolicyFile(t, profilepolicy.Policy{Profiles: []profilepolicy.Profile{
		{Name: "work", ExtensionBridgeAllowed: true, BridgeHTTPAddr: "127.0.0.1:17410"},
	}})
	twoProfiles := writePolicyFile(t, profilepolicy.Policy{Profiles: []profilepolicy.Profile{
		// Nothing listens on port 1, so the probe has to move on to the second.
		{Name: "down", ExtensionBridgeAllowed: true, BridgeHTTPAddr: "127.0.0.1:1"},
		{Name: "up", ExtensionBridgeAllowed: true, BridgeHTTPAddr: reachable.URL},
	}})
	directOnly := writePolicyFile(t, profilepolicy.Policy{Profiles: []profilepolicy.Profile{
		{Name: "direct", DirectCDPAllowed: true},
	}})
	neither := writePolicyFile(t, profilepolicy.Policy{Profiles: []profilepolicy.Profile{
		{Name: "idle"},
	}})

	tests := []struct {
		name    string
		env     string
		opts    options
		want    string
		wantErr string
	}{
		{
			name: "an explicit daemon wins",
			env:  "http://127.0.0.1:19999",
			opts: options{daemon: "http://127.0.0.1:18000", policyPath: onlyProfile},
			want: "http://127.0.0.1:18000",
		},
		{
			name: "BRW_URL comes next",
			env:  "http://127.0.0.1:19999",
			opts: options{policyPath: onlyProfile},
			want: "http://127.0.0.1:19999",
		},
		{
			name: "one configured profile needs no probe",
			opts: options{policyPath: onlyProfile},
			want: "http://127.0.0.1:17410",
		},
		{
			name: "a named profile is taken as named",
			opts: options{policyPath: twoProfiles, profile: "down"},
			want: "http://127.0.0.1:1",
		},
		{
			name: "several profiles resolve to the reachable one",
			opts: options{policyPath: twoProfiles},
			want: reachable.URL,
		},
		{
			name:    "an unknown profile name",
			opts:    options{policyPath: onlyProfile, profile: "personal"},
			wantErr: `no extension-bridge profile named "personal"`,
		},
		{
			name: "a direct-cdp profile is a candidate",
			opts: options{policyPath: directOnly},
			want: "http://127.0.0.1:17310",
		},
		{
			name:    "a policy with no daemon profile",
			opts:    options{policyPath: neither},
			wantErr: "configures no extension-bridge profile",
		},
		{
			name:    "no policy at all",
			opts:    options{policyPath: filepath.Join(t.TempDir(), "missing.json")},
			wantErr: "pass --daemon or set BRW_URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BRW_URL", tt.env)
			opts := tt.opts
			got, err := resolveBaseURL(&opts)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), "no brw daemon reachable") {
					t.Fatalf("err = %v, want it classified as unreachable", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("base URL = %q, want %q", got, tt.want)
			}
		})
	}
}

func writePolicyFile(t *testing.T, policy profilepolicy.Policy) string {
	t.Helper()
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "browser-profiles.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
