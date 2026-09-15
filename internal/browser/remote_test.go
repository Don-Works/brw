package browser

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/snapshot"
)

// remoteMarked is a manager with nothing behind it but the remote mark. Every
// refusal below must land BEFORE the browser is touched, which is exactly what
// calling these methods on a manager with no browser at all proves: a check
// placed after the first CDP round trip would hang or nil-panic here.
func remoteMarked() *Manager {
	return &Manager{remote: &RemoteTarget{ProviderID: "fixture.provider", SessionID: "sess-1", RedactedURL: "wss://browsers.example"}}
}

// refusedSurface is the classification of every browser-surface method that
// cannot work on a plugin-supplied browser, with the capability key that names
// the reason. The enumeration test below fails on any method of the interfaces
// it walks that is in neither this map nor remoteSafeSurface.
//
// This is the "table is the gate" discipline: the reason downloads were refused
// and set_download_path was not, in the first draft of this work, is that
// nothing enumerated the pair.
var refusedSurface = map[string]string{
	"UploadFile":      "local_upload",
	"Downloads":       "local_downloads",
	"SetDownloadPath": "local_downloads",
	"Clipboard":       "local_clipboard",
	// CheckProfileSession is the refusal itself rather than a refused verb: it
	// exists to answer "no" on a remote target.
	"CheckProfileSession": "profile_session",
}

// remoteSafeSurface names every other method of those interfaces, and says
// nothing more than "this is CDP, and CDP works the same over a socket to
// another machine". A method here is a deliberate decision that it does not
// touch this machine.
var remoteSafeSurface = []string{
	"AssertHidden", "AssertText", "AssertValue", "AssertVisible",
	"Cancel", "ClearTrace", "Click", "ClickButton", "ClickText", "ClickXY",
	"CloseContext", "CloseTab", "CommitField", "ConsoleMessages", "Cookies",
	"Dialog", "DocumentIdentity", "Drag", "EmulateDevice", "EmulateMedia",
	"Evaluate", "ExecuteBatch", "ExecutePlan", "Find", "FindLive", "Focus",
	"FocusTab", "GetTrace", "GroupTabs", "Hover", "KeyDown", "KeyUp",
	"ListTabGroups", "ListTabs", "MouseDown", "MouseUp", "Navigate",
	"NavigateTo", "NetworkCapture", "NetworkRequests", "Notify", "Observe",
	"Open", "OpenInGroup", "OpenIncognito", "Press", "PushState", "Read",
	"ReadData", "ReplayRequest", "ResizeWindow", "Route", "Screenshot",
	"ScreenshotAnnotated", "ScreenshotElement", "Scroll", "Select",
	"SessionState", "SetExtraHeaders", "SetGeolocation", "SetNetworkConditions",
	"SetUserAgent", "Snapshot", "Type", "UngroupTabs", "WaitFor",
	"WaitForOutcome", "WindowBounds", "Authenticate", "Fill", "CheckRouteReplay",
}

// remoteSurfaceInterfaces is the domain the enumeration walks: the transport
// contract plus every optional capability a direct-CDP manager also serves. A
// method added to any of them, or a new sibling verb, fails the test until
// somebody decides which half it belongs in.
func remoteSurfaceInterfaces() map[string]reflect.Type {
	return map[string]reflect.Type{
		"Controller":               reflect.TypeOf((*Controller)(nil)).Elem(),
		"EnvironmentController":    reflect.TypeOf((*EnvironmentController)(nil)).Elem(),
		"ClipboardController":      reflect.TypeOf((*ClipboardController)(nil)).Elem(),
		"KeyHoldController":        reflect.TypeOf((*KeyHoldController)(nil)).Elem(),
		"HistoryController":        reflect.TypeOf((*HistoryController)(nil)).Elem(),
		"SessionStateController":   reflect.TypeOf((*SessionStateController)(nil)).Elem(),
		"DialogController":         reflect.TypeOf((*DialogController)(nil)).Elem(),
		"RouteController":          reflect.TypeOf((*RouteController)(nil)).Elem(),
		"RouteReplayer":            reflect.TypeOf((*RouteReplayer)(nil)).Elem(),
		"ElementFocuser":           reflect.TypeOf((*ElementFocuser)(nil)).Elem(),
		"WaitObserver":             reflect.TypeOf((*WaitObserver)(nil)).Elem(),
		"DocumentIdentityProvider": reflect.TypeOf((*DocumentIdentityProvider)(nil)).Elem(),
		"ProfileSessionController": reflect.TypeOf((*ProfileSessionController)(nil)).Elem(),
	}
}

func TestEveryBrowserSurfaceMethodIsClassifiedForARemoteTarget(t *testing.T) {
	seen := map[string]bool{}
	for name, iface := range remoteSurfaceInterfaces() {
		for index := 0; index < iface.NumMethod(); index++ {
			method := iface.Method(index).Name
			seen[method] = true
			_, refused := refusedSurface[method]
			safe := slices.Contains(remoteSafeSurface, method)
			switch {
			case refused && safe:
				t.Errorf("%s.%s is classified both refused and safe on a remote target", name, method)
			case !refused && !safe:
				t.Errorf("%s.%s is unclassified for a remote target: put it in refusedSurface with a capability key, or in remoteSafeSurface because it does not touch this machine", name, method)
			}
		}
	}
	// The reverse direction, so a rename leaves a stale row rather than a
	// silently unenforced one.
	for method := range refusedSurface {
		if !seen[method] {
			t.Errorf("refusedSurface names %q, which is not a method of any browser surface interface", method)
		}
	}
	for _, method := range remoteSafeSurface {
		if !seen[method] {
			t.Errorf("remoteSafeSurface names %q, which is not a method of any browser surface interface", method)
		}
	}
}

// Every capability key in the gate table has to be REACHED by something. A row
// nobody looks up is a reason nobody will ever read, and an operator would
// believe a boundary exists that no code applies.
func TestEveryRemoteCapabilityKeyIsEnforcedSomewhere(t *testing.T) {
	// Enforced when the daemon starts rather than on a verb, because they
	// describe a configuration rather than a call. cmd/brwd refuses the launch;
	// checkRemoteConfig refuses the manager.
	startupEnforced := []string{"profile_reuse", "extension_bridge"}
	reached := map[string]bool{}
	for _, capability := range startupEnforced {
		reached[capability] = true
	}
	for _, capability := range refusedSurface {
		reached[capability] = true
	}
	for _, capability := range RemoteCapabilityNames() {
		if !reached[capability] {
			t.Errorf("RemoteUnavailable declares %q and nothing enforces it; a reason nobody looks up is a boundary that does not exist", capability)
		}
		if why := RemoteUnavailable[capability]; len(why) < 40 {
			t.Errorf("RemoteUnavailable[%q] = %q; the value is what an agent reads to decide what to do next", capability, why)
		}
	}
	for capability := range reached {
		if _, ok := RemoteUnavailable[capability]; !ok {
			t.Errorf("%q is enforced and has no row in RemoteUnavailable, so its refusal has no written reason", capability)
		}
	}
	// checkRemoteConfig is the half the enumeration cannot reach by reflection,
	// so it is exercised directly: profile reuse must be refused, by name, for
	// both spellings of a profile.
	for _, cfg := range []Config{
		{Remote: &RemoteTarget{WebSocketURL: "ws://127.0.0.1:1/x"}, UserDataDir: "/tmp/fixture-profile"},
		{Remote: &RemoteTarget{WebSocketURL: "ws://127.0.0.1:1/x"}, ProfileDirectory: "Profile 1"},
	} {
		err := checkRemoteConfig(cfg)
		if !errors.Is(err, ErrRemoteTargetUnsupported) || !strings.Contains(err.Error(), "profile reuse") {
			t.Errorf("checkRemoteConfig(%+v) = %v, want the named profile-reuse refusal", cfg, err)
		}
	}
}

// Each refused verb must answer with the named class, from the mark alone,
// before any browser work. A verb that reached the browser first would fail
// with a CDP error on a live remote session and look like a transport problem.
func TestRefusedVerbsAnswerWithTheNamedCapabilityClass(t *testing.T) {
	ctx := context.Background()
	calls := map[string]struct {
		capability string
		call       func(*Manager) error
	}{
		"UploadFile": {"local_upload", func(m *Manager) error {
			_, err := m.UploadFile(ctx, snapshot.UploadOptions{Ref: "e1", Paths: []string{"/tmp/fixture.txt"}})
			return err
		}},
		"Downloads": {"local_downloads", func(m *Manager) error {
			_, err := m.Downloads(ctx)
			return err
		}},
		"SetDownloadPath": {"local_downloads", func(m *Manager) error {
			_, err := m.SetDownloadPath(ctx, DownloadPathOptions{Path: "/tmp/fixture-downloads"})
			return err
		}},
		"Clipboard": {"local_clipboard", func(m *Manager) error {
			_, err := m.Clipboard(ctx, ClipboardOptions{Action: "read"})
			return err
		}},
		// The sibling a per-verb guard misses. A `download:` wait reaches the
		// same local bookkeeping without going through either download verb,
		// which is why the guard sits at ensureDownloadTracking rather than on
		// the two verbs somebody thought of first.
		"WaitFor download:": {"local_downloads", func(m *Manager) error {
			return m.WaitFor(ctx, "download:invoice.pdf", time.Second)
		}},
		"CheckProfileSession": {"profile_session", func(m *Manager) error { return m.CheckProfileSession() }},
	}
	// Every refused method has a call here, so adding a row to refusedSurface
	// without proving it refuses fails.
	for method, capability := range refusedSurface {
		entry, ok := calls[method]
		if !ok {
			t.Fatalf("refusedSurface names %q with no call in this test, so nothing proves it refuses", method)
		}
		if entry.capability != capability {
			t.Fatalf("refusedSurface[%q] = %q and this test expects %q", method, capability, entry.capability)
		}
	}
	for method, entry := range calls {
		t.Run(method, func(t *testing.T) {
			err := entry.call(remoteMarked())
			if !errors.Is(err, ErrRemoteTargetUnsupported) {
				t.Fatalf("%s on a remote target = %v, want ErrRemoteTargetUnsupported", method, err)
			}
			want := RemoteUnavailable[entry.capability]
			if want == "" {
				t.Fatalf("%s is checked against capability %q, which has no row in RemoteUnavailable", method, entry.capability)
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s refusal = %q, want it to carry the declared reason %q", method, err, want)
			}
		})
	}
}

// The same verbs must keep working on a local browser: a guard that refused
// everywhere would pass the test above and break brw.
func TestRefusedVerbsAreNotRefusedOnALocalBrowser(t *testing.T) {
	local := &Manager{}
	if err := local.CheckProfileSession(); err != nil {
		t.Fatalf("CheckProfileSession on a local browser = %v, want nil", err)
	}
	for _, capability := range RemoteCapabilityNames() {
		if err := local.refuseOnRemote(capability); err != nil {
			t.Fatalf("refuseOnRemote(%q) on a local browser = %v, want nil", capability, err)
		}
	}
}

// An unnamed refusal is the silent degrade this file exists to prevent, so a
// capability key that is not in the table is a programming error rather than a
// vague message.
func TestRemoteUnavailableErrorRefusesAnUndeclaredCapability(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("RemoteUnavailableError accepted an undeclared capability")
		}
	}()
	_ = RemoteUnavailableError("something_nobody_declared")
}

func TestCheckRemoteConfigRefusesLaunchOnlySettings(t *testing.T) {
	remote := func() *RemoteTarget { return &RemoteTarget{WebSocketURL: "ws://127.0.0.1:1/devtools/browser/x"} }
	for name, test := range map[string]struct {
		cfg     Config
		wantErr string
	}{
		"clean":           {Config{Remote: remote()}, ""},
		"no url":          {Config{Remote: &RemoteTarget{}}, "no websocket URL"},
		"remote flag too": {Config{Remote: remote(), RemoteURL: "http://127.0.0.1:9222"}, "which browser you meant"},
		"user data dir":   {Config{Remote: remote(), UserDataDir: "/tmp/fixture"}, "profile reuse"},
		"profile dir":     {Config{Remote: remote(), ProfileDirectory: "Profile 1"}, "profile reuse"},
		"extension":       {Config{Remote: remote(), Extensions: []string{"/tmp/fixture-ext"}}, "unpacked extension"},
		"chrome arg":      {Config{Remote: remote(), ChromeArgs: []string{"--mute-audio"}}, "launch settings"},
		"headless":        {Config{Remote: remote(), Headless: true}, "launch settings"},
		"debugging port":  {Config{Remote: remote(), Port: 9222}, "launch settings"},
		"real profile":    {Config{Remote: remote(), AllowRealProfile: true}, "launch settings"},
		"proxy":           {Config{Remote: remote(), Network: cdp.NetworkEnvironment{ProxyServer: "http://127.0.0.1:8080"}}, "launch switches"},
		"ignore https":    {Config{Remote: remote(), Network: cdp.NetworkEnvironment{IgnoreHTTPSErrors: true}}, "launch switches"},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkRemoteConfig(test.cfg)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("checkRemoteConfig = %v, want it accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("checkRemoteConfig = %v, want an error containing %q", err, test.wantErr)
			}
		})
	}
}

// A provider states how long its browser lives. Past that the browser is gone,
// and every operation would fail with an unattributable websocket error. Both
// funnels every operation passes through name it instead.
func TestAnExpiredProviderSessionIsNamedAtBothFunnels(t *testing.T) {
	expired := &Manager{remote: &RemoteTarget{
		ProviderID: "fixture.provider",
		SessionID:  "sess-1",
		ExpiresAt:  time.Now().Add(-time.Minute),
	}}
	if _, err := expired.tabContext("tab-1"); !errors.Is(err, ErrRemoteSessionExpired) {
		t.Fatalf("tabContext on an expired session = %v, want ErrRemoteSessionExpired", err)
	}
	if err := expired.runBrowser(context.Background(), func(context.Context) error {
		t.Fatal("runBrowser executed work on an expired session")
		return nil
	}); !errors.Is(err, ErrRemoteSessionExpired) {
		t.Fatalf("runBrowser on an expired session = %v, want ErrRemoteSessionExpired", err)
	}
	live := &Manager{remote: &RemoteTarget{ExpiresAt: time.Now().Add(time.Hour)}}
	if err := live.checkRemoteSession(); err != nil {
		t.Fatalf("a live session = %v, want nil", err)
	}
	local := &Manager{}
	if err := local.checkRemoteSession(); err != nil {
		t.Fatalf("a local browser = %v, want nil", err)
	}
}

// The dialer quotes the URL it could not reach. On a remote target the path of
// that URL is what authenticates the connection, so it must not survive into an
// error anyone logs.
func TestConnectErrorsDoNotCarryTheSessionURL(t *testing.T) {
	const secretPath = "/devtools/browser/fixture-session-token"
	m := &Manager{remote: &RemoteTarget{
		WebSocketURL: "wss://browsers.example" + secretPath + "?token=fixture-provider-key-two",
		RedactedURL:  "wss://browsers.example",
	}}
	raw := fmt.Errorf("dial %s: connection refused", m.remote.WebSocketURL)
	scrubbed := m.scrubRemoteEndpoint(raw)
	if strings.Contains(scrubbed.Error(), secretPath) || strings.Contains(scrubbed.Error(), "fixture-provider-key-two") {
		t.Fatalf("scrubbed error = %q, still carrying the session URL", scrubbed)
	}
	if !strings.Contains(scrubbed.Error(), "browsers.example") {
		t.Fatalf("scrubbed error = %q; the host has to survive or the message names nothing", scrubbed)
	}
	// Not wrapped: the original's own Error() still holds the URL, so a chain
	// would keep a live copy for anything walking Unwrap.
	if errors.Is(scrubbed, raw) {
		t.Fatal("the scrubbed error wraps the original, which still carries the URL")
	}
	// An error that never contained it keeps its chain.
	other := errors.New("some other failure")
	if got := m.scrubRemoteEndpoint(other); !errors.Is(got, other) {
		t.Fatal("an unrelated error lost its chain")
	}
}

// A caller that built a RemoteTarget without a redacted spelling must not end
// up with a scrub that replaces the URL with nothing: the resulting message
// would read as a failure against no host at all.
func TestARemoteTargetWithoutARedactedURLStillRedacts(t *testing.T) {
	const raw = "wss://browsers.example/devtools/browser/fixture-session?token=fixture-provider-key-two"
	cfg := Config{Remote: &RemoteTarget{WebSocketURL: raw}}
	if err := checkRemoteConfig(cfg); err != nil {
		t.Fatalf("checkRemoteConfig = %v", err)
	}
	if cfg.Remote.RedactedURL != "wss://browsers.example" {
		t.Fatalf("derived redaction = %q", cfg.Remote.RedactedURL)
	}
	m := &Manager{remote: cfg.Remote}
	scrubbed := m.scrubRemoteEndpoint(errors.New("dial " + raw + ": refused"))
	if strings.Contains(scrubbed.Error(), "fixture-session") || strings.Contains(scrubbed.Error(), "fixture-provider-key-two") {
		t.Fatalf("scrubbed = %q", scrubbed)
	}
	if !strings.Contains(scrubbed.Error(), "wss://browsers.example") {
		t.Fatalf("scrubbed = %q, want the host to survive", scrubbed)
	}
	for _, malformed := range []string{"", "not-a-url", "wss://"} {
		if got := redactWebSocketURL(malformed); strings.Contains(got, "://") && strings.Count(got, "/") > 2 {
			t.Errorf("redactWebSocketURL(%q) = %q", malformed, got)
		}
	}
}

func TestRemoteCapabilityNamesIsSorted(t *testing.T) {
	names := RemoteCapabilityNames()
	if !sort.StringsAreSorted(names) {
		t.Fatalf("RemoteCapabilityNames() = %v, want sorted so error text is stable", names)
	}
	if len(names) != len(RemoteUnavailable) {
		t.Fatalf("RemoteCapabilityNames() has %d entries for %d rows", len(names), len(RemoteUnavailable))
	}
}
