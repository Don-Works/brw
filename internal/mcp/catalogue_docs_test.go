package mcp

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
)

// The tool catalogue is quoted in four documents, and nothing in the tree read
// any of them: the count is measured by hand with
// scripts/measure-tool-catalogue.py and pasted in, so a branch that adds a tool
// updates whichever documents it happened to look at and leaves the rest a
// release behind. Three of the four already disagreed, and all four quoted a
// direct-CDP daemon's catalogue while one of them called it the unfiltered
// ceiling. This reads the numbers back out and compares them against the
// catalogue the binary advertises on the named transport.
//
// Only the counts are checked. The token figures are prose: they come from the
// measuring script's characters-per-token approximation rather than from
// anything this package can compute, and pinning them here would fail on every
// wording change to a description.
type catalogueClaim struct {
	name string
	file string
	// profile is the --mcp-tools profile the figure is for, and transport the
	// daemon identity it was measured on. An empty transport is the unfiltered
	// catalogue, which no running daemon serves.
	profile   string
	transport string
	pattern   string
}

func catalogueClaims() []catalogueClaim {
	const direct = brwidentity.TransportDirectCDP
	const bridge = brwidentity.TransportExtensionBridge
	return []catalogueClaim{
		{name: "README all", file: "../../README.md", profile: "all", transport: direct, pattern: `costs ~[\d.]+k tokens across (\d+) tools`},
		{name: "agent-guide all", file: "../../docs/agent-guide.md", profile: "all", transport: direct, pattern: "\\| `all` \\| (\\d+) \\|"},
		{name: "agent-guide core", file: "../../docs/agent-guide.md", profile: "core", transport: direct, pattern: "\\| `core` \\| (\\d+) \\|"},
		{name: "agent-guide minimal", file: "../../docs/agent-guide.md", profile: "minimal", transport: direct, pattern: "\\| `minimal` \\| (\\d+) \\|"},
		{name: "agent-guide auto", file: "../../docs/agent-guide.md", profile: "auto", transport: direct, pattern: "\\| `auto` \\(default\\) \\| (\\d+), growing \\|"},
		{name: "agent-guide unfiltered", file: "../../docs/agent-guide.md", profile: "all", pattern: `row is (\d+) tools unfiltered`},
		{name: "agent-guide direct", file: "../../docs/agent-guide.md", profile: "all", transport: direct, pattern: `unfiltered, (\d+) on direct CDP`},
		{name: "agent-guide bridge", file: "../../docs/agent-guide.md", profile: "all", transport: bridge, pattern: `and (\d+) on the extension bridge`},
		{name: "benchmarks all", file: "../../docs/benchmarks.md", profile: "all", transport: direct, pattern: `measured MCP catalogues are (\d+) tools`},
		{name: "benchmarks core", file: "../../docs/benchmarks.md", profile: "core", transport: direct, pattern: "(\\d+) / ~[\\d.]+k for `core`"},
		{name: "benchmarks minimal", file: "../../docs/benchmarks.md", profile: "minimal", transport: direct, pattern: "(\\d+) / ~[\\d.]+k for `minimal`"},
		{name: "benchmarks auto", file: "../../docs/benchmarks.md", profile: "auto", transport: direct, pattern: "(\\d+) / ~[\\d.]+k initially for the default"},
		{name: "benchmarks unfiltered", file: "../../docs/benchmarks.md", profile: "all", pattern: `advertises fewer than the (\d+) tools`},
		{name: "benchmarks direct", file: "../../docs/benchmarks.md", profile: "all", transport: direct, pattern: `leaving the (\d+) above`},
		{name: "benchmarks bridge", file: "../../docs/benchmarks.md", profile: "all", transport: bridge, pattern: `pushState and session snapshots — leaving (\d+)`},
		{name: "SKILL all", file: "../../skills/brw/SKILL.md", profile: "all", transport: direct, pattern: `full surface is (\d+) tools on a direct-CDP`},
		{name: "SKILL bridge", file: "../../skills/brw/SKILL.md", profile: "all", transport: bridge, pattern: `daemon \((\d+) on the extension bridge`},
	}
}

func TestDocumentedCatalogueSizesMatchTheBinary(t *testing.T) {
	documents := map[string]string{}
	for _, claim := range catalogueClaims() {
		if _, read := documents[claim.file]; read {
			continue
		}
		data, err := os.ReadFile(claim.file)
		if err != nil {
			t.Fatalf("read %s: %v", claim.file, err)
		}
		// Line wrapping is an editing accident, not part of the claim: a figure
		// that happens to fall either side of a newline is the same sentence.
		documents[claim.file] = strings.Join(strings.Fields(string(data)), " ")
	}

	for _, claim := range catalogueClaims() {
		t.Run(claim.name, func(t *testing.T) {
			match := regexp.MustCompile(claim.pattern).FindStringSubmatch(documents[claim.file])
			if match == nil {
				t.Fatalf("%s no longer states the %s catalogue size in a form this test can read (%s); the figure and the check have to move together",
					claim.file, claim.profile, claim.pattern)
			}
			claimed, err := strconv.Atoi(match[1])
			if err != nil {
				t.Fatalf("%s: %q is not a number: %v", claim.file, match[1], err)
			}
			server := &Server{toolProfile: claim.profile, identity: brwidentity.Identity{Transport: claim.transport}}
			if live := len(server.advertisedTools()); claimed != live {
				t.Errorf("%s says the %s profile advertises %d tools on transport %q; the binary advertises %d",
					claim.file, claim.profile, claimed, claim.transport, live)
			}
		})
	}
}

// The scan above passes by vacuum if the table stops covering anything, so this
// pins the table's own shape: every document that quotes a figure is in it, and
// the transports it compares against really do produce different catalogues.
func TestCatalogueClaimsCoverEveryDocumentThatQuotesOne(t *testing.T) {
	files := map[string]bool{}
	transports := map[string]bool{}
	for _, claim := range catalogueClaims() {
		files[claim.file] = true
		transports[claim.transport] = true
	}
	for _, want := range []string{"../../README.md", "../../docs/agent-guide.md", "../../docs/benchmarks.md", "../../skills/brw/SKILL.md"} {
		if !files[want] {
			t.Errorf("%s quotes the catalogue size and no claim covers it", want)
		}
	}
	for _, want := range []string{"", brwidentity.TransportDirectCDP, brwidentity.TransportExtensionBridge} {
		if !transports[want] {
			t.Errorf("no claim is checked against transport %q, so a document could quote that catalogue unchecked", want)
		}
	}
	unfiltered := len((&Server{toolProfile: "all"}).advertisedTools())
	direct := len((&Server{toolProfile: "all", identity: brwidentity.Identity{Transport: brwidentity.TransportDirectCDP}}).advertisedTools())
	bridge := len((&Server{toolProfile: "all", identity: brwidentity.Identity{Transport: brwidentity.TransportExtensionBridge}}).advertisedTools())
	if !(bridge < direct && direct < unfiltered) {
		t.Fatalf("catalogue sizes are not distinct per transport (bridge=%d direct=%d unfiltered=%d); the comparison above would not be reading a transport at all",
			bridge, direct, unfiltered)
	}
}
