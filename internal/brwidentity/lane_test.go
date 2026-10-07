package brwidentity

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

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

func TestEveryLaneReportsADeclaredTransport(t *testing.T) {
	lanes := []Lane{{}}
	for _, classified := range laneClassification {
		lanes = append(lanes, classified.lane)
	}

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

func TestAProxyIsNotTreatedAsABrowserElsewhere(t *testing.T) {
	proxy := Lane{UpstreamHTTP: "http://127.0.0.1:17410"}
	if !proxy.BrowserOnThisHost() {
		t.Fatal("a proxy was classified as driving a browser on another machine")
	}
	if got := proxy.Transport(); got != "" {
		t.Fatalf("a proxy reported transport %q; it has to adopt its upstream's", got)
	}
}
