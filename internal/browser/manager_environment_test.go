package browser

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/chromedp/cdproto"
	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
)

// recordingExecutor answers CDP commands from a script, so the fallback branch
// can be driven without a Chrome that has dropped the command.
type recordingExecutor struct {
	answers map[string]error
	sent    []string
}

func (e *recordingExecutor) Execute(_ context.Context, method string, _, _ any) error {
	e.sent = append(e.sent, method)
	return e.answers[method]
}

// The fallback exists because Chrome renamed the command, and it is selected on
// the JSON-RPC error code: matching Chrome's wording would turn the documented
// graceful degrade into a hard failure the day that sentence changes.
func TestApplyNetworkConditionsFallsBackOnMethodNotFound(t *testing.T) {
	const current = "Network.emulateNetworkConditions"
	fallback := string(network.CommandOverrideNetworkState)
	otherFailure := &cdproto.Error{Code: -32000, Message: "Target closed"}

	tests := []struct {
		name         string
		answers      map[string]error
		wantSent     []string
		wantDegraded bool
		wantErr      error
	}{
		{
			name:     "the current command works",
			answers:  nil,
			wantSent: []string{current},
		},
		{
			// The wording is deliberately NOT the one Chrome uses today: the code
			// is what selects the fallback.
			name:         "the current command is gone",
			answers:      map[string]error{current: &cdproto.Error{Code: -32601, Message: "no such method, reworded"}},
			wantSent:     []string{current, fallback},
			wantDegraded: true,
		},
		{
			name:         "both commands are gone",
			answers:      map[string]error{current: &cdproto.Error{Code: -32601, Message: "gone"}, fallback: &cdproto.Error{Code: -32601, Message: "gone"}},
			wantSent:     []string{current, fallback},
			wantDegraded: true,
			wantErr:      &cdproto.Error{Code: -32601, Message: "gone"},
		},
		{
			// Anything that is not method-not-found is the caller's error, not a
			// reason to send a second command to a browser that is already unhappy.
			name:     "some other protocol failure",
			answers:  map[string]error{current: otherFailure},
			wantSent: []string{current},
			wantErr:  otherFailure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &recordingExecutor{answers: tt.answers}
			ctx := cdp.WithExecutor(context.Background(), exec)
			degraded, err := applyNetworkConditions(ctx, NetworkConditionsConfig{Offline: true, DownloadThroughput: unthrottled, UploadThroughput: unthrottled})
			if degraded != tt.wantDegraded {
				t.Errorf("degraded = %v, want %v", degraded, tt.wantDegraded)
			}
			if (err != nil) != (tt.wantErr != nil) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if len(exec.sent) != len(tt.wantSent) {
				t.Fatalf("sent %v, want %v", exec.sent, tt.wantSent)
			}
			for i, method := range tt.wantSent {
				if exec.sent[i] != method {
					t.Fatalf("sent %v, want %v", exec.sent, tt.wantSent)
				}
			}
		})
	}
}

// Fetch interception is not free: every intercepted request pauses, crosses to
// the daemon and is continued from a goroutine. A tab that needed it for one
// brw_authenticate or one header table must not keep paying for it afterwards.
func TestFetchInterceptionIsOnlyEnabledWhileSomethingNeedsIt(t *testing.T) {
	const tabID = "tab-1"
	tests := []struct {
		name           string
		arrange        func(m *Manager)
		wantEnable     bool
		wantHandleAuth bool
		// wantDocumentOnly says Chrome should be told to pause top-level
		// documents and nothing else, which is all the content boundary can
		// decide.
		wantDocumentOnly bool
	}{
		{
			name:    "nothing needs it",
			arrange: func(*Manager) {},
		},
		{
			name:           "a credential is armed",
			arrange:        func(m *Manager) { m.env.armCredential(tabID, "https://api.example.com", "u", "fabricated") },
			wantEnable:     true,
			wantHandleAuth: true,
		},
		{
			name: "a header table is declared",
			arrange: func(m *Manager) {
				m.env.setHeaders(tabID, []OriginHeaders{{Origin: "https://api.example.com", Headers: map[string]string{"X-Test": "1"}}})
			},
			wantEnable: true,
		},
		{
			name: "a route is active",
			arrange: func(m *Manager) {
				m.routes.mu.Lock()
				m.routes.initLocked()
				m.routes.routes[tabID] = []*Route{{Pattern: "*"}}
				m.routes.mu.Unlock()
			},
			wantEnable: true,
		},
		{
			name:       "the navigation policy confines",
			arrange:    func(m *Manager) { m.navPolicy = navpolicy.Parse("example.com", "") },
			wantEnable: true,
		},
		{
			name: "a header table on another tab",
			arrange: func(m *Manager) {
				m.env.setHeaders("tab-2", []OriginHeaders{{Origin: "https://api.example.com", Headers: map[string]string{"X-Test": "1"}}})
			},
		},
		{
			name:             "only the content boundary needs it",
			arrange:          func(m *Manager) { m.contentNavGuard = true },
			wantEnable:       true,
			wantDocumentOnly: true,
		},
		{
			name: "the content boundary alongside a route pauses everything",
			arrange: func(m *Manager) {
				m.contentNavGuard = true
				m.routes.mu.Lock()
				m.routes.initLocked()
				m.routes.routes[tabID] = []*Route{{Pattern: "*"}}
				m.routes.mu.Unlock()
			},
			wantEnable: true,
		},
		{
			name: "the credential is dropped again",
			arrange: func(m *Manager) {
				m.env.armCredential(tabID, "https://api.example.com", "u", "fabricated")
				m.env.dropCredential(tabID)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{}
			tt.arrange(m)
			enable, handleAuth, patterns := m.fetchInterceptionCommand(tabID)
			if enable != tt.wantEnable || handleAuth != tt.wantHandleAuth {
				t.Fatalf("command = (enable %v, handleAuth %v), want (%v, %v)", enable, handleAuth, tt.wantEnable, tt.wantHandleAuth)
			}
			if !enable {
				if patterns != nil {
					t.Fatalf("interception is off but %d patterns were asked for", len(patterns))
				}
				return
			}
			if len(patterns) != 1 {
				t.Fatalf("want one pattern, got %+v", patterns)
			}
			documentOnly := patterns[0].ResourceType == network.ResourceTypeDocument
			if documentOnly != tt.wantDocumentOnly {
				t.Fatalf("pattern = %+v, documentOnly=%v want %v", patterns[0], documentOnly, tt.wantDocumentOnly)
			}
		})
	}
}

// The armed credential is keyed by tab, so two Authenticate calls on one tab
// would overwrite each other's entry and the first to return would drop the
// second's while its navigation was still in flight.
func TestAuthLockSerialisesCallsOnOneTab(t *testing.T) {
	var env environmentState

	first := env.authLock("tab-1")
	first.Lock()

	entered := make(chan struct{})
	go func() {
		second := env.authLock("tab-1")
		second.Lock()
		close(entered)
		second.Unlock()
	}()

	select {
	case <-entered:
		t.Fatal("a second Authenticate on the same tab was not made to wait, so it would clobber the first call's credential")
	case <-time.After(200 * time.Millisecond):
	}

	// A different tab has its own credential slot and must not be held up by it.
	other := make(chan struct{})
	go func() {
		lock := env.authLock("tab-2")
		lock.Lock()
		close(other)
		lock.Unlock()
	}()
	select {
	case <-other:
	case <-time.After(5 * time.Second):
		t.Fatal("a call on another tab blocked behind tab-1; the lock is not per tab")
	}

	first.Unlock()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the queued call never ran after the first released the tab")
	}
}

// Two goroutines arming a credential on one tab, each under the lock, must each
// see their own password answered — which is the property Authenticate needs.
func TestAuthLockKeepsTwoCallsCredentialsApart(t *testing.T) {
	var env environmentState
	const origin = "https://staging.example.com"

	authenticate := func(password string) error {
		lock := env.authLock("tab-1")
		lock.Lock()
		defer lock.Unlock()
		env.armCredential("tab-1", origin, "user", password)
		defer env.dropCredential("tab-1")
		// Stands in for the navigation: the window in which the other call could
		// overwrite this credential.
		time.Sleep(10 * time.Millisecond)
		answer := env.authResponse("tab-1", origin+"/protected")
		if answer.Password != password {
			return errors.New("a concurrent call's credential answered this call's challenge")
		}
		return nil
	}

	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, password := range []string{"fabricated-a-2f71", "fabricated-b-90ce"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := authenticate(password); err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
}

// What brw records before it grants geolocation is what clear puts back. Record
// nothing and clear leaves brw's own grant standing on a persistent profile,
// which is the one outcome a temporary override must never produce.
func TestGeoPermissionToRestore(t *testing.T) {
	tests := []struct {
		name         string
		previous     string
		readErr      error
		wantState    string
		wantRemember bool
	}{
		{
			name:         "a page that has never been asked",
			previous:     permissionStatePrompt,
			wantState:    permissionStatePrompt,
			wantRemember: true,
		},
		{
			name:         "a page that refused geolocation",
			previous:     permissionStateDenied,
			wantState:    permissionStateDenied,
			wantRemember: true,
		},
		{
			name:     "a grant that was already there is not brw's to take back",
			previous: permissionStateGranted,
		},
		{
			// navigator.permissions.query runs in the page and fails on one that is
			// navigating or already gone. Recording nothing there is what turned a
			// temporary grant into a standing one.
			name:         "the state could not be read",
			readErr:      errors.New("permissions query failed"),
			wantState:    permissionStatePrompt,
			wantRemember: true,
		},
		{
			name:         "the state could not be read and the page had reported granted",
			previous:     permissionStateGranted,
			readErr:      errors.New("permissions query failed"),
			wantState:    permissionStatePrompt,
			wantRemember: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, remember := geoPermissionToRestore(tt.previous, tt.readErr)
			if state != tt.wantState || remember != tt.wantRemember {
				t.Fatalf("geoPermissionToRestore(%q, %v) = (%q, %v), want (%q, %v)",
					tt.previous, tt.readErr, state, remember, tt.wantState, tt.wantRemember)
			}
			if !remember {
				return
			}
			// A recorded state is only worth anything if it maps onto a CDP setting
			// that actually undoes the grant.
			if permissionSettingFor(state) == cdpbrowser.PermissionSettingGranted {
				t.Fatalf("clear would restore %q as a grant, leaving brw's own grant in place", state)
			}
		})
	}
}
