package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	cdplaunch "github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/sessionstate"
	"github.com/Don-Works/brw/internal/snapshot"
)

// laneEndpointOnThisHost is a loopback endpoint with nothing behind it. The
// lane tests never dial: they assert what the gate answers before a dial would
// happen, and a port nothing listens on keeps that true even while another
// agent has a Chrome open on 9222.
const laneEndpointOnThisHost = "http://127.0.0.1:1"

// laneEndpointElsewhere is the shape of the blocker: --remote takes a URL, and
// a URL can name another machine.
const laneEndpointElsewhere = "http://198.51.100.7:9222"

// managerForLane builds a manager exactly the way brwd does, short of the
// connect. Nothing is dialled, so the classification every capability gate
// reads is provable here rather than only against a real browser on a real
// second machine — which is the test nobody runs, and the reason the gate was
// inert for the --remote lane.
func managerForLane(t *testing.T, cfg Config) *Manager {
	t.Helper()
	m, err := newManager(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newManager(%+v) = %v", cfg, err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// managerLanes is every way a *Manager can be pointed at a browser, with where
// that browser is. A lane missing from here is a lane whose capability gates
// nobody checked.
func managerLanes() map[string]struct {
	cfg    Config
	onHost bool
} {
	return map[string]struct {
		cfg    Config
		onHost bool
	}{
		"brw launched it":             {Config{}, true},
		"--remote at loopback":        {Config{RemoteURL: laneEndpointOnThisHost}, true},
		"--remote at localhost":       {Config{RemoteURL: "http://localhost:9222"}, true},
		"--remote off this machine":   {Config{RemoteURL: laneEndpointElsewhere}, false},
		"--remote at a hostname":      {Config{RemoteURL: "http://browsers.example:9222"}, false},
		"--remote at an unusable url": {Config{RemoteURL: "http://[::1"}, false},
		"a plugin-supplied browser": {
			Config{Remote: &RemoteTarget{WebSocketURL: "wss://browsers.example/devtools/browser/x", RedactedURL: "wss://browsers.example"}},
			false,
		},
	}
}

// The gate is keyed on where the browser is, so every lane that reaches one
// somewhere else inherits the whole refusal table without an edit. This is the
// case the branch got wrong: `brwd --remote http://198.51.100.7:9222` built a
// manager with Remote() false, so refuseOnRemote returned nil for every
// capability and brw_state restore would have decrypted a snapshot of a session
// a human signed into HERE and installed its cookies over there.
func TestEveryLaneAManagerCanBeBuiltForIsClassified(t *testing.T) {
	for name, lane := range managerLanes() {
		t.Run(name, func(t *testing.T) {
			if got := lane.cfg.BrowserOnThisHost(); got != lane.onHost {
				t.Fatalf("Config%+v.BrowserOnThisHost() = %v, want %v", lane.cfg, got, lane.onHost)
			}
			if lane.cfg.Remote == nil && lane.cfg.RemoteURL == "" {
				// The launch lane would start Chrome. Its classification is the
				// pure answer above; the wired gates below need an endpoint.
				return
			}
			m := managerForLane(t, lane.cfg)
			if got := m.BrowserOnThisHost(); got != lane.onHost {
				t.Fatalf("a manager built for %s reports BrowserOnThisHost() = %v, want %v", name, got, lane.onHost)
			}
			for _, capability := range RemoteCapabilityNames() {
				err := m.refuseOnRemote(capability)
				if lane.onHost && err != nil {
					t.Errorf("refuseOnRemote(%q) on a browser beside brwd = %v, want nil", capability, err)
				}
				if !lane.onHost && !errors.Is(err, ErrRemoteTargetUnsupported) {
					t.Errorf("refuseOnRemote(%q) on a browser elsewhere = %v, want the named refusal", capability, err)
				}
			}
			if err := m.CheckProfileSession(); lane.onHost != (err == nil) {
				t.Errorf("CheckProfileSession() on %s = %v, want nil = %v", name, err, lane.onHost)
			}
		})
	}
}

// The gate has to hold on the verbs an agent actually calls, not only on the
// helper. Each of these is the wired entry point a tool handler reaches, and
// each is called on a manager built from a plain `--remote` endpoint with no
// provider anywhere.
func TestTheRefusedVerbsRefuseOnARemoteEndpointOffThisMachine(t *testing.T) {
	root := filepath.Join(t.TempDir(), "snapshots")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// Obviously fabricated and low entropy: it seals nothing this test reads.
	store, err := sessionstate.NewStore(sessionstate.Config{Root: root, Key: []byte("fixture-session-state-key-0123456789ab")})
	if err != nil {
		t.Fatalf("open the snapshot store: %v", err)
	}
	m := managerForLane(t, Config{RemoteURL: laneEndpointElsewhere})
	m.SetSessionStateStore(store)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for name, call := range map[string]func() error{
		"brw_state save": func() error {
			_, err := m.SessionState(ctx, SessionStateOptions{Action: SessionStateActionSave, Origins: []string{"https://app.example.com"}})
			return err
		},
		"brw_state restore": func() error {
			_, err := m.SessionState(ctx, SessionStateOptions{Action: SessionStateActionRestore, SnapshotID: "snap-1"})
			return err
		},
		"brw_state list": func() error {
			_, err := m.SessionState(ctx, SessionStateOptions{Action: SessionStateActionList})
			return err
		},
		"brw_state delete": func() error {
			_, err := m.SessionState(ctx, SessionStateOptions{Action: SessionStateActionDelete, SnapshotID: "snap-1"})
			return err
		},
		"brw_downloads": func() error { _, err := m.Downloads(ctx); return err },
		"brw_set_download_path": func() error {
			_, err := m.SetDownloadPath(ctx, DownloadPathOptions{Path: t.TempDir()})
			return err
		},
		"brw_upload_file": func() error {
			_, err := m.UploadFile(ctx, snapshot.UploadOptions{Ref: "e1", Paths: []string{"/tmp/fixture-upload"}})
			return err
		},
		"brw_clipboard":          func() error { _, err := m.Clipboard(ctx, ClipboardOptions{Action: "read"}); return err },
		"download staging clean": func() error { _, err := m.CleanupManagedDownload(DownloadEntry{GUID: "d1"}); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, ErrRemoteTargetUnsupported) {
				t.Fatalf("%s on a --remote endpoint off this machine = %v, want the named refusal", name, err)
			}
		})
	}
}

// configFieldsThatDecideWhereTheBrowserIs classifies every field of Config by
// whether it can move the browser off this machine. A field added to Config and
// left out of this map fails the test: "where is the browser?" is the question
// every capability gate reads, and a new way of naming one that nothing
// classifies is how --remote stayed unclassified for as long as it did.
var configFieldsThatDecideWhereTheBrowserIs = map[string]bool{
	"Remote":    true,
	"RemoteURL": true,
	// The endpoint New actually dials when it is set, so it decides where the
	// browser is exactly as RemoteURL does. A config carrying only this one
	// would otherwise read as the empty-endpoint case — "brw launched it" —
	// whatever host the URL names.
	"BrowserWSURL": true,
	// Everything else describes a browser brw launches HERE, so it cannot move
	// one somewhere else. Several are refused outright alongside a browser that
	// is elsewhere; that is checkRemoteConfig's job, not this classification's.
	"ChromePath":       false,
	"UserDataDir":      false,
	"ProfileDirectory": false,
	"Port":             false,
	"Extensions":       false,
	"ChromeArgs":       false,
	"Timeout":          false,
	"WebMCP":           false,
	"AllowRealProfile": false,
	"Headless":         false,
	"Network":          false,
	// Neither of these names a machine. AttachOnly forbids launching one, and
	// SignedInProfile is a claim about whose browser it is, not where it runs.
	// Both are refused alongside a browser that is elsewhere, which is
	// ProviderConfigProblems' job rather than this classification's.
	"AttachOnly":      false,
	"SignedInProfile": false,
}

func TestEveryConfigFieldIsClassifiedForWhereTheBrowserIs(t *testing.T) {
	configType := reflect.TypeOf(Config{})
	for index := 0; index < configType.NumField(); index++ {
		field := configType.Field(index)
		decides, ok := configFieldsThatDecideWhereTheBrowserIs[field.Name]
		if !ok {
			t.Errorf("Config.%s is unclassified: say whether it can name a browser on another machine, and if it can, read it in Config.BrowserOnThisHost", field.Name)
			continue
		}
		probe := Config{}
		reflect.ValueOf(&probe).Elem().Field(index).Set(probeValueForField(t, field))
		onHost := probe.BrowserOnThisHost()
		if decides && onHost {
			t.Errorf("Config.%s is classified as naming a browser elsewhere, but setting it leaves BrowserOnThisHost() true", field.Name)
		}
		if !decides && !onHost {
			t.Errorf("Config.%s is classified as a launch setting, but setting it alone moved the browser off this machine", field.Name)
		}
	}
	for field := range configFieldsThatDecideWhereTheBrowserIs {
		if _, ok := configType.FieldByName(field); !ok {
			t.Errorf("configFieldsThatDecideWhereTheBrowserIs names %q, which is not a field of Config", field)
		}
	}
}

// probeValueForField is a non-zero value for one Config field. Strings that
// name an endpoint get one on another machine, because that is the value whose
// classification is in question.
func probeValueForField(t *testing.T, field reflect.StructField) reflect.Value {
	t.Helper()
	switch field.Type.Kind() {
	case reflect.String:
		return reflect.ValueOf(laneEndpointElsewhere)
	case reflect.Bool:
		return reflect.ValueOf(true)
	case reflect.Int, reflect.Int64:
		return reflect.ValueOf(int64(9222)).Convert(field.Type)
	case reflect.Slice:
		return reflect.ValueOf([]string{"/tmp/fixture"}).Convert(field.Type)
	case reflect.Pointer:
		return reflect.ValueOf(&RemoteTarget{WebSocketURL: "wss://browsers.example/devtools/browser/x"})
	case reflect.Struct:
		return reflect.ValueOf(cdplaunch.NetworkEnvironment{ProxyServer: "http://127.0.0.1:8080"})
	default:
		t.Fatalf("Config.%s is a %s, which this probe cannot build; add a case", field.Name, field.Type)
		return reflect.Value{}
	}
}

// The endpoint that authenticates a provider session must never leave the
// manager. RemoteSession is what /health, the startup log and brw_identity all
// read, and the path of a CDP websocket URL is a bearer token - Chrome's own
// /devtools/browser/ path, plus whatever key a hosted provider carries in the
// query.
//
// Built through the real constructor with no RedactedURL supplied, so the value
// asserted on is the one production derives rather than one the test handed in.
func TestRemoteSessionReportsTheRedactedEndpoint(t *testing.T) {
	const secretPath = "/devtools/browser/8f3c-fixture-session"
	m := managerForLane(t, Config{Remote: &RemoteTarget{
		WebSocketURL: "wss://browsers.example" + secretPath + "?token=fixture-provider-key-two",
		ProviderID:   "fixture.provider",
		SessionID:    "sess-1",
	}})
	session, ok := m.RemoteSession()
	if !ok {
		t.Fatal("a manager driving a plugin-supplied browser reported no session")
	}
	if session.Endpoint != "wss://browsers.example" {
		t.Fatalf("endpoint = %q, want the redacted scheme://host form", session.Endpoint)
	}
	for _, secret := range []string{secretPath, "8f3c-fixture-session", "fixture-provider-key-two"} {
		if strings.Contains(session.Endpoint, secret) {
			t.Fatalf("endpoint = %q, which carries %q - the part that authenticates the socket", session.Endpoint, secret)
		}
	}
	// A browser on this machine has no provider session to report, so nothing
	// downstream has to distinguish an empty session object from a real one.
	local := managerForLane(t, Config{RemoteURL: laneEndpointOnThisHost})
	if _, ok := local.RemoteSession(); ok {
		t.Fatal("a manager driving a browser beside brwd reported a provider session")
	}
}
