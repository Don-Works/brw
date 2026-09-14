package brwidentity

import "testing"

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

// A proxy learns transport and headlessness from its upstream instead of
// asserting them, so neither may be treated as an identity mismatch.
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

// A daemon that launched Chrome with certificate validation off has to say so:
// an agent reading a page over it cannot otherwise tell a real site from an
// intercepted one, and there is no other surface that would reveal it.
func TestIgnoreHTTPSErrorsIsReportedAndCountsAsIdentity(t *testing.T) {
	tests := []struct {
		name       string
		id         Identity
		wantReport bool
		wantEmpty  bool
	}{
		{"zero value", Identity{}, false, true},
		{"certificate validation off", Identity{IgnoreHTTPSErrors: true}, true, false},
		{"an ordinary daemon", Identity{Workspace: "brw-agent"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.id.IgnoreHTTPSErrors != tt.wantReport {
				t.Fatalf("IgnoreHTTPSErrors = %v, want %v", tt.id.IgnoreHTTPSErrors, tt.wantReport)
			}
			if got := tt.id.Empty(); got != tt.wantEmpty {
				t.Fatalf("Empty() = %v, want %v; an identity reported as empty is omitted from /health entirely", got, tt.wantEmpty)
			}
		})
	}
	// It is a property of the daemon answering, not of the workspace binding, so
	// pinning a profile must not reject a daemon over it.
	upstream := Identity{Workspace: "brw-agent", Profile: "chromium-agent", IgnoreHTTPSErrors: true}
	if mismatches := upstream.Mismatches(Identity{Workspace: "brw-agent", Profile: "chromium-agent"}); len(mismatches) != 0 {
		t.Fatalf("unexpected mismatches: %v", mismatches)
	}
}
