package mcp

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
)

// Every place that states how many tools the `all` profile advertises, and the
// catalogue each figure is a count of.
//
// The count is re-sent to the model on every request, so four documents quote
// it and they are corrected by hand: the wave that grew the catalogue updated
// three and left skills/brw/SKILL.md — the agent-facing one — a wave behind,
// while its commit message said all three agreed. The figure is also not one
// number. A tool a transport cannot serve is never advertised on it, so the
// same profile is one size unfiltered, a smaller one on direct CDP and a
// smaller one again on the extension bridge, and a document quoting one of
// those without saying which is wrong for most of its readers. Listing the
// surfaces against the catalogue they each describe is what turns the next miss
// into a build failure.
//
// \s+ rather than a literal space so a re-wrapped paragraph still matches; a
// pattern that stops matching fails rather than passing by vacuum.
var fullCatalogueCountClaims = []struct {
	path      string
	transport string
	pattern   *regexp.Regexp
}{
	{"../../README.md", brwidentity.TransportDirectCDP, regexp.MustCompile(`across\s+(\d+)\s+tools`)},
	{"../../docs/agent-guide.md", brwidentity.TransportDirectCDP, regexp.MustCompile("\\|\\s*`all`\\s*\\|\\s*(\\d+)\\s*\\|")},
	{"../../docs/agent-guide.md", "", regexp.MustCompile(`(\d+)\s+tools\s+unfiltered`)},
	{"../../docs/agent-guide.md", brwidentity.TransportDirectCDP, regexp.MustCompile(`(\d+)\s+on\s+direct\s+CDP`)},
	{"../../docs/agent-guide.md", brwidentity.TransportRemoteCDP, regexp.MustCompile("(\\d+)\\s+on\\s+`--remote`")},
	{"../../docs/agent-guide.md", brwidentity.TransportChromeOptIn, regexp.MustCompile(`(\d+)\s+on\s+the\s+Chrome\s+opt-in\s+lane`)},
	{"../../docs/agent-guide.md", brwidentity.TransportOffHostCDP, regexp.MustCompile(`(\d+)\s+on\s+a\s+plugin-supplied\s+off-host\s+browser`)},
	{"../../docs/agent-guide.md", brwidentity.TransportExtensionBridge, regexp.MustCompile(`(\d+)\s+on\s+the\s+extension\s+bridge`)},
	{"../../docs/benchmarks.md", brwidentity.TransportDirectCDP, regexp.MustCompile(`catalogues are\s+(\d+)\s+tools`)},
	{"../../docs/benchmarks.md", "", regexp.MustCompile(`fewer than the\s+(\d+)\s+tools`)},
	{"../../docs/benchmarks.md", brwidentity.TransportDirectCDP, regexp.MustCompile(`leaving the\s+(\d+)\s+above`)},
	{"../../docs/benchmarks.md", brwidentity.TransportRemoteCDP, regexp.MustCompile(`as\s+well\s+\((\d+)\)`)},
	{"../../docs/benchmarks.md", brwidentity.TransportChromeOptIn, regexp.MustCompile(`on\s+top\s+of\s+that\s+\((\d+)\)`)},
	{"../../docs/benchmarks.md", brwidentity.TransportOffHostCDP, regexp.MustCompile("as\\s+well\\s+as\\s+`brw_state`\\s+\\((\\d+)\\)")},
	{"../../docs/benchmarks.md", brwidentity.TransportExtensionBridge, regexp.MustCompile(`leaving\s+(\d+)\.`)},
	{"../../skills/brw/SKILL.md", brwidentity.TransportDirectCDP, regexp.MustCompile(`full surface is\s+(\d+)\s+tools`)},
	{"../../skills/brw/SKILL.md", brwidentity.TransportRemoteCDP, regexp.MustCompile("(\\d+)\\s+on\\s+`--remote`")},
	{"../../skills/brw/SKILL.md", brwidentity.TransportChromeOptIn, regexp.MustCompile(`(\d+)\s+on\s+the\s+Chrome\s+opt-in\s+lane`)},
	{"../../skills/brw/SKILL.md", brwidentity.TransportOffHostCDP, regexp.MustCompile(`(\d+)\s+on\s+a\s+plugin-supplied\s+off-host\s+browser`)},
	{"../../skills/brw/SKILL.md", brwidentity.TransportExtensionBridge, regexp.MustCompile(`(\d+)\s+on\s+the\s+extension\s+bridge`)},
}

// TestDocumentedCatalogueSizeMatchesTheCatalogue reads each stated count out of
// the document and compares it with what a daemon on that transport advertises.
func TestDocumentedCatalogueSizeMatchesTheCatalogue(t *testing.T) {
	for _, claim := range fullCatalogueCountClaims {
		t.Run(claim.path+"/"+claim.pattern.String(), func(t *testing.T) {
			server := &Server{toolProfile: "all", identity: brwidentity.Identity{Transport: claim.transport}}
			advertised := len(server.advertisedTools())
			// An empty transport is the unfiltered catalogue, not a daemon with
			// a blank name, and the failure has to read as the former.
			daemon := claim.transport
			if daemon == "" {
				daemon = "unfiltered"
			}
			if advertised == 0 {
				t.Fatal("the full catalogue is empty, so this test would pass by vacuum")
			}
			raw, err := os.ReadFile(claim.path)
			if err != nil {
				t.Fatalf("read %s: %v", claim.path, err)
			}
			matches := claim.pattern.FindAllStringSubmatch(string(raw), -1)
			if len(matches) == 0 {
				t.Fatalf("%s no longer states a tool count where %s expects one; restore the sentence or move this entry, because an unmatched pattern is a document nobody is checking", claim.path, claim.pattern)
			}
			for _, match := range matches {
				stated, err := strconv.Atoi(match[1])
				if err != nil {
					t.Fatalf("%s states a tool count that is not a number (%q): %v", claim.path, match[1], err)
				}
				if stated != advertised {
					t.Errorf("%s says %d tools where the %s catalogue holds %d; re-run scripts/measure-tool-catalogue.py and update every surface listed here, not only this one", claim.path, stated, daemon, advertised)
				}
			}
		})
	}
}

// The per-transport counts above are only meaningful while the narrower
// profiles really are transport-independent, which is what lets every other
// figure in those documents stand without a transport beside it.
func TestNarrowProfilesAreTheSameSizeOnEveryTransport(t *testing.T) {
	for _, profile := range []string{"core", "minimal", autoProfile} {
		t.Run(profile, func(t *testing.T) {
			unfiltered := len((&Server{toolProfile: profile}).advertisedTools())
			if unfiltered == 0 {
				t.Fatalf("profile %q advertises nothing, so this test would pass by vacuum", profile)
			}
			for _, transport := range []string{brwidentity.TransportDirectCDP, brwidentity.TransportExtensionBridge} {
				server := &Server{toolProfile: profile, identity: brwidentity.Identity{Transport: transport}}
				if got := len(server.advertisedTools()); got != unfiltered {
					t.Errorf("profile %q advertises %d tools on %s and %d unfiltered; the docs say the narrow profiles are the same size everywhere", profile, got, transport, unfiltered)
				}
			}
		})
	}
}
