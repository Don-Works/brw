package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/sessionstate"
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
	// The cleanup half of the download pair. It deletes from the manager's own
	// staging directory on this machine, and a remote target never gets one, so
	// the guard is unreachable today and costs a nil comparison to keep honest.
	"CleanupManagedDownload": "local_downloads",
	// All four actions, not restore alone. The store holds sessions a human
	// signed into on THIS machine; restore would put them on the provider's
	// host, list would enumerate them to a cloud-backed run and delete would
	// destroy them.
	"SessionState": "local_session_state",
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
	"SetExtraHeaders", "SetGeolocation", "SetNetworkConditions",
	"SetUserAgent", "Snapshot", "Type", "UngroupTabs", "WaitFor",
	"WaitForOutcome", "WindowBounds", "Authenticate", "Fill", "CheckRouteReplay",
	// Added with the 2026-09 Vercel-parity wave. All six are CDP over the same
	// socket: locale/touch/scroll are renderer and input commands, init scripts
	// and React introspection run in the page, and a profile is bytes Chrome
	// sends back for brw to write HERE. None names a path or a store on the
	// browser's host, so all six are safe against somebody else's browser.
	"SetLocale", "InitScript", "Touch", "Profile", "React", "ScrollTo", "Check",
	// Reached through the optional capabilities and through *Manager itself.
	// Assert is safe because its own refusals come from the primitives it is
	// built on: a download assertion resolves through AssertSource.Downloads,
	// which is refused by name above.
	"ActiveTabID", "Assert", "AssertValueContains", "ReadWindow",
	"AccessibilityAudit", "BlockedRequests", "FocusRef", "Highlight", "Vitals",
	// Artifact capture is CDP asking the browser for bytes and brw writing them
	// here, so the file lands on this machine either way.
	"CaptureArtifactScreenshot", "CapturePDF", "CapturePDFStream",
	// Human takeover is a CDP screencast out and CDP input back. Neither end
	// touches the machine the browser runs on.
	"AcquireTakeover", "DispatchTakeoverInput", "ReleaseTakeover",
	"RenewTakeover", "ScreencastFrames", "TakeoverState", "SubscribeTrace",
}

// remoteManagerPlumbing names the exported *Manager methods that are not a
// browser verb: daemon wiring a transport is configured with, or a question
// about the target rather than a request to the page. They are a third bucket
// rather than "safe", because "this never reaches the page" and "this is safe
// to run against somebody else's browser" are different statements and a method
// filed under the wrong one reads as a decision nobody made.
var remoteManagerPlumbing = []string{
	"Close", "ContentNavigationGuard", "Remote", "RemoteSession",
	"SetContentNavigationGuard", "SetNavigationPolicy", "SetSessionStateStore",
	// BrowserOnThisHost is the gate's own question, not a verb it guards.
	"BrowserOnThisHost",
}

// remoteSurfaceInterfaces is half the domain the enumeration walks: every
// browser-surface interface declared in this package - the transport contract
// plus each optional capability a transport may also serve. A method added to
// any of them fails the test until somebody decides which bucket it belongs in.
//
// It is only half because an interface can only be listed here if this package
// declares it, and the ones internal/http and internal/artifact declare for
// their own use (the screencast, takeover and trace-subscription surfaces, the
// PDF capture pair) are satisfied by *Manager without appearing anywhere in
// this file. The first draft of this table claimed to walk "every optional
// capability" and walked thirteen interfaces, so AssertSource.Downloads and
// CapturePDF were unclassified with nothing turning red. The other half is
// TestEveryManagerMethodIsClassifiedForARemoteTarget, which walks *Manager's own
// exported method set and therefore catches an interface declared anywhere.
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
		"WindowReader":             reflect.TypeOf((*WindowReader)(nil)).Elem(),
		"ActiveTabReporter":        reflect.TypeOf((*ActiveTabReporter)(nil)).Elem(),
		"AssertSource":             reflect.TypeOf((*AssertSource)(nil)).Elem(),
		"Asserter":                 reflect.TypeOf((*Asserter)(nil)).Elem(),
		"FindActFinder":            reflect.TypeOf((*FindActFinder)(nil)).Elem(),
		"LiveFinder":               reflect.TypeOf((*LiveFinder)(nil)).Elem(),
		"FindActController":        reflect.TypeOf((*FindActController)(nil)).Elem(),
		"InitScriptController":     reflect.TypeOf((*InitScriptController)(nil)).Elem(),
		"TouchController":          reflect.TypeOf((*TouchController)(nil)).Elem(),
		"ProfilerController":       reflect.TypeOf((*ProfilerController)(nil)).Elem(),
		"ReactController":          reflect.TypeOf((*ReactController)(nil)).Elem(),
		"ScrollToController":       reflect.TypeOf((*ScrollToController)(nil)).Elem(),
		"CheckController":          reflect.TypeOf((*CheckController)(nil)).Elem(),
	}
}

// classifyRemoteMethod reports which bucket a method name is in, and whether it
// is in more than one.
func classifyRemoteMethod(method string) (refused, safe, plumbing bool) {
	_, refused = refusedSurface[method]
	return refused, slices.Contains(remoteSafeSurface, method), slices.Contains(remoteManagerPlumbing, method)
}

// The other half of the domain: *Manager's own exported method set. An
// interface declared in another package (internal/http's screencast, takeover
// and trace surfaces; internal/artifact's PDF capture pair) can only be
// satisfied by methods that appear here, so walking this catches a capability
// added anywhere without this package having to know about it.
func TestEveryManagerMethodIsClassifiedForARemoteTarget(t *testing.T) {
	manager := reflect.TypeOf((*Manager)(nil))
	for index := 0; index < manager.NumMethod(); index++ {
		method := manager.Method(index).Name
		refused, safe, plumbing := classifyRemoteMethod(method)
		count := 0
		for _, in := range []bool{refused, safe, plumbing} {
			if in {
				count++
			}
		}
		switch count {
		case 1:
		case 0:
			t.Errorf("Manager.%s is unclassified for a remote target: refusedSurface with a capability key, remoteSafeSurface because it does not touch this machine, or remoteManagerPlumbing because it is not a browser verb", method)
		default:
			t.Errorf("Manager.%s is classified in more than one bucket", method)
		}
	}
	// A rename leaves a stale row rather than a silently unenforced one.
	for _, method := range remoteManagerPlumbing {
		if _, ok := manager.MethodByName(method); !ok {
			t.Errorf("remoteManagerPlumbing names %q, which is not a method of *Manager", method)
		}
	}
}

func TestEveryBrowserSurfaceMethodIsClassifiedForARemoteTarget(t *testing.T) {
	seen := map[string]bool{}
	for name, iface := range remoteSurfaceInterfaces() {
		for index := 0; index < iface.NumMethod(); index++ {
			method := iface.Method(index).Name
			seen[method] = true
			refused, safe, plumbing := classifyRemoteMethod(method)
			switch {
			case refused && safe:
				t.Errorf("%s.%s is classified both refused and safe on a remote target", name, method)
			case plumbing:
				t.Errorf("%s.%s is filed as plumbing but is part of a browser surface interface, so somebody can call it as a verb", name, method)
			case !refused && !safe:
				t.Errorf("%s.%s is unclassified for a remote target: put it in refusedSurface with a capability key, or in remoteSafeSurface because it does not touch this machine", name, method)
			}
		}
	}
	// The reverse direction, so a rename leaves a stale row rather than a
	// silently unenforced one. The domain is both halves: a method may be on
	// *Manager without being on any interface this package declares.
	manager := reflect.TypeOf((*Manager)(nil))
	for index := 0; index < manager.NumMethod(); index++ {
		seen[manager.Method(index).Name] = true
	}
	for method := range refusedSurface {
		if !seen[method] {
			t.Errorf("refusedSurface names %q, which is not a method of any browser surface", method)
		}
	}
	for _, method := range remoteSafeSurface {
		if !seen[method] {
			t.Errorf("remoteSafeSurface names %q, which is not a method of any browser surface", method)
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
		"CleanupManagedDownload": {"local_downloads", func(m *Manager) error {
			_, err := m.CleanupManagedDownload(DownloadEntry{GUID: "fixture-guid", Path: "/tmp/fixture-downloads/fixture-guid"})
			return err
		}},
		// One subtest per action, because the finding this closes was a guard
		// that covered save and left restore - the action that puts a session a
		// human signed into here onto somebody else's host - open.
		"SessionState": {"local_session_state", func(m *Manager) error {
			for _, opts := range []SessionStateOptions{
				{Action: SessionStateActionSave, Origins: []string{"https://app.example.com"}},
				{Action: SessionStateActionRestore, SnapshotID: "snap-1", Origins: []string{"https://app.example.com"}},
				{Action: SessionStateActionList},
				{Action: SessionStateActionDelete, SnapshotID: "snap-1"},
				// And a malformed one, so the refusal does not depend on the
				// request validating first.
				{Action: "restore"},
			} {
				err := mustRefuse(m, opts)
				if err != nil {
					return err
				}
			}
			_, err := m.SessionState(context.Background(), SessionStateOptions{Action: SessionStateActionList})
			return err
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

// mustRefuse asserts one brw_state action refuses on a remote target, returning
// the refusal so the caller can go on checking its class and reason.
func mustRefuse(m *Manager, opts SessionStateOptions) error {
	_, err := m.SessionState(context.Background(), opts)
	if errors.Is(err, ErrRemoteTargetUnsupported) {
		return nil
	}
	return fmt.Errorf("brw_state %s on a remote target = %v, want the named refusal", opts.Action, err)
}

// The guard must land before the store is consulted, not as a fallback when a
// provider-backed daemon happens to have none installed. A daemon started with
// --state-key-file has a real store, and that is the configuration where a
// restore would have had something to replay.
func TestSessionStateIsRefusedOnARemoteTargetEvenWithAStoreInstalled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "snapshots")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// Obviously fabricated and low entropy: it seals nothing this test reads.
	store, err := sessionstate.NewStore(sessionstate.Config{Root: root, Key: []byte("fixture-session-state-key-0123456789ab")})
	if err != nil {
		t.Fatalf("open the snapshot store: %v", err)
	}
	m := remoteMarked()
	m.SetSessionStateStore(store)
	for _, action := range []string{SessionStateActionSave, SessionStateActionRestore, SessionStateActionList, SessionStateActionDelete} {
		opts := SessionStateOptions{Action: action, SnapshotID: "snap-1", Origins: []string{"https://app.example.com"}}
		if _, err := m.SessionState(context.Background(), opts); !errors.Is(err, ErrRemoteTargetUnsupported) {
			t.Errorf("brw_state %s with a store installed = %v, want the named refusal", action, err)
		}
	}
	// And the same manager without the remote mark still reaches the store, so
	// the guard is a guard rather than a feature switch.
	local := &Manager{}
	local.SetSessionStateStore(store)
	if _, err := local.SessionState(context.Background(), SessionStateOptions{Action: SessionStateActionList}); err != nil {
		t.Fatalf("brw_state list on a local browser = %v, want the store consulted", err)
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
