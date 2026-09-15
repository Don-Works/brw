package brwidentity

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// --remote takes a URL, and a URL can name another machine. The blocker this
// answers is that every capability gate brw added for a plugin-supplied browser
// asked "is there a provider?" instead of "where is this browser?", so
// `brwd --remote http://198.51.100.7:9222` reached a browser on another machine
// with all of them inert: brw_state restore would decrypt a snapshot of a
// session a human signed into HERE and install its cookies over there.
func TestARemoteEndpointOffThisMachineIsNotDirectCDP(t *testing.T) {
	for name, test := range map[string]struct {
		endpoint string
		onHost   bool
	}{
		"brw launched it":     {"", true},
		"loopback v4":         {"http://127.0.0.1:9222", true},
		"another loopback v4": {"http://127.0.0.2:9222/json", true},
		"loopback v6":         {"ws://[::1]:9222/devtools/browser/x", true},
		"localhost":           {"http://localhost:9222", true},
		"localhost cased":     {"http://LocalHost:9222", true},
		"no scheme, loopback": {"127.0.0.1:9222", true},
		// RFC 5737 / RFC 3849 documentation ranges, so no row can name a real
		// machine. Neither is loopback, which is the only thing that matters.
		"an address elsewhere":         {"http://198.51.100.7:9222", false},
		"an ipv6 address elsewhere":    {"http://[2001:db8::1]:9222", false},
		"a hostname":                   {"http://browsers.example:9222", false},
		"a hostname over wss":          {"wss://browsers.example/devtools/browser/x", false},
		"no scheme, another host":      {"198.51.100.7:9222", false},
		"a name that merely starts so": {"http://localhost.evil.example:9222", false},
		"unparseable":                  {"http://[::1", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := BrowserRunsOnThisHost(test.endpoint); got != test.onHost {
				t.Fatalf("BrowserRunsOnThisHost(%q) = %v, want %v", test.endpoint, got, test.onHost)
			}
			lane := Lane{CDPEndpoint: test.endpoint}
			// Three answers, not two. Off this machine is off-host-cdp. On it,
			// an endpoint brw was GIVEN is remote-cdp — brw did not start that
			// browser, so it may not retarget its downloads — and no endpoint
			// at all is the direct-CDP launch.
			want := TransportOffHostCDP
			switch {
			case test.onHost && strings.TrimSpace(test.endpoint) != "":
				want = TransportRemoteCDP
			case test.onHost:
				want = TransportDirectCDP
			}
			if got := lane.Transport(); got != want {
				t.Fatalf("Lane{CDPEndpoint: %q}.Transport() = %q, want %q", test.endpoint, got, want)
			}
			if got := lane.BrowserOnThisHost(); got != test.onHost {
				t.Fatalf("Lane{CDPEndpoint: %q}.BrowserOnThisHost() = %v, want %v", test.endpoint, got, test.onHost)
			}
		})
	}
}

// laneClassification is every lane a brwd process can be started in, keyed by
// the struct field that selects it. The reflection below fails when a field is
// added to Lane and left out of this map, which is the only way a new way of
// naming a browser can reach the gates unclassified.
var laneClassification = map[string]struct {
	lane      Lane
	transport string
}{
	"UpstreamHTTP":    {Lane{UpstreamHTTP: "http://127.0.0.1:17410"}, ""},
	"Bridge":          {Lane{Bridge: true}, TransportExtensionBridge},
	"BrowserProvider": {Lane{BrowserProvider: true}, TransportOffHostCDP},
	"ChromeOptIn":     {Lane{ChromeOptIn: true}, TransportChromeOptIn},
	"CDPEndpoint":     {Lane{CDPEndpoint: "http://198.51.100.7:9222"}, TransportOffHostCDP},
}

func TestEveryLaneFieldChangesTheClassification(t *testing.T) {
	laneType := reflect.TypeOf(Lane{})
	zero := Lane{}.Transport()
	if zero != TransportDirectCDP {
		t.Fatalf("a lane with nothing set = %q, want %q; brw launching Chrome itself is the default lane", zero, TransportDirectCDP)
	}
	for index := 0; index < laneType.NumField(); index++ {
		field := laneType.Field(index).Name
		classified, ok := laneClassification[field]
		if !ok {
			t.Errorf("Lane.%s is a way of reaching a browser that nothing classifies; give it a case in Lane.Transport and a row here", field)
			continue
		}
		got := classified.lane.Transport()
		if got != classified.transport {
			t.Errorf("Lane.%s: Transport() = %q, want %q", field, got, classified.transport)
		}
		if got == zero {
			t.Errorf("Lane.%s makes no difference to the classification (%q either way), so setting it changes no gate", field, got)
		}
	}
	for field := range laneClassification {
		if _, ok := laneType.FieldByName(field); !ok {
			t.Errorf("laneClassification names %q, which is not a field of Lane", field)
		}
	}
}

// Every transport a lane can report has to be one brwidentity declares, or a
// table keyed by transport silently fails to classify a running daemon.
func TestEveryLaneReportsADeclaredTransport(t *testing.T) {
	lanes := []Lane{{}}
	for _, classified := range laneClassification {
		lanes = append(lanes, classified.lane)
	}
	// And the combinations, because a lane is not one flag: --bridge with a
	// --remote endpoint elsewhere still has to land somewhere declared.
	lanes = append(lanes,
		Lane{Bridge: true, CDPEndpoint: "http://198.51.100.7:9222"},
		Lane{BrowserProvider: true, CDPEndpoint: "http://127.0.0.1:9222"},
		Lane{UpstreamHTTP: "http://127.0.0.1:17410", BrowserProvider: true},
		Lane{ChromeOptIn: true, CDPEndpoint: "http://127.0.0.1:9222"},
		Lane{ChromeOptIn: true, CDPEndpoint: "http://198.51.100.7:9222"},
	)
	for _, lane := range lanes {
		transport := lane.Transport()
		if transport == "" {
			if lane.UpstreamHTTP == "" {
				t.Errorf("lane %+v reports no transport and is not a proxy", lane)
			}
			continue
		}
		if !slices.Contains(Transports(), transport) {
			t.Errorf("lane %+v reports %q, which is not in %v", lane, transport, Transports())
		}
	}
}

// The Chrome opt-in resolves to a loopback endpoint, so the two describe the
// same browser and the opt-in is the more specific answer. Paired with an
// endpoint somewhere else they describe different machines, and the answer has
// to be the restrictive one: that lane must not keep the opt-in's local
// filesystem and clipboard.
func TestTheOptInLaneLosesToAnEndpointOnAnotherMachine(t *testing.T) {
	local := Lane{ChromeOptIn: true, CDPEndpoint: "http://127.0.0.1:9222"}
	if got := local.Transport(); got != TransportChromeOptIn {
		t.Fatalf("opt-in at its own loopback endpoint = %q, want %q", got, TransportChromeOptIn)
	}
	elsewhere := Lane{ChromeOptIn: true, CDPEndpoint: "http://198.51.100.7:9222"}
	if got := elsewhere.Transport(); got != TransportOffHostCDP {
		t.Fatalf("opt-in naming an endpoint on another machine = %q, want %q", got, TransportOffHostCDP)
	}
	if elsewhere.BrowserOnThisHost() {
		t.Fatal("a lane naming an endpoint on another machine reported its browser as local")
	}
}

// --remote at a loopback endpoint is a browser on this machine that brw did not
// start. It keeps every local-machine capability and loses only the one that
// depends on brw having started the browser, so it must not be reported as
// direct-cdp (which would let brw retarget that browser's downloads) and must
// not be reported as off-host-cdp (which would refuse uploads and the clipboard
// that work there).
func TestALoopbackRemoteEndpointIsItsOwnLane(t *testing.T) {
	lane := Lane{CDPEndpoint: "http://127.0.0.1:9222"}
	if got := lane.Transport(); got != TransportRemoteCDP {
		t.Fatalf("--remote at loopback = %q, want %q", got, TransportRemoteCDP)
	}
	if !lane.BrowserOnThisHost() {
		t.Fatal("--remote at loopback reported its browser as somewhere else")
	}
	caps, known := Capabilities(TransportRemoteCDP)
	if !known {
		t.Fatal("remote-cdp has no declared capabilities")
	}
	if caps.RuntimeDownloadRouting {
		t.Error("remote-cdp declares download routing; brw did not start that browser")
	}
	if !caps.BrowserOnThisHost {
		t.Error("remote-cdp declares its browser is elsewhere; --remote at loopback is on this machine")
	}
}

// A proxy drives no browser of its own, so it must not be read as driving one
// on another machine: the daemon it forwards to applies its own lane's answer,
// and a proxy that refused this host's capabilities would break every local
// setup that fronts a direct-CDP daemon with an MCP wrapper.
func TestAProxyIsNotTreatedAsABrowserElsewhere(t *testing.T) {
	proxy := Lane{UpstreamHTTP: "http://127.0.0.1:17410"}
	if !proxy.BrowserOnThisHost() {
		t.Fatal("a proxy was classified as driving a browser on another machine")
	}
	if got := proxy.Transport(); got != "" {
		t.Fatalf("a proxy reported transport %q; it has to adopt its upstream's", got)
	}
}
