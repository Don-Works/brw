package brwidentity

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIdentityMismatchesComparesOnlyExpectedFields(t *testing.T) {
	got := Identity{
		Workspace:        "client-a",
		Profile:          "chrome-a",
		UserDataDir:      "/profiles/a",
		ProfileDirectory: "Profile 1",
		Mode:             "bridge",
	}
	if mismatches := got.Mismatches(Identity{Workspace: "client-a", Profile: "chrome-a"}); len(mismatches) != 0 {
		t.Fatalf("unexpected mismatches: %v", mismatches)
	}
	if mismatches := got.Mismatches(Identity{Workspace: "client-b"}); len(mismatches) != 1 {
		t.Fatalf("mismatches = %v, want one workspace mismatch", mismatches)
	}
}

func TestIdentityEmpty(t *testing.T) {
	empty := Identity{}
	if !empty.Empty() {
		t.Fatal("zero identity should be empty")
	}
	nonEmpty := Identity{Workspace: "client-a"}
	if nonEmpty.Empty() {
		t.Fatal("workspace identity should not be empty")
	}
}

func TestEmptyCountsTransportAndHeadless(t *testing.T) {
	tests := []struct {
		name string
		id   Identity
		want bool
	}{
		{"zero value", Identity{}, true},
		{"transport only", Identity{Transport: TransportDirectCDP}, false},
		{"headless only", Identity{Headless: true}, false},
		{"workspace only", Identity{Workspace: "brw-agent"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.id.Empty(); got != tt.want {
				t.Fatalf("Empty() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMismatchesIgnoresTransportAndHeadless(t *testing.T) {
	upstream := Identity{
		Workspace: "brw-agent",
		Profile:   "chromium-agent",
		Transport: TransportDirectCDP,
		Headless:  true,
	}
	expected := Identity{Workspace: "brw-agent", Profile: "chromium-agent"}
	if mismatches := upstream.Mismatches(expected); len(mismatches) != 0 {
		t.Fatalf("expected no mismatches, got %v", mismatches)
	}
	wrong := Identity{Workspace: "brw-other", Profile: "chromium-agent"}
	if mismatches := upstream.Mismatches(wrong); len(mismatches) != 1 {
		t.Fatalf("expected exactly the workspace mismatch, got %v", mismatches)
	}
}

func TestIgnoreHTTPSErrorsIsReportedAndCountsAsIdentity(t *testing.T) {
	tests := []struct {
		name       string
		id         Identity
		wantInJSON bool
		wantEmpty  bool
	}{
		{"zero value", Identity{}, false, true},
		{"certificate validation off", Identity{IgnoreHTTPSErrors: true}, true, false},
		{"an ordinary daemon", Identity{Workspace: "brw-agent"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			encoded, err := json.Marshal(tt.id)
			if err != nil {
				t.Fatalf("marshal identity: %v", err)
			}
			if got := strings.Contains(string(encoded), `"ignore_https_errors":true`); got != tt.wantInJSON {
				t.Fatalf("identity JSON %s carries ignore_https_errors = %v, want %v", encoded, got, tt.wantInJSON)
			}
			if got := tt.id.Empty(); got != tt.wantEmpty {
				t.Fatalf("Empty() = %v, want %v; an identity reported as empty is omitted from /health entirely", got, tt.wantEmpty)
			}
		})
	}

	upstream := Identity{Workspace: "brw-agent", Profile: "chromium-agent", IgnoreHTTPSErrors: true}
	if mismatches := upstream.Mismatches(Identity{Workspace: "brw-agent", Profile: "chromium-agent"}); len(mismatches) != 0 {
		t.Fatalf("unexpected mismatches: %v", mismatches)
	}
}
