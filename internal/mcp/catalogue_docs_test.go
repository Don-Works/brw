package mcp

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
)

type catalogueClaim struct {
	name string
	file string

	profile   string
	transport string
	pattern   string
}

func catalogueClaims() []catalogueClaim {
	const direct = brwidentity.TransportDirectCDP
	const bridge = brwidentity.TransportExtensionBridge
	const remote = brwidentity.TransportRemoteCDP
	const optIn = brwidentity.TransportChromeOptIn
	const offHost = brwidentity.TransportOffHostCDP
	return []catalogueClaim{
		{name: "README all", file: "../../README.md", profile: "all", transport: direct, pattern: `costs ~[\d.]+k tokens across (\d+) tools`},
		{name: "agent-guide all", file: "../../docs/agent-guide.md", profile: "all", transport: direct, pattern: "\\| `all` \\| (\\d+) \\|"},
		{name: "agent-guide core", file: "../../docs/agent-guide.md", profile: "core", transport: direct, pattern: "\\| `core` \\| (\\d+) \\|"},
		{name: "agent-guide minimal", file: "../../docs/agent-guide.md", profile: "minimal", transport: direct, pattern: "\\| `minimal` \\| (\\d+) \\|"},
		{name: "agent-guide auto", file: "../../docs/agent-guide.md", profile: "auto", transport: direct, pattern: "\\| `auto` \\(default\\) \\| (\\d+), growing \\|"},
		{name: "agent-guide unfiltered", file: "../../docs/agent-guide.md", profile: "all", pattern: `row is (\d+) tools unfiltered`},
		{name: "agent-guide direct", file: "../../docs/agent-guide.md", profile: "all", transport: direct, pattern: `unfiltered, (\d+) on direct CDP`},
		{name: "agent-guide remote", file: "../../docs/agent-guide.md", profile: "all", transport: remote, pattern: "(\\d+) on `--remote`"},
		{name: "agent-guide opt-in", file: "../../docs/agent-guide.md", profile: "all", transport: optIn, pattern: `(\d+) on the Chrome opt-in lane`},
		{name: "agent-guide off-host", file: "../../docs/agent-guide.md", profile: "all", transport: offHost, pattern: `(\d+) on a plugin-supplied off-host browser`},
		{name: "agent-guide bridge", file: "../../docs/agent-guide.md", profile: "all", transport: bridge, pattern: `and (\d+) on the extension bridge`},
		{name: "benchmarks all", file: "../../docs/benchmarks.md", profile: "all", transport: direct, pattern: `measured MCP catalogues are (\d+) tools`},
		{name: "benchmarks core", file: "../../docs/benchmarks.md", profile: "core", transport: direct, pattern: "(\\d+) / ~[\\d.]+k for `core`"},
		{name: "benchmarks minimal", file: "../../docs/benchmarks.md", profile: "minimal", transport: direct, pattern: "(\\d+) / ~[\\d.]+k for `minimal`"},
		{name: "benchmarks auto", file: "../../docs/benchmarks.md", profile: "auto", transport: direct, pattern: "(\\d+) / ~[\\d.]+k initially for the default"},
		{name: "benchmarks unfiltered", file: "../../docs/benchmarks.md", profile: "all", pattern: `advertises fewer than the (\d+) tools`},
		{name: "benchmarks direct", file: "../../docs/benchmarks.md", profile: "all", transport: direct, pattern: `leaving the (\d+) above`},
		{name: "benchmarks remote", file: "../../docs/benchmarks.md", profile: "all", transport: remote, pattern: `as well \((\d+)\)`},
		{name: "benchmarks opt-in", file: "../../docs/benchmarks.md", profile: "all", transport: optIn, pattern: `on top of that \((\d+)\)`},
		{name: "benchmarks off-host", file: "../../docs/benchmarks.md", profile: "all", transport: offHost, pattern: "as well as `brw_state` \\((\\d+)\\)"},
		{name: "benchmarks bridge", file: "../../docs/benchmarks.md", profile: "all", transport: bridge, pattern: `pushState and session snapshots — leaving (\d+)`},
		{name: "SKILL all", file: "../../skills/brw/SKILL.md", profile: "all", transport: direct, pattern: `full surface is (\d+) tools on a direct-CDP`},
		{name: "SKILL remote", file: "../../skills/brw/SKILL.md", profile: "all", transport: remote, pattern: "(\\d+) on `--remote`"},
		{name: "SKILL opt-in", file: "../../skills/brw/SKILL.md", profile: "all", transport: optIn, pattern: `(\d+) on the Chrome opt-in lane`},
		{name: "SKILL off-host", file: "../../skills/brw/SKILL.md", profile: "all", transport: offHost, pattern: `(\d+) on a plugin-supplied off-host browser`},
		{name: "SKILL bridge", file: "../../skills/brw/SKILL.md", profile: "all", transport: bridge, pattern: `(\d+) on the extension bridge`},
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

		documents[claim.file] = strings.Join(strings.Fields(string(data)), " ")
	}

	for _, claim := range catalogueClaims() {
		t.Run(claim.name, func(t *testing.T) {
			matches := regexp.MustCompile(claim.pattern).FindAllStringSubmatch(documents[claim.file], -1)
			if len(matches) == 0 {
				t.Fatalf("%s no longer states the %s catalogue size in a form this test can read (%s); the figure and the check have to move together",
					claim.file, claim.profile, claim.pattern)
			}
			server := &Server{toolProfile: claim.profile, identity: brwidentity.Identity{Transport: claim.transport}}
			for _, match := range matches {
				claimed, err := strconv.Atoi(match[1])
				if err != nil {
					t.Fatalf("%s: %q is not a number: %v", claim.file, match[1], err)
				}
				if live := len(server.advertisedTools()); claimed != live {
					t.Errorf("%s says the %s profile advertises %d tools on transport %q; the binary advertises %d",
						claim.file, claim.profile, claimed, claim.transport, live)
				}
			}
		})
	}
}

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

	for _, want := range append([]string{""}, brwidentity.Transports()...) {
		if !transports[want] {
			t.Errorf("no claim is checked against transport %q, so a document could quote that catalogue unchecked", want)
		}
	}

	sizes := map[int]string{}
	unfiltered := len((&Server{toolProfile: "all"}).advertisedTools())
	sizes[unfiltered] = "unfiltered"
	for _, transport := range brwidentity.Transports() {
		size := len((&Server{toolProfile: "all", identity: brwidentity.Identity{Transport: transport}}).advertisedTools())
		if size >= unfiltered {
			t.Errorf("transport %q advertises %d of the %d unfiltered tools; a lane cannot serve more than the catalogue holds", transport, size, unfiltered)
		}
		if other, clash := sizes[size]; clash {
			t.Errorf("transports %q and %q both advertise %d tools, so a claim against either would pass while naming the wrong lane", transport, other, size)
			continue
		}
		sizes[size] = transport
	}
}
