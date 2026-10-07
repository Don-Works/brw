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

func remoteMarked() *Manager {
	return &Manager{remote: &RemoteTarget{ProviderID: "fixture.provider", SessionID: "sess-1", RedactedURL: "wss://browsers.example"}}
}

var refusedSurface = map[string]string{
	"SaveScreenshot":  "local_screenshot_files",
	"UploadFile":      "local_upload",
	"Downloads":       "local_downloads",
	"SetDownloadPath": "local_downloads",
	"Clipboard":       "local_clipboard",

	"CleanupManagedDownload": "local_downloads",

	"SessionState": "local_session_state",

	"CheckProfileSession": "profile_session",
}

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
	"ReloadPage",
	"ScreenshotAnnotated", "ScreenshotElement", "Scroll", "Select",
	"SetExtraHeaders", "SetGeolocation", "SetNetworkConditions",
	"SetUserAgent", "Snapshot", "Type", "UngroupTabs", "WaitFor",
	"WaitForOutcome", "WindowBounds", "Authenticate", "Fill", "CheckRouteReplay",

	"SetLocale", "InitScript", "Touch", "Profile", "React", "ScrollTo", "Check",

	"ActiveTabID", "Assert", "AssertValueContains", "ReadWindow",
	"AccessibilityAudit", "BlockedRequests", "FocusRef", "Highlight", "Vitals",

	"CaptureArtifactScreenshot", "CapturePDF", "CapturePDFStream",

	"AcquireTakeover", "DispatchTakeoverInput", "ReleaseTakeover",
	"RenewTakeover", "ScreencastFrames", "TakeoverState", "SubscribeTrace",
}

var remoteManagerPlumbing = []string{
	"CheckBackgroundTab", "ReleaseBackgroundTab", "SetTabAccessGuard",
	"Close", "ContentNavigationGuard", "Remote", "RemoteSession",
	"SetContentNavigationGuard", "SetNavigationPolicy", "SetSessionStateStore", "Pacing", "SetPacing",

	"BrowserOnThisHost",
}

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
		"PageReloader":             reflect.TypeOf((*PageReloader)(nil)).Elem(),
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

func classifyRemoteMethod(method string) (refused, safe, plumbing bool) {
	_, refused = refusedSurface[method]
	return refused, slices.Contains(remoteSafeSurface, method), slices.Contains(remoteManagerPlumbing, method)
}

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

func TestEveryRemoteCapabilityKeyIsEnforcedSomewhere(t *testing.T) {

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

func TestRefusedVerbsAnswerWithTheNamedCapabilityClass(t *testing.T) {
	ctx := context.Background()
	calls := map[string]struct {
		capability string
		call       func(*Manager) error
	}{
		"SaveScreenshot": {"local_screenshot_files", func(m *Manager) error {
			_, err := m.SaveScreenshot(ctx, ScreenshotSaveOptions{SavePath: "/tmp/capture.png"})
			return err
		}},
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

		"WaitFor download:": {"local_downloads", func(m *Manager) error {
			return m.WaitFor(ctx, "download:invoice.pdf", time.Second)
		}},
		"CleanupManagedDownload": {"local_downloads", func(m *Manager) error {
			_, err := m.CleanupManagedDownload(DownloadEntry{GUID: "fixture-guid", Path: "/tmp/fixture-downloads/fixture-guid"})
			return err
		}},

		"SessionState": {"local_session_state", func(m *Manager) error {
			for _, opts := range []SessionStateOptions{
				{Action: SessionStateActionSave, Origins: []string{"https://app.example.com"}},
				{Action: SessionStateActionRestore, SnapshotID: "snap-1", Origins: []string{"https://app.example.com"}},
				{Action: SessionStateActionList},
				{Action: SessionStateActionDelete, SnapshotID: "snap-1"},

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

func mustRefuse(m *Manager, opts SessionStateOptions) error {
	_, err := m.SessionState(context.Background(), opts)
	if errors.Is(err, ErrRemoteTargetUnsupported) {
		return nil
	}
	return fmt.Errorf("brw_state %s on a remote target = %v, want the named refusal", opts.Action, err)
}

func TestSessionStateIsRefusedOnARemoteTargetEvenWithAStoreInstalled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "snapshots")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

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

	local := &Manager{}
	local.SetSessionStateStore(store)
	if _, err := local.SessionState(context.Background(), SessionStateOptions{Action: SessionStateActionList}); err != nil {
		t.Fatalf("brw_state list on a local browser = %v, want the store consulted", err)
	}
}

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

	if errors.Is(scrubbed, raw) {
		t.Fatal("the scrubbed error wraps the original, which still carries the URL")
	}

	other := errors.New("some other failure")
	if got := m.scrubRemoteEndpoint(other); !errors.Is(got, other) {
		t.Fatal("an unrelated error lost its chain")
	}
}

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
	for _, tc := range []struct{ raw, want string }{
		{"", "the remote browser endpoint"},
		{"not-a-url", "the remote browser endpoint"},
		{"wss://", "the remote browser endpoint"},
		{"wss://browsers.example#fixture-provider-token", "wss://browsers.example"},
		{"wss://fixture-user:fixture-password@browsers.example/devtools/browser/x", "the remote browser endpoint"},
		{"wss://[::1]:9222/devtools/browser/x?token=fixture-token", "wss://[::1]:9222"},
	} {
		if got := redactWebSocketURL(tc.raw); got != tc.want {
			t.Errorf("redactWebSocketURL(%q) = %q, want %q", tc.raw, got, tc.want)
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
