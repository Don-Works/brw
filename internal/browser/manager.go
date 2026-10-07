package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Don-Works/brw/internal/actions"
	cdplaunch "github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/sessionstate"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/Don-Works/brw/internal/store"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const (
	actionSettleDelay     = 150 * time.Millisecond
	actionSettleDelayFast = 100 * time.Millisecond
	mouseHalfSettleDelay  = 75 * time.Millisecond
	menuHoverSettleDelay  = 325 * time.Millisecond

	fileChooserWaitTimeout = 5 * time.Second
)

func (m *Manager) runWithPrearmedSettle(tabCtx context.Context, cap time.Duration, action func() error) error {
	if FrameReadCheckFromContext(tabCtx) != nil {
		if err := m.guardCurrentURL(eventScopeFromCtx(tabCtx), tabCtx); err != nil {
			return err
		}
	}

	if err := tabCtx.Err(); err != nil {
		return err
	}
	if readiness, ok := tabCtx.Value(batchReadinessKey{}).(batchReadiness); ok && readiness.target == eventScopeFromCtx(tabCtx) && readiness.document != "" && readiness.document == batchDocumentIdentity(tabCtx) {
		return m.runWithBatchReadiness(tabCtx, readiness, action)
	}
	if cap <= 0 {
		return action()
	}
	handle, armErr := snapshot.ArmSettle(tabCtx, cap.Milliseconds())
	if err := tabCtx.Err(); err != nil {
		return err
	}

	var sub <-chan pageEvent
	if scope := eventScopeFromCtx(tabCtx); scope != "" {
		stream, release := m.events.subscribe([]pageEventKind{eventNavigated}, scope)
		defer release()
		sub = stream
	}
	if err := action(); err != nil {
		return err
	}
	if armErr != nil {
		_, _ = snapshot.Settle(tabCtx, cap.Milliseconds())
		return nil
	}
	awaitPrearmedSettle(tabCtx, sub, cap, func(ctx context.Context) {
		_, _ = snapshot.AwaitSettle(ctx, handle)
	})
	return nil
}

func awaitPrearmedSettle(ctx context.Context, sub <-chan pageEvent, cap time.Duration, await func(context.Context)) {
	awaitCtx, cancelAwait := context.WithCancel(ctx)
	defer cancelAwait()
	if sub == nil {
		await(awaitCtx)
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		await(awaitCtx)
	}()

	backstop := time.NewTimer(cap + settleAwaitGrace)
	defer backstop.Stop()
	for {
		select {
		case <-done:
			return
		case ev := <-sub:
			if ev.Kind == eventNavigated {
				return
			}
		case <-backstop.C:
			return
		}
	}
}

const settleAwaitGrace = 250 * time.Millisecond

type Manager struct {
	backgroundTabs sync.Map
	tabAccessGuard func(context.Context, string) error
	mu             sync.RWMutex
	launcher       *cdplaunch.Launcher
	allocCancel    context.CancelFunc
	browserCtx     context.Context
	browserCancel  context.CancelFunc
	tabContexts    map[string]tabContext
	refs           *store.RefStore
	timeout        time.Duration
	navPolicy      *navpolicy.Policy

	remote *RemoteTarget

	offHost bool

	stateMu       sync.Mutex
	lastState     map[string]*SemanticState
	observedState map[string]*SemanticState
	versions      map[string]int64

	traceMu sync.Mutex

	streamMu   sync.RWMutex
	streamSubs map[*traceSubscriber]struct{}
	trace      []TraceEntry

	consoleCaptureMu   sync.Mutex
	consoleCaptureTabs map[string]bool
	consoleMessages    map[string][]ConsoleMessage

	downloadsMu      sync.Mutex
	downloads        []DownloadEntry
	downloadIndex    map[string]int
	downloadVersions map[string]uint64

	downloadChangedAt map[string]time.Time
	downloadCursors   map[string]uint64
	downloadSequence  uint64
	downloadDir       string
	downloadDirOwned  bool

	retiredDownloadDirs []string
	userDataDir         string
	downloadsEnabled    bool

	dialogs dialogState

	containment containmentState

	contentNavGuard bool
	pacer           *Pacer

	contentNav contentNavState

	routes routeState

	env environmentState

	profiles profileState

	cancels *cancelRegistry

	netCaptureMu   sync.Mutex
	netCaptureTabs map[string]bool

	shadowPierceMu   sync.Mutex
	shadowPierceTabs map[string]bool

	documentMu      sync.Mutex
	documentTracked map[string]bool
	documentReady   map[string]bool
	documentEpoch   map[string]uint64

	webmcpEnabled bool
	webmcpMu      sync.Mutex
	webmcpTabs    map[string]bool

	emulationMu     sync.Mutex
	emulationStates map[string]deviceEmulationState

	heldMu   sync.Mutex
	heldKeys map[string]map[string]actions.KeyDescriptor

	incognitoMu       sync.Mutex
	incognitoContexts map[string]bool

	sessionStateMu sync.Mutex
	sessionState   *sessionstate.Store

	signedInProfile bool

	attachedBrowser bool

	events eventHub

	takeoverMu     sync.Mutex
	takeoverToken  string
	takeoverHolder string
	takeoverTab    string
	takeoverExpiry time.Time

	takeoverDispatchMu sync.RWMutex

	pdfStreamChunk int64
}

// SetNavigationPolicy installs the controller-level policy used for defense in depth and final-destination checks.
func (m *Manager) SetNavigationPolicy(p *navpolicy.Policy) { m.navPolicy = p }

func (m *Manager) prepareNavigationURL(rawURL string) (string, error) {
	return m.navPolicy.CheckNavigation(rawURL)
}

func (m *Manager) enforceFinalURL(tabID string, tabCtx context.Context, rawURL string) error {
	if check := FrameReadCheckFromContext(tabCtx); check != nil {
		if err := check(rawURL); err != nil {
			return err
		}
	}
	if m.navPolicy.Empty() {
		return nil
	}
	if err := m.navPolicy.Check(rawURL); err != nil {
		_ = chromedp.Run(tabCtx, chromedp.Navigate("about:blank"))
		m.invalidateState(tabID)
		return fmt.Errorf("final browser destination rejected by navigation policy and reset to about:blank: %w", err)
	}
	return nil
}

func (m *Manager) guardCurrentURL(tabID string, tabCtx context.Context) error {
	if m.navPolicy.Empty() && FrameReadCheckFromContext(tabCtx) == nil {
		return nil
	}
	var current string
	if err := chromedp.Run(tabCtx, chromedp.Evaluate("location.href", &current)); err != nil {
		return fmt.Errorf("verify current navigation destination: %w", err)
	}
	return m.enforceFinalURL(tabID, tabCtx, current)
}

func (m *Manager) guardPageError(tabID string, tabCtx context.Context, err error) error {
	if err != nil {
		if guardErr := m.guardCurrentURL(tabID, tabCtx); guardErr != nil {
			return guardErr
		}
	}
	return err
}

type tabContext struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type ctxKeyTabID struct{}
type ctxKeyTabIDExplicit struct{}
type ctxKeyTabIDRequiresCurrentOwnership struct{}

func WithTabID(ctx context.Context, tabID string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyTabID{}, tabID)
	ctx = context.WithValue(ctx, ctxKeyTabIDExplicit{}, true)

	return context.WithValue(ctx, ctxKeyTabIDRequiresCurrentOwnership{}, false)
}

// WithImplicitTabID pins a server-selected working tab without pretending the caller explicitly supplied it.
func WithImplicitTabID(ctx context.Context, tabID string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyTabID{}, tabID)
	return context.WithValue(ctx, ctxKeyTabIDExplicit{}, false)
}

// WithCurrentOwnedTabID pins a server-resolved working tab and marks it for connection-generation ownership checks.
func WithCurrentOwnedTabID(ctx context.Context, tabID string) context.Context {
	ctx = WithImplicitTabID(ctx, tabID)
	return context.WithValue(ctx, ctxKeyTabIDRequiresCurrentOwnership{}, true)
}

func TabIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKeyTabID{}).(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// TabIDIsExplicit reports whether the current pin came from a caller-supplied tab_id rather than server-side tab selection (for example a session lease).
func TabIDIsExplicit(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(ctxKeyTabIDExplicit{}).(bool)
	return v
}

// TabIDRequiresCurrentOwnership reports whether a server-resolved pin must still match the browser bridge's authoritative owned tab at dispatch time.
func TabIDRequiresCurrentOwnership(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(ctxKeyTabIDRequiresCurrentOwnership{}).(bool)
	return v
}

func tabIDFromCtx(ctx context.Context) string {
	return TabIDFromContext(ctx)
}

func New(ctx context.Context, cfg Config) (*Manager, error) {
	m, err := newManager(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := m.connect(); err != nil {
		_ = m.Close()

		return nil, m.scrubRemoteEndpoint(err)
	}
	if tabs, err := m.ListTabs(ctx); err == nil && len(tabs) > 0 {
		m.refs.SetActive(tabs[0].ID)
	}
	return m, nil
}

func newManager(ctx context.Context, cfg Config) (*Manager, error) {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}

	endpoint := cfg.RemoteURL

	var allocOpts []chromedp.RemoteAllocatorOption
	if cfg.BrowserWSURL != "" {
		endpoint = cfg.BrowserWSURL
		allocOpts = append(allocOpts, chromedp.NoModifyURL)
	}
	var launcher *cdplaunch.Launcher
	var err error
	if cfg.Remote != nil {
		if err := checkRemoteConfig(cfg); err != nil {
			return nil, err
		}
		endpoint = cfg.Remote.WebSocketURL

		allocOpts = append(allocOpts, chromedp.NoModifyURL)
	} else if endpoint == "" {
		if cfg.AttachOnly {
			return nil, ErrAttachOnlyNoEndpoint
		}
		launcher, err = cdplaunch.Launch(ctx, cdplaunch.LaunchConfig{
			ChromePath:       cfg.ChromePath,
			UserDataDir:      cfg.UserDataDir,
			ProfileDirectory: cfg.ProfileDirectory,
			Port:             cfg.Port,
			Extensions:       cfg.Extensions,
			Args:             cfg.ChromeArgs,
			AllowRealProfile: cfg.AllowRealProfile,
			Network:          cfg.Network,
			Headless:         cfg.Headless,
		})
		if err != nil {
			return nil, err
		}
		endpoint = launcher.Endpoint()
	}

	allocCtx, allocCancel := chromedp.NewRemoteAllocator(ctx, endpoint, allocOpts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	m := &Manager{
		launcher:           launcher,
		remote:             cfg.Remote,
		offHost:            !cfg.BrowserOnThisHost(),
		allocCancel:        allocCancel,
		browserCtx:         browserCtx,
		browserCancel:      browserCancel,
		tabContexts:        map[string]tabContext{},
		refs:               store.New(),
		timeout:            timeout,
		lastState:          map[string]*SemanticState{},
		observedState:      map[string]*SemanticState{},
		versions:           map[string]int64{},
		trace:              make([]TraceEntry, 0, 256),
		consoleCaptureTabs: map[string]bool{},
		consoleMessages:    map[string][]ConsoleMessage{},
		userDataDir:        cfg.UserDataDir,
		downloadIndex:      map[string]int{},
		downloadVersions:   map[string]uint64{},
		downloadCursors:    map[string]uint64{},
		cancels:            newCancelRegistry(),
		netCaptureTabs:     map[string]bool{},
		shadowPierceTabs:   map[string]bool{},
		documentTracked:    map[string]bool{},
		documentReady:      map[string]bool{},
		documentEpoch:      map[string]uint64{},
		webmcpEnabled:      cfg.WebMCP,
		webmcpTabs:         map[string]bool{},
		emulationStates:    map[string]deviceEmulationState{},
		incognitoContexts:  map[string]bool{},
		signedInProfile:    cfg.SignedInProfile,
		attachedBrowser:    launcher == nil,
	}

	return m, nil
}

func (m *Manager) Close() error {

	m.disposeIncognitoContexts()
	m.mu.Lock()
	for id, tab := range m.tabContexts {
		tab.cancel()
		delete(m.tabContexts, id)
	}
	m.mu.Unlock()
	if m.browserCancel != nil {
		m.browserCancel()
	}
	if m.allocCancel != nil {
		m.allocCancel()
	}
	var closeErr error
	if m.launcher != nil {
		closeErr = m.launcher.Close()
	}

	if m.remote != nil && m.remote.Release != nil {
		releaseCtx, cancel := context.WithTimeout(context.Background(), remoteReleaseTimeout)
		if err := m.remote.Release(releaseCtx); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("release the plugin-supplied browser session: %w", err))
		}
		cancel()
		m.remote.Release = nil
	}
	if cleanupErr := m.cleanupDownloadStaging(); cleanupErr != nil {
		closeErr = errors.Join(closeErr, cleanupErr)
	}
	return closeErr
}

var attachApprovalWindow = 2 * time.Minute

// ErrAttachNotApproved says why a browser brw attached to never answered, and names the only thing that can fix it.
var ErrAttachNotApproved = errors.New("the browser never answered brw's DevTools connection. Chrome 144 and newer ask the person at the browser to approve each remote debugging connection, so look for that prompt in the browser window and allow it; brw cannot answer it for you. If there is no prompt, the endpoint brw was pointed at is not speaking CDP")

func (m *Manager) connect() error {

	chromedp.ListenBrowser(m.browserCtx, m.handleTargetLifecycle)
	firstRoundTrip := func() error {
		return chromedp.Run(m.browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			c := chromedp.FromContext(ctx)
			if c == nil || c.Browser == nil {
				return errors.New("browser executor is not available")
			}
			ctx = cdp.WithExecutor(ctx, c.Browser)
			_, _, _, _, _, err := browser.GetVersion().Do(ctx)
			return err
		}))
	}
	if !m.attachedBrowser {
		return firstRoundTrip()
	}

	done := make(chan error, 1)
	go func() { done <- firstRoundTrip() }()
	timer := time.NewTimer(attachApprovalWindow)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil && errors.Is(err, context.DeadlineExceeded) {

			return fmt.Errorf("%w (%v)", ErrAttachNotApproved, err)
		}
		return err
	case <-timer.C:
		return fmt.Errorf("%w (waited %s)", ErrAttachNotApproved, attachApprovalWindow)
	}
}

const openNavigateTimeout = 10 * time.Second

func (m *Manager) Open(ctx context.Context, url string) (OpenResult, error) {

	if err := m.guardTakeover(TraceActionOpen); err != nil {
		return OpenResult{}, err
	}
	start := time.Now()
	var err error
	url, err = m.prepareNavigationURL(url)
	if err != nil {
		return OpenResult{}, err
	}

	var id target.ID
	if err := m.runBrowser(ctx, func(ctx context.Context) error {
		var err error
		id, err = target.CreateTarget("about:blank").Do(ctx)
		return err
	}); err != nil {
		return OpenResult{}, err
	}
	tabID := string(id)
	if IsBackgroundPage(ctx) {
		m.backgroundTabs.Store(tabID, true)
	}

	recordOpen := func(finalURL string, err error) {
		if finalURL == "" {
			finalURL = url
		}
		m.recordObservation(tabID, TraceActionOpen, finalURL, start, err)
	}

	var navErr error
	var navErrorText string
	if url != "about:blank" {

		m.recordAgentNavigation(tabID, url)

		if tabCtx, ctxErr := m.tabContext(tabID); ctxErr == nil {

			m.ensureWebMCP(tabID, tabCtx)
			navCtx, cancelNav := context.WithTimeout(tabCtx, openNavigateTimeout)
			defer m.armInlineDocument(navCtx, tabID, url)()

			navErr = chromedp.Run(navCtx, chromedp.ActionFunc(func(ctx context.Context) error {
				_, _, errorText, _, err := page.Navigate(url).Do(ctx)
				navErrorText = errorText
				return err
			}))
			cancelNav()
		} else {
			_ = m.CloseTab(ctx, tabID)
			if err := m.runBrowser(ctx, func(ctx context.Context) error {
				var err error
				id, err = target.CreateTarget(url).Do(ctx)
				return err
			}); err != nil {
				return OpenResult{}, err
			}
			tabID = string(id)
		}
	}
	if IsBackgroundPage(ctx) {
		m.backgroundTabs.Store(tabID, true)
	}
	if !IsBackgroundPage(ctx) {
		m.refs.SetActive(tabID)
	}
	ctx = WithTabID(ctx, tabID)

	var ready bool
	if url == "about:blank" {
		ready = m.WaitFor(ctx, "ready", 5*time.Second) == nil
	} else {
		ready = m.WaitFor(ctx, "committed", 10*time.Second) == nil
	}

	tab, err := m.tabByID(ctx, tabID)
	if err != nil {
		if !m.navPolicy.Empty() || FrameReadCheckFromContext(ctx) != nil {
			verifyErr := fmt.Errorf("verify open final destination: %w", err)
			recordOpen(url, verifyErr)
			_ = m.CloseTab(ctx, tabID)
			return OpenResult{}, verifyErr
		}
		recordOpen(url, nil)
		return OpenResult{Tab: Tab{ID: tabID, URL: url, Type: "page"}, Ready: ready}, nil
	}
	if check := FrameReadCheckFromContext(ctx); check != nil {
		if err := check(tab.URL); err != nil {
			recordOpen(tab.URL, err)
			_ = m.CloseTab(ctx, tabID)
			return OpenResult{}, err
		}
	}
	if err := m.navPolicy.Check(tab.URL); err != nil {
		blocked := fmt.Errorf("open redirected to a disallowed final destination: %w", err)

		recordOpen(tab.URL, blocked)
		_ = m.CloseTab(ctx, tabID)
		return OpenResult{}, blocked
	}

	if navErr != nil && strings.HasPrefix(tab.URL, "about:") {
		if IsNavigationAbortedError(navErr) {
			navErr = NavigationAbortedError("open")
		}
		stalled := fmt.Errorf("navigate new tab to %s: %w", url, navErr)
		recordOpen(url, stalled)
		return OpenResult{Tab: tab, Ready: false}, stalled
	}
	result := OpenResult{Tab: tab, Ready: ready}
	if url != "about:blank" {
		result.ApplyNavigationOutcome(m.navigationOutcome(tabID, url, tab.URL, navErrorText), true)
	}
	recordOpen(tab.URL, result.NavigationErr())
	return result, nil
}

func (m *Manager) navigationOutcome(tabID, requestedURL, committedURL, errorText string) NavigationOutcome {
	outcome := NavigationOutcome{URL: requestedURL, Error: strings.TrimSpace(errorText)}
	if IsErrorPageURL(committedURL) {
		outcome.CommittedURL = committedURL
	}
	outcome.HTTPStatus, outcome.AuthRequired = m.containment.lastDocumentResponse(tabID)
	return outcome
}

func (m *Manager) ListTabs(ctx context.Context) ([]Tab, error) {
	var infos []*target.Info
	if err := m.runBrowser(ctx, func(ctx context.Context) error {
		var err error
		infos, err = target.GetTargets().Do(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	tabs := make([]Tab, 0, len(infos))
	for _, info := range infos {
		if info == nil || info.Type != "page" {
			continue
		}
		tabs = append(tabs, Tab{
			ID:    string(info.TargetID),
			URL:   info.URL,
			Title: info.Title,
			Type:  string(info.Type),
		})
	}
	return tabs, nil
}

func (m *Manager) FocusTab(ctx context.Context, id string) error {
	if err := m.checkTabAccess(ctx, id); err != nil {
		return err
	}
	if err := m.guardTakeover(TraceActionFocusTab); err != nil {
		return err
	}
	if id == "" {
		return errors.New("tab id is required")
	}
	start := time.Now()
	if err := m.runBrowser(ctx, func(ctx context.Context) error {
		return target.ActivateTarget(target.ID(id)).Do(ctx)
	}); err != nil {
		m.recordObservation(id, TraceActionFocusTab, "", start, err)
		return err
	}
	m.refs.SetActive(id)
	m.recordObservation(id, TraceActionFocusTab, "", start, nil)
	return nil
}

func (m *Manager) CloseTab(ctx context.Context, id string) error {
	if err := m.checkTabAccess(ctx, id); err != nil {
		return err
	}

	if err := m.guardTakeover(TraceActionCloseTab); err != nil {
		return err
	}
	if id == "" {
		return errors.New("tab id is required")
	}
	start := time.Now()
	if err := m.runBrowser(ctx, func(ctx context.Context) error {
		return target.CloseTarget(target.ID(id)).Do(ctx)
	}); err != nil {
		m.recordObservation(id, TraceActionCloseTab, "", start, err)
		return err
	}
	m.backgroundTabs.Delete(id)

	m.recordObservation(id, TraceActionCloseTab, "", start, nil)
	m.forgetTab(id)
	return nil
}

func (m *Manager) forgetTab(id string) {
	if id == "" {
		return
	}
	m.mu.Lock()
	if tab, ok := m.tabContexts[id]; ok {
		if tab.cancel != nil {
			tab.cancel()
		}
		delete(m.tabContexts, id)
	}
	m.mu.Unlock()
	m.refs.DropTab(id)
	m.invalidateState(id)
	m.forgetTabCaches(id)
}

func (m *Manager) handleTargetLifecycle(ev any) {
	switch e := ev.(type) {
	case *target.EventTargetDestroyed:
		go m.forgetTab(string(e.TargetID))
	case *target.EventTargetCrashed:
		go m.forgetTab(string(e.TargetID))
	}
}

func (m *Manager) forgetTabCaches(id string) {
	m.downloadsMu.Lock()
	delete(m.downloadCursors, id)
	m.downloadsMu.Unlock()
	m.netCaptureMu.Lock()
	delete(m.netCaptureTabs, id)
	m.netCaptureMu.Unlock()
	m.shadowPierceMu.Lock()
	delete(m.shadowPierceTabs, id)
	m.shadowPierceMu.Unlock()
	m.documentMu.Lock()
	delete(m.documentTracked, id)
	delete(m.documentReady, id)
	delete(m.documentEpoch, id)
	m.documentMu.Unlock()
	m.consoleCaptureMu.Lock()
	delete(m.consoleCaptureTabs, id)
	delete(m.consoleMessages, id)
	m.consoleCaptureMu.Unlock()
	m.webmcpMu.Lock()
	delete(m.webmcpTabs, id)
	m.webmcpMu.Unlock()
	m.emulationMu.Lock()
	delete(m.emulationStates, id)
	m.emulationMu.Unlock()
	m.env.forget(id)
	m.routes.forget(id)
	m.containment.forget(id)
	m.contentNav.forget(id)
	m.heldMu.Lock()
	delete(m.heldKeys, id)
	m.heldMu.Unlock()

	m.events.closeScope(id)
}

type ctxKeyEventScope struct{}

func withEventScope(ctx context.Context, tabID string) context.Context {
	return context.WithValue(ctx, ctxKeyEventScope{}, tabID)
}

func eventScopeFromCtx(ctx context.Context) string {
	scope, _ := ctx.Value(ctxKeyEventScope{}).(string)
	return scope
}

func (m *Manager) ensureWebMCP(tabID string, tabCtx context.Context) {
	if !m.webmcpEnabled {
		return
	}
	m.webmcpMu.Lock()
	armed := m.webmcpTabs[tabID]
	m.webmcpMu.Unlock()
	if armed {
		return
	}

	_ = snapshot.RegisterWebMCPOnNewDocument(tabCtx)
	var ignored json.RawMessage
	_ = chromedp.Run(tabCtx, chromedp.Evaluate(snapshot.WebMCPInstallScript, &ignored))
	m.webmcpMu.Lock()
	if m.webmcpTabs == nil {
		m.webmcpTabs = map[string]bool{}
	}
	m.webmcpTabs[tabID] = true
	m.webmcpMu.Unlock()
}

func (m *Manager) ensureShadowPierce(tabID string, tabCtx context.Context) {
	m.shadowPierceMu.Lock()
	armed := m.shadowPierceTabs[tabID]
	m.shadowPierceMu.Unlock()
	if armed {
		return
	}
	if err := snapshot.RegisterShadowPierceOnNewDocument(tabCtx); err == nil {
		m.shadowPierceMu.Lock()
		if m.shadowPierceTabs == nil {
			m.shadowPierceTabs = map[string]bool{}
		}
		m.shadowPierceTabs[tabID] = true
		m.shadowPierceMu.Unlock()
	}
}

func (m *Manager) Snapshot(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return snapshot.PageSnapshot{}, err
	}
	defer cancel()

	m.ensureShadowPierce(tabID, tabCtx)
	m.ensureWebMCP(tabID, tabCtx)
	snap, err := snapshot.EvaluateWithOptions(tabCtx, opts)
	if err != nil {
		return snapshot.PageSnapshot{}, m.guardPageError(tabID, tabCtx, err)
	}
	if err := m.enforceFinalURL(tabID, tabCtx, snap.URL); err != nil {
		return snapshot.PageSnapshot{}, err
	}
	if opts.IncludeFrames {
		m.mergeCrossOriginFrames(ctx, tabCtx, &snap, opts)
	}
	if opts.IncludeAX {
		snapshot.EnrichAccessibility(tabCtx, &snap)
	}
	if opts.IncludeFrames || opts.IncludeAX {
		if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
			return snapshot.PageSnapshot{}, err
		}
	}

	snap.Accessibility.Requested = opts.IncludeAX
	m.refs.Observe(tabID, snap.Elements)
	return snap, nil
}

func (m *Manager) Find(ctx context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return snapshot.FindResult{}, err
	}
	defer cancel()

	m.ensureShadowPierce(tabID, tabCtx)
	m.ensureWebMCP(tabID, tabCtx)
	result, err := snapshot.Find(tabCtx, opts)
	if err != nil {
		return snapshot.FindResult{}, m.guardPageError(tabID, tabCtx, err)
	}
	if err := m.enforceFinalURL(tabID, tabCtx, result.URL); err != nil {
		return snapshot.FindResult{}, err
	}
	m.refs.Observe(tabID, result.Elements)
	return result, nil
}

// FindLive is Find on the direct-CDP transport: every search here walks the live DOM through snapshot.Find, so there is no cached element list to bypass.
func (m *Manager) FindLive(ctx context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	return m.Find(ctx, opts)
}

func (m *Manager) Read(ctx context.Context) (readability.PageRead, error) {
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return readability.PageRead{}, err
	}
	defer cancel()

	snap, err := snapshot.EvaluateWithOptions(tabCtx, snapshot.SnapshotOptions{})
	if err == nil {
		if guardErr := m.enforceFinalURL(tabID, tabCtx, snap.URL); guardErr != nil {
			m.recordObservation(tabID, TraceActionRead, snap.URL, start, guardErr)
			return readability.PageRead{}, guardErr
		}
		m.refs.Observe(tabID, snap.Elements)
	}
	read, readErr := readability.Evaluate(tabCtx, readability.SettleMS(ctx))
	readErr = m.guardPageError(tabID, tabCtx, readErr)
	url := read.URL
	if url == "" {
		url = snap.URL
	}
	if readErr == nil {
		readErr = m.enforceFinalURL(tabID, tabCtx, url)
	}
	m.recordObservation(tabID, TraceActionRead, url, start, readErr)
	if readErr != nil {
		return readability.PageRead{}, readErr
	}
	return read, readErr
}

func (m *Manager) ReadData(ctx context.Context) (snapshot.StructuredData, error) {
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return snapshot.StructuredData{}, err
	}
	defer cancel()
	if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
		return snapshot.StructuredData{}, err
	}
	data, dataErr := snapshot.EvaluateStructured(tabCtx)
	dataErr = m.guardPageError(tabID, tabCtx, dataErr)
	if dataErr == nil {
		dataErr = m.enforceFinalURL(tabID, tabCtx, data.URL)
	}
	m.recordObservation(tabID, TraceActionReadData, data.URL, start, dataErr)
	if dataErr != nil {
		return snapshot.StructuredData{}, dataErr
	}
	return data, dataErr
}

func (m *Manager) Click(ctx context.Context, ref string) (ActionResult, error) {
	if err := m.pacer.BeforeAction(ctx, TabIDFromContext(ctx)); err != nil {
		return ActionResult{}, err
	}

	if snapshot.IsCrossOriginElementRef(ref) {
		return m.clickCrossOriginFrameRef(ctx, ref)
	}
	if err := m.guardTakeover("click"); err != nil {
		return ActionResult{}, err
	}
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()
	m.recordAgentInteraction(tabID, "click")

	actionable, err := snapshot.WaitForActionableResult(tabCtx, ref, 5000)
	if err != nil {
		return ActionResult{}, err
	}
	if !actionable.OK {
		return ActionResult{}, fmt.Errorf("element ref %q not actionable within %dms — it may be hidden, disabled, or covered by an overlay; re-run brw_snapshot to refresh refs, or brw_screenshot with ref %q to inspect it", ref, 5000, ref)
	}
	before := m.cachedBefore(tabID, tabCtx)
	traceName, traceRole, traceNameIsText := m.refIdentity(tabID, ref)
	var warning string
	var clickErr error
	if modifiers := m.heldModifierMask(tabID); modifiers != 0 {
		warning, clickErr = m.clickElementCenterWithModifiers(tabCtx, ref, input.Modifier(modifiers))
	} else {
		warning, clickErr = m.clickElementCenter(tabCtx, ref, 150*time.Millisecond)
	}
	if clickErr != nil {
		return ActionResult{}, clickErr
	}
	result := m.observeActionWithBefore(tabID, tabCtx, "dispatched click "+ref, before)
	if actionable.Mode == "hit_test" {
		note := "clicked via geometry hit-test (element reported AX-invisible)"
		if warning != "" {
			warning = warning + "; " + note
		} else {
			warning = note
		}
	}
	if warning != "" {
		appendWarning(&result, warning)
	}
	result.DurationMS = time.Since(start).Milliseconds()
	m.recordTrace(tabID, TraceEntry{
		Action:            "click",
		Ref:               ref,
		Name:              traceName,
		Role:              traceRole,
		NameIsVisibleText: traceNameIsText,
		OK:                result.OK,
		Error:             result.Warning,
		DurationMS:        result.DurationMS,
		Timestamp:         time.Now().Format(time.RFC3339),
	})
	return result, nil
}

func (m *Manager) ClickText(ctx context.Context, opts snapshot.ClickTextOptions) (ActionResult, error) {
	if err := m.pacer.BeforeAction(ctx, TabIDFromContext(ctx)); err != nil {
		return ActionResult{}, err
	}
	if err := m.guardTakeover("click_text"); err != nil {
		return ActionResult{}, err
	}
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()
	m.recordAgentInteraction(tabID, "click_text")

	before := m.cachedBefore(tabID, tabCtx)
	clicked, err := m.clickTextTarget(tabCtx, tabID, opts, actionSettleDelay)
	if err != nil {
		return ActionResult{}, err
	}
	label := opts.Text
	if clicked.Name != "" {
		label = clicked.Name
	}
	result := m.observeActionWithBefore(tabID, tabCtx, "dispatched click text "+strconv.Quote(label), before)
	result.DurationMS = time.Since(start).Milliseconds()
	m.recordTrace(tabID, TraceEntry{
		Action:     "click_text",
		Text:       opts.Text,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}

func (m *Manager) clickTextTarget(tabCtx context.Context, tabID string, opts snapshot.ClickTextOptions, delay time.Duration) (snapshot.ClickXYResult, error) {
	opts.Locate = true
	located, err := snapshot.ClickText(tabCtx, opts)
	if err != nil {
		return located, err
	}
	if located.Ref == "" {
		return located, errors.New("click text resolved no target ref")
	}
	err = m.trustedClickRefAtPoint(tabCtx, located.Ref, &located, delay, input.Modifier(m.heldModifierMask(tabID)))
	located.Deferred = false
	return located, err
}

func (m *Manager) Hover(ctx context.Context, ref string) (ActionResult, error) {
	if err := m.pacer.BeforeAction(ctx, TabIDFromContext(ctx)); err != nil {
		return ActionResult{}, err
	}
	if err := GuardCrossOriginRefs("hover", DirectCrossOriginRemedy, ref); err != nil {
		return ActionResult{}, err
	}
	if err := m.guardTakeover("hover"); err != nil {
		return ActionResult{}, err
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	before := m.cachedBefore(tabID, tabCtx)
	traceName, traceRole, traceNameIsText := m.refIdentity(tabID, ref)
	recovery, err := m.hoverRef(tabCtx, tabID, ref)
	if err != nil {
		return ActionResult{}, err
	}
	result := m.observeActionWithBefore(tabID, tabCtx, "hovered "+ref, before)
	if recovery != "" {
		appendWarning(&result, recovery)
	}
	m.recordTrace(tabID, TraceEntry{
		Action:            "hover",
		Ref:               ref,
		Name:              traceName,
		Role:              traceRole,
		NameIsVisibleText: traceNameIsText,
		OK:                result.OK,
		Error:             result.Warning,
		DurationMS:        result.DurationMS,
		Timestamp:         time.Now().Format(time.RFC3339),
	})
	return result, nil
}

func (m *Manager) hoverRef(tabCtx context.Context, tabID, ref string) (string, error) {
	if err := snapshot.WaitForActionable(tabCtx, ref, 5000); err != nil {
		return "", err
	}
	box, err := snapshot.ResolveOrRecoverBox(tabCtx, ref)
	if err != nil {
		return "", err
	}
	recovery := ""
	if box.Recovered {
		recovery = fmt.Sprintf("ref recovered: %s -> %s", box.OldRef, box.Ref)
	}
	settleCap := actionSettleDelay
	if box.DelayedHover {
		settleCap += menuHoverSettleDelay
	}
	if err := m.runWithPrearmedSettle(tabCtx, settleCap, func() error {
		if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			return input.DispatchMouseEvent(input.MouseMoved, box.ViewportX, box.ViewportY).
				WithModifiers(input.Modifier(m.heldModifierMask(tabID))).
				Do(ctx)
		})); err != nil {
			return err
		}
		if !box.DelayedHover {
			return nil
		}
		timer := time.NewTimer(menuHoverSettleDelay)
		defer timer.Stop()
		select {
		case <-tabCtx.Done():
			return tabCtx.Err()
		case <-timer.C:
			return nil
		}
	}); err != nil {
		return "", err
	}
	return recovery, nil
}

func (m *Manager) Evaluate(ctx context.Context, expression string) (any, error) {

	if !isGeneratedReadExpression(traceLabelAction(ctx), expression) {
		if err := m.guardTakeover(takeoverActionEvaluateScript); err != nil {
			return nil, err
		}
	}
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	m.recordAgentInteraction(tabID, "evaluate")

	recordEvaluate := func(err error) {
		if tabID == "" {
			return
		}
		action, text := TraceActionEvaluate, expression
		if label, ok := TraceLabelFromCtx(ctx); ok {
			action, text = label.Action, label.Value
		}
		entry := RedactTraceEntry(ctx, NewObservationTrace(action, text, start, err))
		m.recordTrace(tabID, entry)
	}

	var result any
	evaluate := func(replMode bool) error {
		result = nil
		return chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			params := runtime.Evaluate(expression).
				WithAwaitPromise(true).
				WithReturnByValue(true)
			if replMode {
				params = params.WithReplMode(true)
			}
			obj, exception, err := params.Do(ctx)
			if err != nil {
				return err
			}
			if exception != nil {
				if msg := FormatRuntimeException(exception); msg != "" {
					return fmt.Errorf("runtime exception: %s", msg)
				}
				details, _ := json.Marshal(exception)
				return fmt.Errorf("runtime exception: %s", details)
			}
			if obj == nil || len(obj.Value) == 0 {
				return nil
			}
			return json.Unmarshal(obj.Value, &result)
		}))
	}
	err = evaluate(false)
	if guardErr := m.guardCurrentURL(tabID, tabCtx); guardErr != nil {
		recordEvaluate(guardErr)
		return nil, guardErr
	}
	if isTopLevelAwaitSyntaxError(err) {
		err = evaluate(true)
		if guardErr := m.guardCurrentURL(tabID, tabCtx); guardErr != nil {
			recordEvaluate(guardErr)
			return nil, guardErr
		}
	}
	if err != nil {
		recordEvaluate(err)
		return nil, err
	}
	recordEvaluate(nil)
	return result, nil
}

func isTopLevelAwaitSyntaxError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "await is only valid") ||
		strings.Contains(message, "unexpected reserved word") ||
		strings.Contains(message, "await is not defined")
}

func (m *Manager) NetworkRequests(ctx context.Context, filter string) ([]NetworkRequest, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	filterJSON, _ := json.Marshal(filter)
	expr := fmt.Sprintf(`(function(filter) {
	  var entries = performance.getEntriesByType('resource');
	  if (filter) {
	    var lower = filter.toLowerCase();
	    entries = entries.filter(function(e) { return e.name.toLowerCase().indexOf(lower) !== -1; });
	  }
	  return entries.map(function(e) {
	    return {
	      url: e.name,
	      initiator_type: e.initiatorType || '',
	      start_time: Math.round(e.startTime),
	      duration: Math.round(e.duration),
	      transfer_size: e.transferSize || 0,
	      status: 0
	    };
	  });
	})(%s)`, filterJSON)
	var requests []NetworkRequest
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(expr, &requests)); err != nil {
		return nil, err
	}
	if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
		return nil, err
	}
	return requests, nil
}

func (m *Manager) Type(ctx context.Context, ref, text string) (ActionResult, error) {
	if err := m.pacer.BeforeAction(ctx, TabIDFromContext(ctx)); err != nil {
		return ActionResult{}, err
	}
	if err := GuardCrossOriginRefs("type", DirectCrossOriginRemedy, ref); err != nil {
		return ActionResult{}, err
	}
	if err := m.guardTakeover("type"); err != nil {
		return ActionResult{}, err
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()
	m.recordAgentInteraction(tabID, "type")

	before := m.cachedBefore(tabID, tabCtx)
	traceName, traceRole, traceNameIsText := m.refIdentity(tabID, ref)
	if err := m.typeRef(tabCtx, ref, text); err != nil {
		return ActionResult{}, err
	}
	result := m.observeActionWithBefore(tabID, tabCtx, "typed into "+ref, before)
	m.recordTrace(tabID, RedactTraceEntry(ctx, TraceEntry{
		Action:            "type",
		Ref:               ref,
		Name:              traceName,
		Role:              traceRole,
		NameIsVisibleText: traceNameIsText,
		Text:              text,
		OK:                result.OK,
		Error:             result.Warning,
		DurationMS:        result.DurationMS,
		Timestamp:         time.Now().Format(time.RFC3339),
	}))
	return result, nil
}

func (m *Manager) typeRef(tabCtx context.Context, ref, text string) error {
	if err := snapshot.WaitForActionable(tabCtx, ref, 5000); err != nil {
		return err
	}
	if err := snapshot.Focus(tabCtx, ref); err != nil {
		return err
	}
	return m.runWithPrearmedSettle(tabCtx, actionSettleDelayFast, func() error {
		return chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			return m.pacer.Type(ctx, text, func(chunk string) error {
				return input.InsertText(chunk).Do(ctx)
			})
		}))
	})
}

// Focus gives one element the keyboard focus and reports the page afterwards, which is the contract every action tool answers on.
func (m *Manager) Focus(ctx context.Context, ref string) (ActionResult, error) {
	if err := GuardCrossOriginRefs("focus", DirectCrossOriginRemedy, ref); err != nil {
		return ActionResult{}, err
	}
	if err := m.guardTakeover("focus"); err != nil {
		return ActionResult{}, err
	}
	if strings.TrimSpace(ref) == "" {
		return ActionResult{}, errors.New("ref is required")
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	before := m.cachedBefore(tabID, tabCtx)
	traceName, traceRole, traceNameIsText := m.refIdentity(tabID, ref)
	if err := snapshot.WaitForActionable(tabCtx, ref, 5000); err != nil {
		return ActionResult{}, err
	}
	if err := m.runWithPrearmedSettle(tabCtx, actionSettleDelayFast, func() error {
		return snapshot.Focus(tabCtx, ref)
	}); err != nil {
		return ActionResult{}, err
	}
	result := m.observeActionWithBefore(tabID, tabCtx, "focused "+ref, before)
	if result.Focus != "" && result.Focus != ref {
		appendWarning(&result, fmt.Sprintf("focus landed on %s, not %s: the element moved focus on its own", result.Focus, ref))
	}
	m.recordTrace(tabID, TraceEntry{
		Action:            "focus",
		Ref:               ref,
		Name:              traceName,
		Role:              traceRole,
		NameIsVisibleText: traceNameIsText,
		OK:                result.OK,
		Error:             result.Warning,
		DurationMS:        result.DurationMS,
		Timestamp:         time.Now().Format(time.RFC3339),
	})
	return result, nil
}

// FocusRef is the narrow transport capability deterministic recipes use before a key press.
func (m *Manager) FocusRef(ctx context.Context, ref string) error {
	if err := GuardCrossOriginRefs("focus", DirectCrossOriginRemedy, ref); err != nil {
		return err
	}
	if err := m.guardTakeover("focus"); err != nil {
		return err
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
		return err
	}
	if err := snapshot.WaitForActionable(tabCtx, ref, 5000); err != nil {
		return err
	}
	if err := snapshot.Focus(tabCtx, ref); err != nil {
		return err
	}
	return m.guardCurrentURL(tabID, tabCtx)
}

func (m *Manager) Fill(ctx context.Context, opts snapshot.FillOptions) (ActionResult, error) {
	if err := m.pacer.BeforeAction(ctx, TabIDFromContext(ctx)); err != nil {
		return ActionResult{}, err
	}
	if err := GuardCrossOriginRefs("fill", DirectCrossOriginRemedy, opts.Ref); err != nil {
		return ActionResult{}, err
	}
	if err := m.guardTakeover("fill"); err != nil {
		return ActionResult{}, err
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()
	m.recordAgentInteraction(tabID, "fill")

	ref := opts.Ref
	if ref == "" {
		result, err := snapshot.Find(tabCtx, snapshot.FindOptions{
			Query: opts.Query,
			Role:  opts.Role,
			Limit: 1,
		})
		if err != nil {
			return ActionResult{}, err
		}
		if len(result.Elements) == 0 {
			return ActionResult{}, fmt.Errorf("no fill target found for query %q", opts.Query)
		}
		ref = result.Elements[0].Ref
		m.refs.Observe(tabID, result.Elements)
	}
	before := m.cachedBefore(tabID, tabCtx)
	traceName, traceRole, traceNameIsText := m.refIdentity(tabID, ref)
	if err := m.fillRef(tabCtx, ref, opts.EffectiveText(), opts.Replace); err != nil {
		return ActionResult{}, err
	}
	result := m.observeActionWithBefore(tabID, tabCtx, "filled "+ref, before)
	m.recordTrace(tabID, RedactTraceEntry(ctx, TraceEntry{
		Action:            "fill",
		Ref:               ref,
		Name:              traceName,
		Role:              traceRole,
		NameIsVisibleText: traceNameIsText,
		Text:              opts.EffectiveText(),
		OK:                result.OK,
		Error:             result.Warning,
		DurationMS:        result.DurationMS,
		Timestamp:         time.Now().Format(time.RFC3339),
	}))
	return result, nil
}

func (m *Manager) fillRef(tabCtx context.Context, ref, text string, replace bool) error {
	if err := snapshot.WaitForActionable(tabCtx, ref, 5000); err != nil {
		return err
	}
	return m.runWithPrearmedSettle(tabCtx, actionSettleDelayFast, func() error {
		return snapshot.Fill(tabCtx, ref, text, replace)
	})
}

func (m *Manager) UploadFile(ctx context.Context, opts snapshot.UploadOptions) (ActionResult, error) {
	if err := m.pacer.BeforeAction(ctx, TabIDFromContext(ctx)); err != nil {
		return ActionResult{}, err
	}
	if err := GuardCrossOriginRefs("upload file", DirectCrossOriginRemedy, opts.Ref, opts.ClickRef); err != nil {
		return ActionResult{}, err
	}
	if err := m.guardTakeover("upload_file"); err != nil {
		return ActionResult{}, err
	}

	if err := m.refuseOnRemote("local_upload"); err != nil {
		return ActionResult{}, err
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()
	m.recordAgentInteraction(tabID, "upload_file")

	paths, cleanup, err := ResolveUploadPaths(ctx, opts)
	if err != nil {
		return ActionResult{}, err
	}
	cleanupNow := true
	defer func() {
		if cleanupNow {
			cleanup()
		}
	}()

	if opts.ClickRef != "" || opts.ClickText != "" {
		result, err := m.uploadFileViaChooser(tabID, tabCtx, opts, paths)
		if err != nil {
			return ActionResult{}, err
		}
		RetainUploadCleanup(cleanup)
		cleanupNow = false
		return result, nil
	}

	ref := opts.Ref
	if ref == "" {
		query := opts.Query
		if strings.TrimSpace(query) == "" {
			query = "file"
		}
		result, err := snapshot.Find(tabCtx, snapshot.FindOptions{
			Query: query,
			Role:  opts.Role,
			Limit: 20,
		})
		if err != nil {
			return ActionResult{}, err
		}
		m.refs.Observe(tabID, result.Elements)
		for _, el := range result.Elements {
			if el.Tag == "input" && el.Type == "file" {
				ref = el.Ref
				break
			}
		}
		if ref == "" {
			return ActionResult{}, fmt.Errorf("no file input found for query %q", query)
		}
	}

	before := m.cachedBefore(tabID, tabCtx)
	if err := m.runWithPrearmedSettle(tabCtx, actionSettleDelayFast, func() error {
		return snapshot.SetFileInputFiles(tabCtx, ref, paths)
	}); err != nil {
		return ActionResult{}, err
	}
	result := m.observeActionWithBefore(tabID, tabCtx, "uploaded file to "+ref, before)
	RetainUploadCleanup(cleanup)
	cleanupNow = false
	return result, nil
}

func (m *Manager) uploadFileViaChooser(tabID string, tabCtx context.Context, opts snapshot.UploadOptions, paths []string) (ActionResult, error) {
	if err := chromedp.Run(tabCtx, page.SetInterceptFileChooserDialog(true)); err != nil {
		return ActionResult{}, fmt.Errorf("enable file chooser interception: %w", err)
	}
	defer func() {

		disableCtx, cancel := context.WithTimeout(context.WithoutCancel(tabCtx), 2*time.Second)
		defer cancel()
		_ = chromedp.Run(disableCtx, page.SetInterceptFileChooserDialog(false))
	}()

	chooserCh := make(chan cdp.BackendNodeID, 1)
	var once sync.Once
	chromedp.ListenTarget(tabCtx, func(ev any) {
		if e, ok := ev.(*page.EventFileChooserOpened); ok {
			once.Do(func() { chooserCh <- e.BackendNodeID })
		}
	})

	before := m.cachedBefore(tabID, tabCtx)

	if opts.ClickRef != "" {
		if err := snapshot.WaitForActionable(tabCtx, opts.ClickRef, 5000); err != nil {
			return ActionResult{}, err
		}
		if _, err := m.clickElementCenter(tabCtx, opts.ClickRef, 150*time.Millisecond); err != nil {
			return ActionResult{}, fmt.Errorf("click upload trigger %s: %w", opts.ClickRef, err)
		}
	} else {
		if _, err := m.clickTextTarget(tabCtx, tabID, snapshot.ClickTextOptions{Text: opts.ClickText, Role: opts.Role}, actionSettleDelay); err != nil {
			return ActionResult{}, fmt.Errorf("click upload trigger %q: %w", opts.ClickText, err)
		}
	}

	var backendNodeID cdp.BackendNodeID
	select {
	case backendNodeID = <-chooserCh:
	case <-tabCtx.Done():
		return ActionResult{}, tabCtx.Err()
	case <-time.After(fileChooserWaitTimeout):
		return ActionResult{}, fmt.Errorf("no file chooser opened within %s after clicking the trigger — confirm the trigger opens a file picker", fileChooserWaitTimeout)
	}
	if backendNodeID == 0 {
		return ActionResult{}, errors.New("file chooser opened but reported no backendNodeId")
	}

	if err := m.runWithPrearmedSettle(tabCtx, actionSettleDelayFast, func() error {
		return chromedp.Run(tabCtx, dom.SetFileInputFiles(paths).WithBackendNodeID(backendNodeID))
	}); err != nil {
		return ActionResult{}, err
	}
	return m.observeActionWithBefore(tabID, tabCtx, "uploaded file via intercepted file chooser", before), nil
}

func (m *Manager) Select(ctx context.Context, ref, value string) (ActionResult, error) {
	if err := m.pacer.BeforeAction(ctx, TabIDFromContext(ctx)); err != nil {
		return ActionResult{}, err
	}
	if err := GuardCrossOriginRefs("select", DirectCrossOriginRemedy, ref); err != nil {
		return ActionResult{}, err
	}
	if err := m.guardTakeover("select"); err != nil {
		return ActionResult{}, err
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()
	m.recordAgentInteraction(tabID, "select")
	before := m.cachedBefore(tabID, tabCtx)
	traceName, traceRole, traceNameIsText := m.refIdentity(tabID, ref)
	message, err := m.selectValue(tabCtx, ref, value)
	if err != nil {
		return ActionResult{}, err
	}
	result := m.observeActionWithBefore(tabID, tabCtx, message, before)
	m.recordTrace(tabID, RedactTraceEntry(ctx, TraceEntry{
		Action:            "select",
		Ref:               ref,
		Name:              traceName,
		Role:              traceRole,
		NameIsVisibleText: traceNameIsText,
		Value:             value,
		OK:                result.OK,
		Error:             result.Warning,
		DurationMS:        result.DurationMS,
		Timestamp:         time.Now().Format(time.RFC3339),
	}))
	return result, nil
}

func (m *Manager) selectValue(tabCtx context.Context, ref, value string) (string, error) {
	if err := snapshot.WaitForActionable(tabCtx, ref, 5000); err != nil {
		return "", err
	}
	if err := m.runWithPrearmedSettle(tabCtx, actionSettleDelayFast, func() error {
		return snapshot.Select(tabCtx, ref, value)
	}); err == nil {

		return "selected " + ref, nil
	} else if !strings.Contains(err.Error(), snapshot.NotASelectElement) {
		return "", err
	}
	if elementValueMatches(tabCtx, ref, value) {
		return "selected " + ref + " already " + value, nil
	}
	option, err := findOptionCandidate(tabCtx, value)
	if err != nil {
		if _, clickErr := m.clickElementCenter(tabCtx, ref, 125*time.Millisecond); clickErr != nil {
			return "", fmt.Errorf("open custom select %s: %w", ref, clickErr)
		}
		option, err = findOptionCandidate(tabCtx, value)
		if err != nil {
			return "", err
		}
	}
	if _, clickErr := m.clickElementCenter(tabCtx, option.Ref, 150*time.Millisecond); clickErr != nil {
		return "", fmt.Errorf("select option %s: %w", option.Ref, clickErr)
	}
	return "selected " + ref + " via option " + option.Ref, nil
}

func elementValueMatches(tabCtx context.Context, ref, value string) bool {
	snap, err := snapshot.EvaluateWithOptions(tabCtx, snapshot.SnapshotOptions{Limit: 0, ViewportOnly: false})
	if err != nil {
		return false
	}
	for _, el := range snap.Elements {
		if el.Ref == ref && ElementMatchesOptionValue(el, value) {
			return true
		}
	}
	return false
}

func (m *Manager) clickElementCenter(tabCtx context.Context, ref string, delay time.Duration) (string, error) {
	return m.clickElementCenterTrusted(tabCtx, ref, delay, 0)
}

func (m *Manager) clickElementCenterWithModifiers(tabCtx context.Context, ref string, modifiers input.Modifier) (string, error) {
	return m.clickElementCenterTrusted(tabCtx, ref, actionSettleDelay, modifiers)
}

func (m *Manager) clickElementCenterTrusted(tabCtx context.Context, ref string, delay time.Duration, modifiers input.Modifier) (string, error) {
	box, err := snapshot.ResolveOrRecoverBox(tabCtx, ref)
	if err != nil {
		return "", err
	}
	warning := ""
	if box.Recovered {
		warning = fmt.Sprintf("ref recovered: %s -> %s", box.OldRef, box.Ref)
	}
	if err := m.trustedClickRefAtPoint(tabCtx, box.Ref, &snapshot.ClickXYResult{X: box.ViewportX, Y: box.ViewportY}, delay, modifiers); err != nil {
		return "", err
	}
	return warning, nil
}

func (m *Manager) trustedClickRefAtPoint(tabCtx context.Context, ref string, located *snapshot.ClickXYResult, delay time.Duration, modifiers input.Modifier) error {
	resolve := func(ctx context.Context) (snapshot.ClickXYResult, error) {
		if located != nil {
			return snapshot.ResolveClickTargetAtPoint(ctx, ref, located.X, located.Y)
		}
		return snapshot.ResolveClickTarget(ctx, ref)
	}
	return m.runWithPrearmedSettle(tabCtx, delay, func() error {
		point, err := resolve(tabCtx)
		if err != nil {
			return err
		}
		return chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			if m.emulatedTouchEnabled(ctx) {
				point, err = resolve(ctx)
				if err != nil {
					return err
				}
				return dispatchTrustedTap(ctx, point.X, point.Y, modifiers)
			}
			if err := input.DispatchMouseEvent(input.MouseMoved, point.X, point.Y).WithModifiers(modifiers).Do(ctx); err != nil {
				return err
			}
			point, err = resolve(ctx)
			if err != nil {
				return err
			}
			if err := input.DispatchMouseEvent(input.MousePressed, point.X, point.Y).WithButton(input.Left).WithButtons(1).WithModifiers(modifiers).WithClickCount(1).Do(ctx); err != nil {
				return err
			}
			return input.DispatchMouseEvent(input.MouseReleased, point.X, point.Y).WithButton(input.Left).WithButtons(0).WithModifiers(modifiers).WithClickCount(1).Do(ctx)
		}))
	})
}

func findOptionCandidate(tabCtx context.Context, value string) (snapshot.Element, error) {
	for _, opts := range []snapshot.SnapshotOptions{
		{Role: "option", Query: value, Limit: 100, ViewportOnly: false},
		{Role: "option", Limit: 200, ViewportOnly: false},
	} {
		snap, err := snapshot.EvaluateWithOptions(tabCtx, opts)
		if err != nil {
			return snapshot.Element{}, err
		}
		if option, ok := SelectOptionCandidate(snap.Elements, value); ok {
			return option, nil
		}
	}
	return snapshot.Element{}, fmt.Errorf("no visible option found for %q", value)
}

func (m *Manager) Press(ctx context.Context, key string) (ActionResult, error) {
	if err := m.pacer.BeforeAction(ctx, TabIDFromContext(ctx)); err != nil {
		return ActionResult{}, err
	}
	if err := m.guardTakeover("press"); err != nil {
		return ActionResult{}, err
	}
	if key == "" {
		return ActionResult{}, errors.New("key is required")
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()
	m.recordAgentInteraction(tabID, "press")
	before := m.cachedBefore(tabID, tabCtx)
	if err := m.runWithPrearmedSettle(tabCtx, actionSettleDelay, func() error {
		return m.pressKey(tabCtx, tabID, key)
	}); err != nil {
		return ActionResult{}, err
	}
	result := m.observeActionWithBefore(tabID, tabCtx, "pressed "+key, before)
	m.recordTrace(tabID, TraceEntry{
		Action:     "press",
		Value:      key,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}

func (m *Manager) pressKey(tabCtx context.Context, tabID, key string) error {
	desc := actions.DescribeKey(key)
	if desc.Key == "" {
		return errors.New("key is required")
	}

	desc = actions.ApplyModifiers(desc, m.heldModifierMask(tabID))
	modifiers := input.Modifier(desc.Modifiers)
	return chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		keyType := input.KeyDown
		if desc.Text == "" {

			keyType = input.KeyRawDown
		}
		down := input.DispatchKeyEvent(keyType).
			WithModifiers(modifiers).
			WithKey(desc.Key).
			WithCode(desc.Code).
			WithWindowsVirtualKeyCode(desc.WindowsVirtualKeyCode).
			WithNativeVirtualKeyCode(desc.WindowsVirtualKeyCode)
		if desc.Text != "" {
			down = down.WithText(desc.Text).WithUnmodifiedText(desc.Text)
		}
		if err := down.Do(ctx); err != nil {
			return err
		}
		return input.DispatchKeyEvent(input.KeyUp).
			WithModifiers(modifiers).
			WithKey(desc.Key).
			WithCode(desc.Code).
			WithWindowsVirtualKeyCode(desc.WindowsVirtualKeyCode).
			WithNativeVirtualKeyCode(desc.WindowsVirtualKeyCode).
			Do(ctx)
	}))
}

func (m *Manager) Scroll(ctx context.Context, direction string) (ActionResult, error) {
	if err := m.pacer.BeforeAction(ctx, TabIDFromContext(ctx)); err != nil {
		return ActionResult{}, err
	}
	if err := m.guardTakeover("scroll"); err != nil {
		return ActionResult{}, err
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	before := m.cachedBefore(tabID, tabCtx)
	var message string
	if err := m.runWithPrearmedSettle(tabCtx, actionSettleDelayFast, func() error {
		var scrollErr error
		message, scrollErr = m.scrollDirection(tabCtx, direction)
		return scrollErr
	}); err != nil {
		return ActionResult{}, err
	}
	result := m.observeActionWithBefore(tabID, tabCtx, message, before)
	m.recordTrace(tabID, TraceEntry{
		Action:     "scroll",
		Value:      direction,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}

func (m *Manager) scrollDirection(tabCtx context.Context, direction string) (string, error) {
	direction = strings.ToLower(strings.TrimSpace(direction))
	if direction == "" {
		direction = "down"
	}
	scroll, err := snapshot.Scroll(tabCtx, direction)
	if err != nil {
		return "", err
	}
	message := fmt.Sprintf("scrolled %s target:%s", direction, scroll.Target)
	if scroll.Name != "" {
		message += " " + strconv.Quote(scroll.Name)
	}
	return message, nil
}

func isTransientNavigationError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "execution context was destroyed") ||
		strings.Contains(msg, "cannot find context with specified id") ||
		strings.Contains(msg, "frame was detached") ||
		strings.Contains(msg, "inspected target navigated or closed")
}

func retryAssertAfterNavigation(ctx context.Context, timeout time.Duration, operation func(time.Duration) error) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return snapshot.ErrAssertionTimeout
		}
		err := operation(remaining)
		if err == nil || !isTransientNavigationError(err) {
			return err
		}

		remaining = time.Until(deadline)
		if remaining <= 0 {
			return snapshot.ErrAssertionTimeout
		}
		delay := 100 * time.Millisecond
		if remaining < delay {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Manager) evalAssert(ctx context.Context, timeout time.Duration, script string, args ...any) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	tabID, tabCtx, cancel, err := m.activeContextWithTimeout(ctx, timeout+2*time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
		return err
	}
	if err := retryAssertAfterNavigation(tabCtx, timeout, func(remaining time.Duration) error {
		evalArgs := make([]any, len(args)+1)
		copy(evalArgs, args)
		evalArgs[len(args)] = remaining.Milliseconds()
		return snapshot.EvalAssert(tabCtx, script, evalArgs...)
	}); err != nil {
		return err
	}
	return m.guardCurrentURL(tabID, tabCtx)
}

func (m *Manager) AssertVisible(ctx context.Context, ref string, timeout time.Duration) error {
	if err := GuardCrossOriginRefs("assert visible", DirectCrossOriginRemedy, ref); err != nil {
		return err
	}
	return m.evalAssert(ctx, timeout, snapshot.AssertVisibleScript, ref)
}

func (m *Manager) AssertText(ctx context.Context, ref, expected string, timeout time.Duration) error {
	if err := GuardCrossOriginRefs("assert text", DirectCrossOriginRemedy, ref); err != nil {
		return err
	}
	return m.evalAssert(ctx, timeout, snapshot.AssertTextScript, ref, expected)
}

func (m *Manager) AssertValue(ctx context.Context, ref, expected string, timeout time.Duration) error {
	if err := GuardCrossOriginRefs("assert value", DirectCrossOriginRemedy, ref); err != nil {
		return err
	}
	return m.evalAssert(ctx, timeout, snapshot.AssertValueScript, ref, expected)
}

func (m *Manager) AssertValueContains(ctx context.Context, ref, expected string, timeout time.Duration) error {
	if err := GuardCrossOriginRefs("assert value contains", DirectCrossOriginRemedy, ref); err != nil {
		return err
	}
	return m.evalAssert(ctx, timeout, snapshot.AssertValueContainsScript, ref, expected)
}

func (m *Manager) AssertHidden(ctx context.Context, ref string, timeout time.Duration) error {
	if err := GuardCrossOriginRefs("assert hidden", DirectCrossOriginRemedy, ref); err != nil {
		return err
	}
	return m.evalAssert(ctx, timeout, snapshot.AssertHiddenScript, ref)
}

func (m *Manager) CommitField(ctx context.Context, ref string) error {
	if err := GuardCrossOriginRefs("commit field", DirectCrossOriginRemedy, ref); err != nil {
		return err
	}
	if err := m.guardTakeover("commit"); err != nil {
		return err
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	m.recordAgentInteraction(tabID, "commit")
	if err := m.runWithPrearmedSettle(tabCtx, 0, func() error { return snapshot.CommitField(tabCtx, ref) }); err != nil {
		return err
	}
	return m.guardCurrentURL(tabID, tabCtx)
}

func (m *Manager) ClickXY(ctx context.Context, x, y float64) (snapshot.ClickXYResult, error) {
	if err := m.pacer.BeforeAction(ctx, TabIDFromContext(ctx)); err != nil {
		return snapshot.ClickXYResult{}, err
	}
	if err := m.guardTakeover("click_xy"); err != nil {
		return snapshot.ClickXYResult{}, err
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return snapshot.ClickXYResult{}, err
	}
	defer cancel()
	if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
		return snapshot.ClickXYResult{}, err
	}
	m.recordAgentInteraction(tabID, "click_xy")
	point, err := snapshot.ResolveClickPoint(tabCtx, x, y)
	if err != nil {
		if guardErr := m.guardCurrentURL(tabID, tabCtx); guardErr != nil {
			return snapshot.ClickXYResult{}, guardErr
		}
		return point, err
	}
	err = chromedp.Run(tabCtx, chromedp.ActionFunc(func(c context.Context) error {
		modifiers := input.Modifier(m.heldModifierMask(tabID))
		if m.emulatedTouchEnabled(c) {
			return dispatchTrustedTap(c, x, y, modifiers)
		}
		if err := input.DispatchMouseEvent(input.MouseMoved, x, y).WithModifiers(modifiers).Do(c); err != nil {
			return err
		}
		if _, err := snapshot.ResolveClickPoint(c, x, y); err != nil {
			return err
		}
		if err := input.DispatchMouseEvent(input.MousePressed, x, y).WithButton(input.Left).WithButtons(1).WithModifiers(modifiers).WithClickCount(1).Do(c); err != nil {
			return err
		}
		return input.DispatchMouseEvent(input.MouseReleased, x, y).WithButton(input.Left).WithButtons(0).WithModifiers(modifiers).WithClickCount(1).Do(c)
	}))
	if guardErr := m.guardCurrentURL(tabID, tabCtx); guardErr != nil {
		return snapshot.ClickXYResult{}, guardErr
	}
	return point, err
}

func (m *Manager) WindowBounds(ctx context.Context) (snapshot.WindowBoundsResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return snapshot.WindowBoundsResult{}, err
	}
	defer cancel()
	result, err := snapshot.WindowBounds(tabCtx)
	if err != nil {
		return snapshot.WindowBoundsResult{}, err
	}
	if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
		return snapshot.WindowBoundsResult{}, err
	}
	return result, nil
}

type ConsoleMessage struct {
	Level     string `json:"level"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
}

func (m *Manager) ConsoleMessages(ctx context.Context) ([]ConsoleMessage, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
		return nil, err
	}
	m.ensureConsoleCapture(tabID, tabCtx)
	m.consoleCaptureMu.Lock()
	msgs := append([]ConsoleMessage(nil), m.consoleMessages[tabID]...)
	m.consoleMessages[tabID] = m.consoleMessages[tabID][:0]
	m.consoleCaptureMu.Unlock()
	if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
		return nil, err
	}
	return msgs, nil
}

func (m *Manager) ensureConsoleCapture(tabID string, tabCtx context.Context) {
	m.consoleCaptureMu.Lock()
	if m.consoleCaptureTabs[tabID] {
		m.consoleCaptureMu.Unlock()
		return
	}
	if m.consoleCaptureTabs == nil {
		m.consoleCaptureTabs = map[string]bool{}
	}
	if m.consoleMessages == nil {
		m.consoleMessages = map[string][]ConsoleMessage{}
	}
	m.consoleCaptureTabs[tabID] = true
	m.consoleCaptureMu.Unlock()

	if err := chromedp.Run(tabCtx, runtime.Enable()); err != nil {
		m.consoleCaptureMu.Lock()
		delete(m.consoleCaptureTabs, tabID)
		m.consoleCaptureMu.Unlock()
	}
}

func (m *Manager) recordConsoleEvent(tabID string, event any) {
	var message ConsoleMessage
	switch typed := event.(type) {
	case *page.EventFrameNavigated:
		if typed.Frame != nil && typed.Frame.ParentID == "" {
			m.consoleCaptureMu.Lock()
			delete(m.consoleMessages, tabID)
			m.consoleCaptureMu.Unlock()
		}
		return
	case *runtime.EventConsoleAPICalled:
		message = consoleMessageFromEvent(typed)
	case *runtime.EventExceptionThrown:
		message = consoleMessageFromException(typed)
	default:
		return
	}
	m.consoleCaptureMu.Lock()
	defer m.consoleCaptureMu.Unlock()
	if !m.consoleCaptureTabs[tabID] {
		return
	}
	messages := append(m.consoleMessages[tabID], message)
	if len(messages) > 200 {
		messages = messages[len(messages)-200:]
	}
	m.consoleMessages[tabID] = messages
}

func consoleMessageFromEvent(event *runtime.EventConsoleAPICalled) ConsoleMessage {
	level := string(event.Type)
	if level == "warning" {
		level = "warn"
	}
	parts := make([]string, 0, len(event.Args))
	for _, arg := range event.Args {
		parts = append(parts, consoleRemoteObjectText(arg))
	}
	text := strings.Join(parts, " ")
	if len(text) > 1000 {
		text = text[:1000]
	}
	timestamp := time.Now()
	if event.Timestamp != nil {
		timestamp = time.Time(*event.Timestamp)
	}
	return ConsoleMessage{Level: level, Text: text, Timestamp: timestamp.Format(time.RFC3339Nano)}
}

func consoleMessageFromException(event *runtime.EventExceptionThrown) ConsoleMessage {
	message := ConsoleMessage{Level: "error", Text: "Uncaught exception", Timestamp: time.Now().Format(time.RFC3339Nano)}
	if event == nil {
		return message
	}
	if event.Timestamp != nil {
		message.Timestamp = time.Time(*event.Timestamp).Format(time.RFC3339Nano)
	}
	if details := event.ExceptionDetails; details != nil {
		if details.Exception != nil {
			message.Text = consoleRemoteObjectText(details.Exception)
		} else if details.Text != "" {
			message.Text = details.Text
		}
	}
	if len(message.Text) > 1000 {
		message.Text = message.Text[:1000]
	}
	return message
}

func consoleRemoteObjectText(object *runtime.RemoteObject) string {
	if object == nil {
		return "null"
	}
	if len(object.Value) > 0 {
		var value any
		if err := json.Unmarshal([]byte(object.Value), &value); err == nil {
			if text, ok := value.(string); ok {
				return text
			}
			if encoded, err := json.Marshal(value); err == nil {
				return string(encoded)
			}
		}
	}
	if object.UnserializableValue != "" {
		return string(object.UnserializableValue)
	}
	if preview := object.Preview; preview != nil && len(preview.Properties) > 0 {
		parts := make([]string, 0, min(len(preview.Properties), 12))
		for _, property := range preview.Properties {
			if property == nil || len(parts) >= 12 {
				continue
			}
			name, _ := json.Marshal(property.Name)
			value := property.Value
			if property.Type == runtime.TypeString {
				encoded, _ := json.Marshal(value)
				value = string(encoded)
			}
			parts = append(parts, string(name)+":"+value)
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	if object.Description != "" {
		return object.Description
	}
	return string(object.Type)
}

const screenshotMaxWidth = 800

const screenshotJPEGQuality = 50

const screenshotAnnotateMaxDim = 900

func (m *Manager) Screenshot(ctx context.Context) (Screenshot, error) {
	shot, err := m.CaptureArtifactScreenshot(ctx, "")
	if err != nil {
		return Screenshot{}, err
	}
	shot.Base64 = base64.StdEncoding.EncodeToString(shot.Data)
	return shot, nil
}

// CaptureArtifactScreenshot returns raw screenshot bytes without allocating a duplicate base64 string.
func (m *Manager) CaptureArtifactScreenshot(ctx context.Context, ref string) (Screenshot, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return Screenshot{}, err
	}
	defer cancel()
	if strings.TrimSpace(ref) != "" {
		box, err := snapshot.ResolveOrRecoverBox(tabCtx, ref)
		if err != nil {
			return Screenshot{}, err
		}
		clip := &page.Viewport{X: box.X, Y: box.Y, Width: box.Width, Height: box.Height, Scale: 1}
		var data []byte
		if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			var captureErr error
			data, captureErr = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).WithClip(clip).Do(ctx)
			return captureErr
		})); err != nil {
			return Screenshot{}, err
		}
		if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
			return Screenshot{}, err
		}
		return Screenshot{MIMEType: "image/png", Data: data}, nil
	}

	var data []byte
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {

		var dims []float64
		_ = chromedp.Evaluate(`[Math.round(window.innerWidth),Math.round(window.innerHeight),window.scrollX,window.scrollY]`, &dims).Do(ctx)
		var vw, vh, scrollX, scrollY float64
		if len(dims) == 4 {
			vw, vh = dims[0], dims[1]
			scrollX, scrollY = dims[2], dims[3]
		}
		if vw <= 0 || vh <= 0 {

			d, capErr := page.CaptureScreenshot().
				WithFormat(page.CaptureScreenshotFormatJpeg).
				WithQuality(screenshotJPEGQuality).Do(ctx)
			if capErr != nil {
				return capErr
			}
			data = d
			return nil
		}
		scale := 1.0
		if vw > screenshotMaxWidth {
			scale = screenshotMaxWidth / vw
		}
		d, capErr := page.CaptureScreenshot().
			WithFormat(page.CaptureScreenshotFormatJpeg).
			WithQuality(screenshotJPEGQuality).
			WithClip(&page.Viewport{X: scrollX, Y: scrollY, Width: vw, Height: vh, Scale: scale}).Do(ctx)
		if capErr != nil {
			return capErr
		}
		data = d
		return nil
	})); err != nil {
		return Screenshot{}, err
	}
	if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
		return Screenshot{}, err
	}
	return Screenshot{MIMEType: "image/jpeg", Data: data}, nil
}

// CapturePDF renders the active page as a PDF on the browser host.
func (m *Manager) CapturePDF(ctx context.Context) ([]byte, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	var data []byte
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		var printErr error
		data, _, printErr = page.PrintToPDF().WithPrintBackground(true).Do(ctx)
		return printErr
	})); err != nil {
		return nil, err
	}
	if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
		return nil, err
	}
	return data, nil
}

// ScreenshotAnnotated captures a Set-of-Marks (SoM) screenshot: it takes an authoritative snapshot in the given mode (defaulting to "frontier"), draws a transient labelled box over each frontier element using the SAME refs the snapshot returned, captures the PNG via CDP, removes the overlay, and returns the PNG plus a ref->box legend.
func (m *Manager) ScreenshotAnnotated(ctx context.Context, aopts AnnotatedScreenshotOptions) (AnnotatedScreenshot, error) {
	if err := GuardCrossOriginRefs("screenshot annotate", DirectCrossOriginRemedy, aopts.Ref); err != nil {
		return AnnotatedScreenshot{}, err
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return AnnotatedScreenshot{}, err
	}
	defer cancel()

	mode := aopts.Mode
	if strings.TrimSpace(mode) == "" {
		mode = snapshot.DefaultSnapshotMode
	}
	opts := snapshot.NormalizeOptions(snapshot.SnapshotOptions{Mode: mode})
	snap, err := snapshot.EvaluateWithOptions(tabCtx, opts)
	if err != nil {
		return AnnotatedScreenshot{}, err
	}
	if err := m.enforceFinalURL(tabID, tabCtx, snap.URL); err != nil {
		return AnnotatedScreenshot{}, err
	}
	m.refs.Observe(tabID, snap.Elements)

	clip, clipErr := m.resolveAnnotationClip(tabCtx, aopts)
	if clipErr != nil {
		return AnnotatedScreenshot{}, clipErr
	}

	marks := make([]snapshot.AnnotationMark, 0, len(snap.Elements))
	meta := make(map[string]snapshot.Element, len(snap.Elements))
	for _, el := range snap.Elements {
		if !el.InViewport {
			continue
		}
		marks = append(marks, snapshot.AnnotationMark{Ref: el.Ref, Name: el.Name, Role: el.Role})
		meta[el.Ref] = el
	}

	boxes, err := snapshot.InjectAnnotationOverlay(tabCtx, marks)

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(tabCtx), 3*time.Second)
		defer cancel()
		_, _ = snapshot.RemoveAnnotationOverlay(cleanupCtx)
	}()
	if err != nil {
		return AnnotatedScreenshot{}, err
	}

	var data []byte
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {

		capClip := clip
		if capClip == nil {
			var dims []float64
			_ = chromedp.Evaluate(`[Math.round(window.innerWidth),Math.round(window.innerHeight)]`, &dims).Do(ctx)
			if len(dims) == 2 && dims[0] > 0 && dims[1] > 0 {
				capClip = &page.Viewport{X: 0, Y: 0, Width: dims[0], Height: dims[1], Scale: 1}
			}
		}

		if capClip != nil {
			var offset [2]float64
			if err := chromedp.Evaluate(`[window.scrollX,window.scrollY]`, &offset).Do(ctx); err != nil {
				return err
			}
			captureClip := *capClip
			capClip = &captureClip
			capClip.X += offset[0]
			capClip.Y += offset[1]
			longest := capClip.Width
			if capClip.Height > longest {
				longest = capClip.Height
			}
			if longest > screenshotAnnotateMaxDim {
				capClip.Scale = screenshotAnnotateMaxDim / longest
			}
			var capErr error
			data, capErr = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).WithClip(capClip).Do(ctx)
			return capErr
		}
		return chromedp.CaptureScreenshot(&data).Do(ctx)
	})); err != nil {
		return AnnotatedScreenshot{}, err
	}
	if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
		return AnnotatedScreenshot{}, err
	}

	legend := make(map[string]LegendEntry, len(boxes))
	for _, b := range boxes {
		if !b.OK {
			continue
		}

		if clip != nil && !boxIntersectsClip(b, clip) {
			continue
		}
		el := meta[b.Ref]
		legend[b.Ref] = LegendEntry{
			Ref:    b.Ref,
			Name:   el.Name,
			Role:   el.Role,
			X:      b.X,
			Y:      b.Y,
			Width:  b.Width,
			Height: b.Height,
		}
	}

	return AnnotatedScreenshot{
		MIMEType: "image/png",
		Data:     data,
		Base64:   base64.StdEncoding.EncodeToString(data),
		Legend:   legend,
	}, nil
}

const annotationClipMargin = 18.0

func (m *Manager) resolveAnnotationClip(tabCtx context.Context, aopts AnnotatedScreenshotOptions) (*page.Viewport, error) {
	var x, y, w, h float64
	switch {
	case strings.TrimSpace(aopts.Ref) != "":
		box, err := snapshot.ResolveOrRecoverBox(tabCtx, aopts.Ref)
		if err != nil {
			return nil, err
		}
		x = box.ViewportX - box.Width/2 - annotationClipMargin
		y = box.ViewportY - box.Height/2 - annotationClipMargin
		w = box.Width + 2*annotationClipMargin
		h = box.Height + 2*annotationClipMargin
	case !aopts.Region.IsZero():
		x = aopts.Region.X - annotationClipMargin
		y = aopts.Region.Y - annotationClipMargin
		w = aopts.Region.Width + 2*annotationClipMargin
		h = aopts.Region.Height + 2*annotationClipMargin
	default:
		return nil, nil
	}

	vw, vh := m.viewportSize(tabCtx)
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if vw > 0 && x+w > vw {
		w = vw - x
	}
	if vh > 0 && y+h > vh {
		h = vh - y
	}
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("screenshot clip resolves to an empty region")
	}
	return &page.Viewport{X: x, Y: y, Width: w, Height: h, Scale: 1}, nil
}

func (m *Manager) viewportSize(tabCtx context.Context) (float64, float64) {
	var dims struct {
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	expr := `({w: window.innerWidth||document.documentElement.clientWidth||0, h: window.innerHeight||document.documentElement.clientHeight||0})`
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(expr, &dims)); err != nil {
		return 0, 0
	}
	return dims.W, dims.H
}

func boxIntersectsClip(b snapshot.AnnotationBox, clip *page.Viewport) bool {
	return b.X < clip.X+clip.Width && b.X+b.Width > clip.X &&
		b.Y < clip.Y+clip.Height && b.Y+b.Height > clip.Y
}

func (m *Manager) ScreenshotElement(ctx context.Context, ref string) (Screenshot, error) {
	if err := GuardCrossOriginRefs("screenshot element", DirectCrossOriginRemedy, ref); err != nil {
		return Screenshot{}, err
	}
	shot, err := m.CaptureArtifactScreenshot(ctx, ref)
	if err != nil {
		return Screenshot{}, err
	}
	shot.Base64 = base64.StdEncoding.EncodeToString(shot.Data)
	return shot, nil
}

func (m *Manager) ExecutePlan(ctx context.Context, steps []PlanStep) (PlanResult, error) {
	if err := GuardCrossOriginRefs("plan", DirectCrossOriginRemedy, PlanStepRefs(steps)...); err != nil {
		return PlanResult{}, err
	}
	if err := m.guardTakeover("plan"); err != nil {
		return PlanResult{}, err
	}
	entry, release := m.cancels.register(ctx, cancelToken(ctx, ""))
	defer release()
	return runPlanSteps(entry.ctx, entry, steps, m.executePlanStep), nil
}

func runPlanSteps(ctx context.Context, c interface{ Cancelled() bool }, steps []PlanStep, run func(context.Context, int, PlanStep) PlanStepResult) PlanResult {
	result := PlanResult{OK: true, Steps: make([]PlanStepResult, 0, len(steps))}
	for i, step := range steps {

		if c.Cancelled() {
			result.Cancelled = true
			result.OK = false
			result.Error = "cancelled"
			result.StepsCompleted = len(result.Steps)
			return result
		}
		stepResult := run(ctx, i, step)
		result.Steps = append(result.Steps, stepResult)
		if !stepResult.OK {

			if c.Cancelled() {
				result.Cancelled = true
				result.OK = false
				result.Error = "cancelled"
				result.Steps = result.Steps[:len(result.Steps)-1]
				result.StepsCompleted = i
				return result
			}
			result.OK = false
			failedAt := i
			result.FailedAt = &failedAt
			result.Error = stepResult.Error
			result.StepsCompleted = i
			return result
		}
	}
	result.StepsCompleted = len(result.Steps)
	return result
}

func (m *Manager) executePlanStep(ctx context.Context, index int, step PlanStep) PlanStepResult {
	sr := PlanStepResult{Index: index, Action: step.Action, OK: true}

	if err := GateSequenceStep(ctx, index, TabIDFromContext(ctx), step.ConsentProbe()); err != nil {
		sr.OK = false
		sr.Error = err.Error()
		return sr
	}
	if err := m.pacer.BeforeSequenceStep(ctx, TabIDFromContext(ctx), step.Action); err != nil {
		sr.OK = false
		sr.Error = err.Error()
		return sr
	}

	if step.ExpectRef != "" {
		findResult, err := m.Find(ctx, snapshot.FindOptions{Query: step.ExpectRef, Limit: 1})
		if err != nil {
			sr.OK = false
			sr.Error = fmt.Sprintf("expect_ref %q lookup failed: %v", step.ExpectRef, err)
			return sr
		}
		if len(findResult.Elements) == 0 {
			sr.OK = false
			sr.Error = fmt.Sprintf("expect_ref %q not found", step.ExpectRef)
			return sr
		}
		if step.ExpectRole != "" && findResult.Elements[0].Role != step.ExpectRole {
			sr.OK = false
			sr.Error = fmt.Sprintf("expect_ref %q has role %q, expected %q", step.ExpectRef, findResult.Elements[0].Role, step.ExpectRole)
			return sr
		}
	}

	var actionErr error
	switch step.Action {
	case "click":
		if step.Ref == "" {
			actionErr = errors.New("click requires ref")
			break
		}
		var actionResult ActionResult
		actionResult, actionErr = m.Click(ctx, step.Ref)
		sr.Result = actionResult
	case "click_text":
		if step.Text == "" {
			actionErr = errors.New("click_text requires text")
			break
		}
		var actionResult ActionResult
		actionResult, actionErr = m.ClickText(ctx, snapshot.ClickTextOptions{Text: step.Text})
		sr.Result = actionResult
	case "find_act":

		if step.Find == nil {
			actionErr = errors.New("find_act requires find")
			break
		}
		var findResult FindActResult
		findResult, actionErr = RunFindAct(ctx, m, *step.Find)
		if actionErr == nil {
			sr.Result = findResult
			sr.Message = "find_act " + findResult.Action + " " + findResult.Matched.Ref
		}
	case "type":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("type requires ref and text")
			break
		}
		var actionResult ActionResult
		actionResult, actionErr = m.Type(ctx, step.Ref, step.Text)
		sr.Result = actionResult
	case "fill":
		var actionResult ActionResult
		actionResult, actionErr = m.Fill(ctx, snapshot.FillOptions{Ref: step.Ref, Text: step.Text, Value: step.Value, Replace: true})
		sr.Result = actionResult
	case "select":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("select requires ref and value")
			break
		}
		var actionResult ActionResult
		actionResult, actionErr = m.Select(ctx, step.Ref, step.Value)
		sr.Result = actionResult
	case "press":
		if step.Key == "" {
			actionErr = errors.New("press requires key")
			break
		}
		var actionResult ActionResult
		actionResult, actionErr = m.Press(ctx, step.Key)
		sr.Result = actionResult
	case "scroll":
		var actionResult ActionResult
		actionResult, actionErr = m.Scroll(ctx, step.Direction)
		sr.Result = actionResult
	case "hover":
		if step.Ref == "" {
			actionErr = errors.New("hover requires ref")
			break
		}
		var actionResult ActionResult
		actionResult, actionErr = m.Hover(ctx, step.Ref)
		sr.Result = actionResult
	case "wait":
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = m.timeout
		}
		actionErr = m.WaitFor(ctx, step.Condition, timeout)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "wait matched " + step.Condition, "condition": step.Condition}
		}
	case "read":
		var read readability.PageRead
		read, actionErr = m.Read(ctx)
		sr.Result = read
		if actionErr == nil {
			sr.Message = "read captured"
		}
	case "snapshot":
		snap, err := m.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
		if err != nil {
			actionErr = err
			break
		}
		sr.Snapshot = &snap
		sr.Result = snap
		sr.Message = "snapshot captured"
	case "open":
		if step.URL == "" {
			actionErr = errors.New("open requires url")
			break
		}
		var openRes OpenResult
		openRes, actionErr = m.Open(ctx, step.URL)
		sr.Result = openRes
		if actionErr == nil {
			actionErr = openRes.NavigationErr()
		}
	case "navigate_to":

		if step.URL == "" {
			actionErr = errors.New("navigate_to requires url")
			break
		}
		var actionResult ActionResult
		actionResult, actionErr = m.NavigateTo(ctx, step.URL)
		sr.Result = actionResult
	case "focus_tab":
		if step.ID == "" {
			actionErr = errors.New("focus_tab requires id")
			break
		}
		actionErr = m.FocusTab(ctx, step.ID)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "focused tab " + step.ID, "tab_id": step.ID}
		}
	default:
		actionErr = fmt.Errorf("unknown action %q", step.Action)
	}

	if actionErr != nil {
		sr.OK = false
		sr.Error = actionErr.Error()
	}
	if sr.Message == "" && sr.OK {
		sr.Message = step.Action + " ok"
	}
	return sr
}

// ExecuteBatch executes multiple actions sequentially without intermediate observations, then returns a single compact observation at the end.
func (m *Manager) ExecuteBatch(ctx context.Context, steps []BatchStep) (BatchResult, error) {
	if err := GuardCrossOriginRefs("batch", DirectCrossOriginRemedy, BatchStepRefs(steps)...); err != nil {
		return BatchResult{}, err
	}
	if err := m.guardTakeover("batch"); err != nil {
		return BatchResult{}, err
	}
	entry, release := m.cancels.register(ctx, cancelToken(ctx, ""))
	defer release()

	ctx = entry.ctx

	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return BatchResult{}, err
	}
	defer func() { cancel() }()
	tabCtx = carryConsentHooks(ctx, tabCtx)

	result := BatchResult{OK: true, Steps: make([]BatchStepResult, 0, len(steps)), TabID: tabID}
	for i, step := range steps {

		if entry.Cancelled() {
			result.Cancelled = true
			result.OK = false
			result.Error = "cancelled"
			break
		}
		stepCtx := tabCtx
		if i+1 < len(steps) {
			stepCtx = batchReadinessContext(ctx, tabCtx, step, steps[i+1])
		}
		sr := m.executeBatchStep(stepCtx, tabID, i, step)
		result.Steps = append(result.Steps, sr)
		if !sr.OK {
			if entry.Cancelled() {
				result.Cancelled = true
				result.OK = false
				result.Error = "cancelled"

				result.Steps = result.Steps[:len(result.Steps)-1]
				break
			}
			result.OK = false
			result.Error = sr.Error
			break
		}
		if step.Action == "open" || step.Action == "focus_tab" {
			if newTabID, newTabCtx, newCancel, err := m.activeContext(ctx); err == nil {
				cancel()
				tabID = newTabID
				tabCtx = carryConsentHooks(ctx, newTabCtx)
				cancel = newCancel
				result.TabID = tabID
				result.Steps[len(result.Steps)-1].TabID = tabID
				if step.Action == "open" {
					result.Steps[len(result.Steps)-1].NewTabID = tabID
				}
			}
		}
		if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
			result.OK = false
			result.Error = err.Error()
			break
		}
	}
	result.StepsCompleted = len(result.Steps)

	snap, snapErr := snapshot.EvaluateWithOptions(tabCtx, snapshot.SnapshotOptions{ViewportOnly: true})
	if snapErr == nil {
		if guardErr := m.enforceFinalURL(tabID, tabCtx, snap.URL); guardErr != nil {
			result.OK = false
			if result.Error == "" {
				result.Error = guardErr.Error()
			}
			return result, nil
		}
	}
	if snapErr == nil {
		m.refs.Observe(tabID, snap.Elements)
		result.URL = snap.URL
		result.Title = snap.Title
		if snap.Metadata != nil {
			result.Version = MetadataInt64(snap.Metadata["version"])
			if focus, ok := snap.Metadata["focused_ref"].(string); ok {
				result.Focus = focus
			}
		}
		frontier := SelectFrontierElements(snap.Elements, result.Focus, 12)
		result.Changed = SummarizeElements(frontier, 12)
	} else if result.Error == "" {
		result.OK = false
		result.Error = "final batch observation failed: " + snapErr.Error()
	}
	return result, nil
}

func (m *Manager) executeBatchStep(tabCtx context.Context, tabID string, index int, step BatchStep) BatchStepResult {
	sr := BatchStepResult{Index: index, Action: step.Action, OK: true}

	if err := m.guardTakeoverStep(step); err != nil {
		sr.OK = false
		sr.Error = err.Error()
		return sr
	}

	if err := GateSequenceStep(tabCtx, index, tabID, step.ConsentProbe()); err != nil {
		sr.OK = false
		sr.Error = err.Error()
		return sr
	}
	if err := m.pacer.BeforeSequenceStep(tabCtx, tabID, step.Action); err != nil {
		sr.OK = false
		sr.Error = err.Error()
		return sr
	}

	m.recordAgentInteraction(tabID, step.Action)

	var actionErr error
	switch step.Action {
	case "click":
		if step.Ref == "" {
			actionErr = errors.New("click requires ref")
			break
		}
		actionErr = snapshot.WaitForActionable(tabCtx, step.Ref, 5000)
		if actionErr == nil {
			if modifiers := input.Modifier(m.heldModifierMask(tabID)); modifiers != 0 {
				_, actionErr = m.clickElementCenterWithModifiers(tabCtx, step.Ref, modifiers)
			} else {
				_, actionErr = m.clickElementCenter(tabCtx, step.Ref, actionSettleDelay)
			}
		}
	case "click_text":
		if step.Text == "" {
			actionErr = errors.New("click_text requires text")
			break
		}
		_, actionErr = m.clickTextTarget(tabCtx, tabID, snapshot.ClickTextOptions{Text: step.Text}, actionSettleDelay)
	case "find_act":

		if step.Find == nil {
			actionErr = errors.New("find_act requires find")
			break
		}
		var ref string
		ref, actionErr = RunFindActStep(tabCtx, m.tabFinder(tabID), m.findActuator(tabID), *step.Find)
		if ref != "" {
			sr.Ref = ref
		}
	case "type":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("type requires ref and text")
			break
		}
		actionErr = m.typeRef(tabCtx, step.Ref, step.Text)
	case "fill":
		if step.Ref == "" {
			actionErr = errors.New("fill requires ref")
			break
		}
		opts := snapshot.FillOptions{Text: step.Text, Value: step.Value}
		actionErr = m.fillRef(tabCtx, step.Ref, opts.EffectiveText(), true)
	case "select":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("select requires ref and value")
			break
		}
		_, actionErr = m.selectValue(tabCtx, step.Ref, step.Value)
	case "press":
		if step.Key == "" {
			actionErr = errors.New("press requires key")
			break
		}
		actionErr = m.runWithPrearmedSettle(tabCtx, actionSettleDelay, func() error {
			return m.pressKey(tabCtx, tabID, step.Key)
		})
	case "scroll":
		actionErr = m.runWithPrearmedSettle(tabCtx, actionSettleDelayFast, func() error {
			_, scrollErr := m.scrollDirection(tabCtx, step.Direction)
			return scrollErr
		})
	case "hover":
		if step.Ref == "" {
			actionErr = errors.New("hover requires ref")
			break
		}
		_, actionErr = m.hoverRef(tabCtx, tabID, step.Ref)
	case "wait":
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = m.timeout
		}
		actionErr = m.WaitFor(tabCtx, step.Condition, timeout)
	case "open":
		if step.URL == "" {
			actionErr = errors.New("open requires url")
			break
		}
		var openRes OpenResult
		openRes, actionErr = m.Open(tabCtx, step.URL)
		if actionErr == nil {
			actionErr = openRes.NavigationErr()
		}
	case "navigate_to":

		if step.URL == "" {
			actionErr = errors.New("navigate_to requires url")
			break
		}
		var url string
		url, actionErr = m.prepareNavigationURL(step.URL)
		if actionErr == nil {

			m.recordAgentNavigation(tabID, url)
			actionErr = chromedp.Run(tabCtx, chromedp.Navigate(url))
		}
	case "focus_tab":
		if step.ID == "" {
			actionErr = errors.New("focus_tab requires id")
			break
		}
		actionErr = m.FocusTab(tabCtx, step.ID)
	case "assert_visible":
		if step.Ref == "" {
			actionErr = errors.New("assert_visible requires ref")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = snapshot.EvalAssert(tabCtx, snapshot.AssertVisibleScript, step.Ref, timeout.Milliseconds())
	case "assert_text":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("assert_text requires ref and text")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = snapshot.EvalAssert(tabCtx, snapshot.AssertTextScript, step.Ref, step.Text, timeout.Milliseconds())
	case "assert_value":
		if step.Ref == "" {
			actionErr = errors.New("assert_value requires ref")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = snapshot.EvalAssert(tabCtx, snapshot.AssertValueScript, step.Ref, step.Value, timeout.Milliseconds())
	case "assert_hidden":
		if step.Ref == "" {
			actionErr = errors.New("assert_hidden requires ref")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = snapshot.EvalAssert(tabCtx, snapshot.AssertHiddenScript, step.Ref, timeout.Milliseconds())
	case "assert":

		if step.Assertion == nil {
			actionErr = errors.New("assert requires assertion")
			break
		}
		_, actionErr = evaluateAssertion(tabCtx, m, *step.Assertion)
	default:
		actionErr = fmt.Errorf("unknown action %q", step.Action)
	}

	if actionErr != nil {
		sr.OK = false
		sr.Error = actionErr.Error()
	}
	return sr
}

func (m *Manager) observeActionWithBefore(tabID string, tabCtx context.Context, message string, before *SemanticState) (result ActionResult) {
	result = ActionResult{OK: true, Message: message, TabID: tabID}

	defer m.warnHeldKeys(tabID, &result)
	snap, err := snapshot.EvaluateWithOptions(tabCtx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		result.OK = false
		result.Message = "observation failed: " + m.guardPageError(tabID, tabCtx, err).Error()
		return result
	}
	if err := m.enforceFinalURL(tabID, tabCtx, snap.URL); err != nil {
		result.OK = false
		result.Message = err.Error()
		return result
	}
	m.refs.Observe(tabID, snap.Elements)
	result.URL = snap.URL
	result.Title = snap.Title
	if snap.Metadata != nil {
		result.Version = MetadataInt64(snap.Metadata["version"])
		if focus, ok := snap.Metadata["focused_ref"].(string); ok {
			result.Focus = focus
		}
	}
	after := NewSemanticState(snap)
	ApplyStateDiff(&result, before, after)
	m.storeState(tabID, after)
	frontier := SelectFrontierElements(snap.Elements, result.Focus, 12)
	result.Elements = frontier
	result.Changed = SummarizeElements(frontier, 12)
	if WantSnapshotFromCtx(tabCtx) {
		result.Snapshot = &snap
	}
	return result
}

func captureSemanticState(tabCtx context.Context) *SemanticState {
	snap, err := snapshot.EvaluateWithOptions(tabCtx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		return nil
	}
	state := NewSemanticState(snap)
	return &state
}

func (m *Manager) cachedBefore(tabID string, tabCtx context.Context) *SemanticState {
	m.stateMu.Lock()
	cached := m.lastState[tabID]
	m.stateMu.Unlock()
	if cached != nil {
		return cached
	}
	return captureSemanticState(tabCtx)
}

func (m *Manager) storeState(tabID string, state SemanticState) {
	if tabID == "" {
		return
	}
	s := state
	m.stateMu.Lock()
	m.lastState[tabID] = &s
	m.versions[tabID] = m.versions[tabID] + 1
	m.stateMu.Unlock()
}

func (m *Manager) invalidateState(tabID string) {
	m.stateMu.Lock()
	delete(m.lastState, tabID)
	delete(m.observedState, tabID)
	delete(m.versions, tabID)
	m.stateMu.Unlock()
}

func (m *Manager) refIdentity(tabID, ref string) (name, role string, nameIsText bool) {
	if ref == "" {
		return "", "", false
	}
	if el, ok := m.refs.Get(tabID, ref); ok {
		return el.Name, el.Role, el.NameIsVisibleText
	}
	return "", "", false
}

func (m *Manager) recordObservation(tabID, action, text string, start time.Time, err error) {
	if tabID == "" {
		return
	}
	m.recordTrace(tabID, NewObservationTrace(action, text, start, err))
}

func (m *Manager) recordTrace(tabID string, entry TraceEntry) {
	entry.TabID = tabID
	if entry.Ref != "" {
		if el, ok := m.refs.Get(tabID, entry.Ref); ok {
			if entry.Name == "" {
				entry.Name = el.Name
				entry.Role = el.Role
				entry.NameIsVisibleText = el.NameIsVisibleText
			}

			if el.Sensitive {
				entry.Text = ""
				entry.Value = ""
				entry.Redacted = true
			}
		}
	}

	m.traceMu.Lock()
	if n := len(m.trace); n > 0 && IsCollapsibleAction(entry.Action) && RepeatsTraceEntry(m.trace[n-1], entry) {
		m.trace[n-1].Repeat++
		m.trace[n-1].DurationMS += entry.DurationMS
		m.trace[n-1].Timestamp = entry.Timestamp
		m.traceMu.Unlock()
		return
	}
	m.trace = append(m.trace, entry)
	if len(m.trace) > 500 {
		m.trace = m.trace[len(m.trace)-500:]
	}
	m.traceMu.Unlock()

	m.publishTrace(entry)
}

func (m *Manager) GetTrace() TraceResult {
	m.traceMu.Lock()
	entries := make([]TraceEntry, len(m.trace))
	copy(entries, m.trace)
	m.traceMu.Unlock()
	return TraceResult{Entries: entries, Count: len(entries)}
}

func (m *Manager) ClearTrace() {
	m.traceMu.Lock()
	m.trace = m.trace[:0]
	m.traceMu.Unlock()
}

type ObserveResult struct {
	Version int64    `json:"version"`
	URL     string   `json:"url,omitempty"`
	Title   string   `json:"title,omitempty"`
	Focus   string   `json:"focus,omitempty"`
	Changed []string `json:"changed,omitempty"`
	// Blocked reports subresources containment refused since the previous observation.
	Blocked []BlockedRequest `json:"blocked_requests,omitempty"`
	// ActiveRoutes reports how many interception rules are answering requests on this tab.
	ActiveRoutes int `json:"active_routes,omitempty"`
	// RouteMisses reports requests a HAR-backed route had no recorded answer for, with the most recent reasons.
	RouteMisses      int      `json:"route_fixture_misses,omitempty"`
	RouteMissReasons []string `json:"route_fixture_miss_reasons,omitempty"`
}

func (m *Manager) Observe(ctx context.Context) (ObserveResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ObserveResult{}, err
	}
	defer cancel()

	snap, err := snapshot.EvaluateWithOptions(tabCtx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		return ObserveResult{}, m.guardPageError(tabID, tabCtx, err)
	}
	if err := m.enforceFinalURL(tabID, tabCtx, snap.URL); err != nil {
		return ObserveResult{}, err
	}
	m.refs.Observe(tabID, snap.Elements)

	blocked := m.BlockedRequests(tabID)

	focus := ""
	if snap.Metadata != nil {
		if f, ok := snap.Metadata["focused_ref"].(string); ok {
			focus = f
		}
	}

	after := NewSemanticState(snap)
	m.stateMu.Lock()
	next, version, stateChanged := AdvanceObservationState(m.observedState[tabID], m.versions[tabID], after)
	m.observedState[tabID] = next
	m.versions[tabID] = version
	m.stateMu.Unlock()

	var changed []string
	if stateChanged {
		changed = SummarizeElements(SelectFrontierElements(snap.Elements, focus, 12), 12)
	}

	result := ObserveResult{
		Version: version,
		URL:     snap.URL,
		Title:   snap.Title,
		Focus:   focus,
		Changed: changed,
	}
	if len(blocked) > 0 {
		result.Blocked = blocked
	}
	result.ActiveRoutes = m.routes.count(tabID)
	result.RouteMisses, result.RouteMissReasons = m.routes.missSummary(tabID, maxObservedRouteMissReasons)
	return result, nil
}

// SummarizeElements returns compact one-line summaries of the given elements, capped at limit entries.
func SummarizeElements(elements []snapshot.Element, limit int) []string {
	if limit <= 0 || len(elements) == 0 {
		return nil
	}
	out := make([]string, 0, min(limit, len(elements)))
	for i, el := range elements {
		if i >= limit {
			break
		}
		summary := strings.TrimSpace(el.Role + " " + el.Ref + " " + strconv.Quote(el.Name))
		if el.Value != "" {
			summary += " value:" + strconv.Quote(el.Value)
		}
		if el.Disabled {
			summary += " disabled"
		}
		out = append(out, summary)
	}
	return out
}

// MetadataInt64 extracts an int64 from a metadata value that may be int64, int, float64, or json.Number.
func MetadataInt64(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}

func (m *Manager) runBrowser(ctx context.Context, fn func(context.Context) error) error {

	if err := m.checkRemoteSession(); err != nil {
		return err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	timeoutCtx, cancel := context.WithTimeout(m.browserCtx, m.timeout)
	if ctx != nil {
		if deadline, ok := ctx.Deadline(); ok {
			cancel()
			timeoutCtx, cancel = context.WithDeadline(m.browserCtx, deadline)
		}
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
	}
	defer cancel()
	return chromedp.Run(timeoutCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		c := chromedp.FromContext(ctx)
		if c == nil || c.Browser == nil {
			return errors.New("browser executor is not available")
		}
		return fn(cdp.WithExecutor(ctx, c.Browser))
	}))
}

func (m *Manager) tabContextFor(ctx context.Context) (context.Context, error) {
	tabID := tabIDFromCtx(ctx)
	if tabID == "" {
		var err error
		if tabID, err = m.ensureActive(ctx); err != nil {
			return nil, err
		}
	}
	if err := m.checkTabAccess(ctx, tabID); err != nil {
		return nil, err
	}
	tabCtx, err := m.tabContext(tabID)
	if err != nil {
		return nil, err
	}
	return carryConsentHooks(ctx, tabCtx), nil
}

func (m *Manager) activeContext(ctx context.Context) (string, context.Context, context.CancelFunc, error) {
	if tabID := tabIDFromCtx(ctx); tabID != "" {
		return m.contextForTab(ctx, tabID)
	}
	return m.activeContextWithTimeout(ctx, m.timeout)
}

func (m *Manager) contextForTab(ctx context.Context, tabID string) (string, context.Context, context.CancelFunc, error) {
	if tabID == "" {
		return m.activeContext(ctx)
	}
	if err := m.checkTabAccess(ctx, tabID); err != nil {
		return "", nil, nil, err
	}
	tabCtx, err := m.tabContext(tabID)
	if err != nil {
		return "", nil, nil, err
	}
	timeoutCtx, timeoutCancel := context.WithTimeout(carryConsentHooks(ctx, tabCtx), m.timeout)
	stop := context.AfterFunc(ctx, timeoutCancel)
	return tabID, timeoutCtx, func() { stop(); timeoutCancel() }, nil
}

func (m *Manager) activeContextWithTimeout(ctx context.Context, timeout time.Duration) (string, context.Context, context.CancelFunc, error) {
	tabID := tabIDFromCtx(ctx)
	if tabID == "" {
		var err error
		tabID, err = m.ensureActive(ctx)
		if err != nil {
			return "", nil, nil, err
		}
	}
	if err := m.checkTabAccess(ctx, tabID); err != nil {
		return "", nil, nil, err
	}
	tabCtx, err := m.tabContext(tabID)
	if err != nil {
		return "", nil, nil, err
	}
	timeoutCtx, timeoutCancel := context.WithTimeout(carryConsentHooks(ctx, tabCtx), timeout)
	stop := context.AfterFunc(ctx, timeoutCancel)
	return tabID, timeoutCtx, func() { stop(); timeoutCancel() }, nil
}

func (m *Manager) tabContext(tabID string) (context.Context, error) {

	if err := m.checkRemoteSession(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	if tab, ok := m.tabContexts[tabID]; ok {
		m.mu.RUnlock()
		return tab.ctx, nil
	}
	m.mu.RUnlock()

	ctx, cancel := chromedp.NewContext(m.browserCtx, chromedp.WithTargetID(target.ID(tabID)))

	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return emulation.SetFocusEmulationEnabled(true).Do(ctx)
	})); err != nil {
		cancel()
		return nil, err
	}

	ctx = withEventScope(ctx, tabID)

	m.mu.Lock()

	if existing, ok := m.tabContexts[tabID]; ok {
		m.mu.Unlock()
		cancel()
		return existing.ctx, nil
	}
	m.tabContexts[tabID] = tabContext{ctx: ctx, cancel: cancel}
	m.mu.Unlock()

	m.events.attachTab(tabID, ctx, func(ev any) {
		m.handleDownloadEventForTab(tabID, ev)
		m.handleDialogEvent(tabID, ctx, ev)
		m.recordConsoleEvent(tabID, ev)
	})
	m.ensureConsoleCapture(tabID, ctx)

	m.ensureContainment(tabID, ctx)
	return ctx, nil
}

var _ ActiveTabReporter = (*Manager)(nil)

// ActiveTabID names the tab an untargeted page call lands in, which is the same answer activeContext reaches on its way into every Evaluate.
func (m *Manager) ActiveTabID(ctx context.Context) (string, error) {
	if active := m.refs.Active(); active != "" {
		return active, nil
	}
	tabs, err := m.ListTabs(ctx)
	if err != nil {
		return "", err
	}
	if len(tabs) == 0 {
		return "", errors.New("no tab is open to name")
	}
	return tabs[0].ID, nil
}

func (m *Manager) ensureActive(ctx context.Context) (string, error) {
	if active := m.refs.Active(); active != "" {
		return active, nil
	}
	tabs, err := m.ListTabs(ctx)
	if err != nil {
		return "", err
	}
	if len(tabs) == 0 {
		result, err := m.Open(ctx, "about:blank")
		if err != nil {
			return "", err
		}
		return result.Tab.ID, nil
	}
	m.refs.SetActive(tabs[0].ID)
	return tabs[0].ID, nil
}

func (m *Manager) tabByID(ctx context.Context, id string) (Tab, error) {
	tabs, err := m.ListTabs(ctx)
	if err != nil {
		return Tab{}, err
	}
	for _, tab := range tabs {
		if tab.ID == id {
			return tab, nil
		}
	}
	return Tab{}, fmt.Errorf("tab %q not found", id)
}
