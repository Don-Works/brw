package setup

import (
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
)

// doctor prints this table as the answer to "what can this install do". Every
// lane brw can report has to be in it: a lane that fell through to another
// lane's text would be described authoritatively and wrongly, and the reader
// has nothing to check it against.
func TestEveryTransportIsDescribed(t *testing.T) {
	for _, transport := range brwidentity.Transports() {
		caps := CapabilitiesFor(transport)
		if strings.HasPrefix(caps.Has, "unknown") {
			t.Errorf("transport %q has no capability description", transport)
		}
		if caps.Transport != transport {
			t.Errorf("CapabilitiesFor(%q).Transport = %q", transport, caps.Transport)
		}
	}
	if len(capabilityTable) != len(brwidentity.Transports()) {
		t.Errorf("%d described lanes but %d transports brw can report", len(capabilityTable), len(brwidentity.Transports()))
	}
	// An unclassified lane must say so rather than borrowing another's list.
	unknown := CapabilitiesFor("some-future-lane")
	if !strings.HasPrefix(unknown.Has, "unknown") || !strings.HasPrefix(unknown.Lacks, "unknown") {
		t.Fatalf("an undescribed transport was described as %+v", unknown)
	}
}

// The lanes differ in exactly the capabilities people ask about, so the text has
// to differ there. Two lanes sharing a sentence is the way this table has gone
// wrong before.
func TestLaneDescriptionsDifferWhereTheLanesDo(t *testing.T) {
	for _, tc := range []struct {
		transport   string
		hasMentions []string
		lackMention string
	}{
		{ResolvedDirectCDP, []string{"brw_cookies", "brw_open_incognito"}, "tab groups"},
		{ResolvedChromeOptIn, []string{"brw_cookies", "brw_open_incognito", "signed-in"}, "tab groups"},
		{ResolvedRemoteCDP, []string{"brw_cookies", "brw_open_incognito", "somebody else started"}, "download routing"},
		{ResolvedExtensionBridge, []string{"tab groups"}, "brw_cookies"},
	} {
		caps := CapabilitiesFor(tc.transport)
		for _, mention := range tc.hasMentions {
			if !strings.Contains(caps.Has, mention) {
				t.Errorf("%s does not say it has %q: %q", tc.transport, mention, caps.Has)
			}
		}
		if !strings.Contains(caps.Lacks, tc.lackMention) {
			t.Errorf("%s does not say it lacks %q: %q", tc.transport, tc.lackMention, caps.Lacks)
		}
	}
}
