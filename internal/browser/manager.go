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
	"github.com/Don-Works/brw/internal/readability"
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

// Action settle delays — the upper bound on the pause after an action (click,
// type, fill, scroll, etc.) before the post-action observation snapshot. The
// settle lets the page react (DOM mutation, focus change, navigation start)
// before we read the result.
//
// These are now CAPS, not fixed sleeps: settle (below) runs an in-page,
// event-driven SettleScript that returns the moment the page demonstrably
// settles (DOM mutations quiesce ~2 frames, OR a navigation/popstate/hashchange/
// pagehide fires, OR a network response lands) and is hard-bounded by the cap so
// the worst case is exactly today's fixed delay — never slower. Named here so
// every call site shares the same bound and it is easy to tune globally.
const (
	actionSettleDelay     = 150 * time.Millisecond // click, hover, press, drag, mouse half
	actionSettleDelayFast = 100 * time.Millisecond // type, fill, select, scroll, upload
	mouseHalfSettleDelay  = 75 * time.Millisecond  // mouse_down/mouse_up press/release
	// fileChooserWaitTimeout bounds how long file-chooser-interception upload mode
	// waits for the Page.fileChooserOpened event after clicking the trigger.
	fileChooserWaitTimeout = 5 * time.Second
)

// settle waits for the page to settle after an action, bounded by cap. It
// replaces the old unconditional chromedp.Sleep(cap): event-driven so a page that
// settles fast returns in a few milliseconds, but hard-capped at cap so a page
// that never quiesces degrades to exactly the old fixed behavior.
//
// It returns the actually-observed settle duration (for trace/latency reporting).
// Like the chromedp.Sleep it replaces, it never fails the action on a settle
// error: a navigation can tear down the execution context mid-settle (the action
// itself succeeded), so an eval error degrades to "settled, duration unknown"
// rather than surfacing — matching the previous fixed-sleep semantics where a
// nav-during-sleep was invisible to the caller.
func (m *Manager) settle(tabCtx context.Context, cap time.Duration) time.Duration {
	res, err := snapshot.Settle(tabCtx, cap.Milliseconds())
	if err != nil {
		// Treat a settle eval error (commonly execution-context-destroyed during a
		// navigation the action triggered) as a non-fatal full-cap wait: the action
		// already happened, and the post-action snapshot will retry against the new
		// document. This preserves the old chromedp.Sleep contract (never errors).
		return cap
	}
	return time.Duration(res.SettledMS) * time.Millisecond
}

type Manager struct {
	mu            sync.RWMutex
	launcher      *cdplaunch.Launcher
	allocCancel   context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc
	tabContexts   map[string]tabContext
	refs          *store.RefStore
	timeout       time.Duration

	// lastState caches each tab's most-recent post-action SemanticState so the
	// next action can reuse it as its "before" baseline instead of taking a
	// second viewport snapshot. The before-state only feeds the advisory
	// ChangedState diff, so a slightly stale cache never corrupts an action
	// result — it just halves the per-action snapshot round-trips in steady state.
	stateMu   sync.Mutex
	lastState map[string]*SemanticState
	versions  map[string]int64

	traceMu sync.Mutex
	trace   []TraceEntry

	// downloads tracks file downloads observed via the Browser.downloadWillBegin /
	// Browser.downloadProgress CDP events. The listener is wired lazily on first
	// access and writes into a bounded buffer that Downloads() drains, mirroring
	// the console-drain pattern.
	downloadsMu      sync.Mutex
	downloads        []DownloadEntry
	downloadIndex    map[string]int // guid -> index into downloads
	downloadDir      string
	userDataDir      string
	downloadsEnabled bool

	// cancels tracks in-flight long-running operations (plan / batch / wait
	// loops) keyed by an operation token so brw_cancel can stop a specific
	// run cooperatively instead of killing the whole daemon.
	cancels *cancelRegistry

	// netCaptureTabs records which tabs have had the network interceptor armed
	// to re-install on every new document (so capture survives navigations).
	netCaptureMu   sync.Mutex
	netCaptureTabs map[string]bool

	// shadowPierceTabs records which tabs have had the closed-shadow piercer
	// armed at document-start (direct-CDP only), so a tab is registered once.
	shadowPierceMu   sync.Mutex
	shadowPierceTabs map[string]bool

	// webmcpEnabled gates the opt-in WebMCP runtime (navigator.modelContext); when
	// true, webmcpTabs records which tabs have had its document-start shim armed.
	webmcpEnabled bool
	webmcpMu      sync.Mutex
	webmcpTabs    map[string]bool

	// emulationStates tracks per-target DevTools device emulation so clear can
	// restore UA/platform overrides that CDP itself has no clear command for.
	emulationMu     sync.Mutex
	emulationStates map[string]deviceEmulationState
}

type tabContext struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type ctxKeyTabID struct{}

func WithTabID(ctx context.Context, tabID string) context.Context {
	return context.WithValue(ctx, ctxKeyTabID{}, tabID)
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

func tabIDFromCtx(ctx context.Context) string {
	return TabIDFromContext(ctx)
}

func New(ctx context.Context, cfg Config) (*Manager, error) {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}

	endpoint := cfg.RemoteURL
	var launcher *cdplaunch.Launcher
	var err error
	if endpoint == "" {
		launcher, err = cdplaunch.Launch(ctx, cdplaunch.LaunchConfig{
			ChromePath:       cfg.ChromePath,
			UserDataDir:      cfg.UserDataDir,
			ProfileDirectory: cfg.ProfileDirectory,
			Port:             cfg.Port,
			Extensions:       cfg.Extensions,
			Args:             cfg.ChromeArgs,
		})
		if err != nil {
			return nil, err
		}
		endpoint = launcher.Endpoint()
	}

	allocCtx, allocCancel := chromedp.NewRemoteAllocator(ctx, endpoint)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	m := &Manager{
		launcher:         launcher,
		allocCancel:      allocCancel,
		browserCtx:       browserCtx,
		browserCancel:    browserCancel,
		tabContexts:      map[string]tabContext{},
		refs:             store.New(),
		timeout:          timeout,
		lastState:        map[string]*SemanticState{},
		versions:         map[string]int64{},
		trace:            make([]TraceEntry, 0, 256),
		userDataDir:      cfg.UserDataDir,
		downloadIndex:    map[string]int{},
		cancels:          newCancelRegistry(),
		netCaptureTabs:   map[string]bool{},
		shadowPierceTabs: map[string]bool{},
		webmcpEnabled:    cfg.WebMCP,
		webmcpTabs:       map[string]bool{},
		emulationStates:  map[string]deviceEmulationState{},
	}

	if err := m.connect(); err != nil {
		_ = m.Close()
		return nil, err
	}
	if tabs, err := m.ListTabs(ctx); err == nil && len(tabs) > 0 {
		m.refs.SetActive(tabs[0].ID)
	}
	return m, nil
}

func (m *Manager) Close() error {
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
	if m.launcher != nil {
		return m.launcher.Close()
	}
	return nil
}

func (m *Manager) connect() error {
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

func (m *Manager) Open(ctx context.Context, url string) (OpenResult, error) {
	if strings.TrimSpace(url) == "" {
		url = "about:blank"
	}
	if !strings.Contains(url, "://") && url != "about:blank" {
		url = "https://" + url
	}

	var id target.ID
	if err := m.runBrowser(ctx, func(ctx context.Context) error {
		var err error
		id, err = target.CreateTarget(url).Do(ctx)
		return err
	}); err != nil {
		return OpenResult{}, err
	}
	tabID := string(id)
	m.refs.SetActive(tabID)
	// Wait for the target document to actually commit, not the transient
	// about:blank that a freshly created target reports as "ready" before the
	// real navigation lands — otherwise an immediate snapshot races to an empty
	// about:blank page. Plain about:blank opens just wait for readiness.
	var ready bool
	if url == "about:blank" {
		ready = m.WaitFor(ctx, "ready", 5*time.Second) == nil
	} else {
		ready = m.WaitFor(ctx, "committed", 10*time.Second) == nil
	}
	// Do NOT activate the new tab here. OS foreground focus is reserved for the
	// explicit FocusTab/brw_focus_tab tool so automation never steals the
	// user's foreground, especially on a remote browser machine,
	// where an implicit activate raises Chrome over whatever the human is doing.
	// The tab is tracked as the active ref above; page tools bind to it via
	// chromedp.WithTargetID without needing OS activation.

	tab, err := m.tabByID(ctx, tabID)
	if err != nil {
		return OpenResult{Tab: Tab{ID: tabID, URL: url, Type: "page"}, Ready: ready}, nil
	}
	return OpenResult{Tab: tab, Ready: ready}, nil
}

// OpenInGroup, GroupTabs, and UngroupTabs live in manager_tabgroups.go. Chrome
// tab grouping is not expressible over the DevTools Protocol, so those methods
// return ErrTabGroupingUnsupported rather than silently succeeding.

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
	if id == "" {
		return errors.New("tab id is required")
	}
	if err := m.runBrowser(ctx, func(ctx context.Context) error {
		return target.ActivateTarget(target.ID(id)).Do(ctx)
	}); err != nil {
		return err
	}
	m.refs.SetActive(id)
	return nil
}

func (m *Manager) CloseTab(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("tab id is required")
	}
	if err := m.runBrowser(ctx, func(ctx context.Context) error {
		return target.CloseTarget(target.ID(id)).Do(ctx)
	}); err != nil {
		return err
	}
	m.mu.Lock()
	if tab := m.tabContexts[id]; tab.cancel != nil {
		tab.cancel()
		delete(m.tabContexts, id)
	}
	m.mu.Unlock()
	m.refs.DropTab(id)
	m.invalidateState(id)
	// Symmetric with the other per-tab caches above: drop the network-capture
	// arm marker so the map can't grow unbounded across a long open/close churn.
	m.netCaptureMu.Lock()
	delete(m.netCaptureTabs, id)
	m.netCaptureMu.Unlock()
	m.shadowPierceMu.Lock()
	delete(m.shadowPierceTabs, id)
	m.shadowPierceMu.Unlock()
	m.webmcpMu.Lock()
	delete(m.webmcpTabs, id)
	m.webmcpMu.Unlock()
	return nil
}

// ensureWebMCP arms the opt-in WebMCP runtime shim to install at document-start
// for this tab so cooperating sites can register page tools before their own
// scripts run. No-op unless --enable-webmcp is set. Best-effort and once per tab.
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
	// Document-start covers future navigations; an immediate (idempotent) install
	// covers the current document so a site that already loaded can still register.
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

// ensureShadowPierce arms the closed-shadow piercer to (re)install at
// document-start for this tab so later navigations capture closed roots before
// the page's own scripts run. Done once per tab and best-effort: a failure here
// (e.g. the extension-bridge transport, which has no CDP document-start hook)
// must not break the snapshot, because the in-walker installer
// (__abEnsureShadowPierce) still covers post-load roots on every transport.
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
		return snapshot.PageSnapshot{}, err
	}
	if opts.IncludeAX {
		snapshot.EnrichAccessibility(tabCtx, &snap)
	}
	// Record whether accessibility was opt-in so agents can tell "not requested"
	// (available:false, requested:false) apart from "requested but the AX fetch
	// failed" (available:false, requested:true, error:...). EnrichAccessibility
	// replaces the whole summary, so set this last to survive both paths.
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
		return snapshot.FindResult{}, err
	}
	m.refs.Observe(tabID, result.Elements)
	return result, nil
}

func (m *Manager) Read(ctx context.Context) (readability.PageRead, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return readability.PageRead{}, err
	}
	defer cancel()

	snap, err := snapshot.EvaluateWithOptions(tabCtx, snapshot.SnapshotOptions{})
	if err == nil {
		m.refs.Observe(tabID, snap.Elements)
	}
	return readability.Evaluate(tabCtx)
}

func (m *Manager) ReadData(ctx context.Context) (snapshot.StructuredData, error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return snapshot.StructuredData{}, err
	}
	defer cancel()
	return snapshot.EvaluateStructured(tabCtx)
}

func (m *Manager) Click(ctx context.Context, ref string) (ActionResult, error) {
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	// Gate actuation on actionability that accepts EITHER the strict AX heuristic
	// OR geometry+hit-test (so a custom web component reporting visible:false in
	// the AX snapshot, but painted and hit-testable, still clicks). The
	// present-but-invisible case fails fast inside the script rather than burning
	// the full 5s. A "hit_test" mode means we clicked an element the AX heuristic
	// would have refused — surfaced as a warning for observability.
	actionable, err := snapshot.WaitForActionableResult(tabCtx, ref, 5000)
	if err != nil {
		return ActionResult{}, err
	}
	if !actionable.OK {
		return ActionResult{}, fmt.Errorf("element ref %q not actionable within %dms — it may be hidden, disabled, or covered by an overlay; re-run brw_snapshot to refresh refs, or brw_screenshot with ref %q to inspect it", ref, 5000, ref)
	}
	before := m.cachedBefore(tabID, tabCtx)
	// clickElementCenter already actuates by coordinate (in-page ClickXY at the
	// element box, CDP MouseClickXY fallback), which is the correct path for an
	// AX-invisible custom component resolved by hit-test.
	warning, clickErr := clickElementCenter(tabCtx, ref, 150*time.Millisecond)
	if clickErr != nil {
		return ActionResult{}, clickErr
	}
	result := m.observeActionWithBefore(tabID, tabCtx, "clicked "+ref, before)
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
	m.recordTrace(TraceEntry{
		Action:     "click",
		Ref:        ref,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}

func (m *Manager) ClickText(ctx context.Context, opts snapshot.ClickTextOptions) (ActionResult, error) {
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	before := m.cachedBefore(tabID, tabCtx)
	clicked, err := snapshot.ClickText(tabCtx, opts)
	if err != nil {
		return ActionResult{}, err
	}
	m.settle(tabCtx, actionSettleDelay)
	label := opts.Text
	if clicked.Name != "" {
		label = clicked.Name
	}
	result := m.observeActionWithBefore(tabID, tabCtx, "clicked text "+strconv.Quote(label), before)
	result.DurationMS = time.Since(start).Milliseconds()
	m.recordTrace(TraceEntry{
		Action:     "click_text",
		Text:       opts.Text,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}

func (m *Manager) Hover(ctx context.Context, ref string) (ActionResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	if err := snapshot.WaitForActionable(tabCtx, ref, 5000); err != nil {
		return ActionResult{}, err
	}
	// Move the REAL cursor to the element center via CDP. Synthetic JS mouseover
	// events (the old HoverElementScript path) do NOT trigger the CSS :hover
	// pseudo-class — only the browser's true pointer position does — so
	// :hover-gated reveals (caption overlays, dropdown/tooltip menus) never
	// appeared and agents fell back to screenshots. dispatchMouseEvent(mouseMoved)
	// updates Chromium's actual hover state, firing BOTH the native :hover styling
	// and real mouseenter/mouseover/pointermove events. Focus emulation (enabled
	// per target in tabContext) ensures delivery even when the window is backgrounded.
	x, y, recovery, err := resolvePoint(tabCtx, MousePoint{Ref: ref})
	if err != nil {
		return ActionResult{}, err
	}
	before := m.cachedBefore(tabID, tabCtx)
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx)
	})); err != nil {
		return ActionResult{}, err
	}
	m.settle(tabCtx, actionSettleDelay)
	result := m.observeActionWithBefore(tabID, tabCtx, "hovered "+ref, before)
	if recovery != "" {
		appendWarning(&result, recovery)
	}
	return result, nil
}

func (m *Manager) Evaluate(ctx context.Context, expression string) (any, error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	var result any
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		obj, exception, err := runtime.Evaluate(expression).
			WithAwaitPromise(true).
			WithReturnByValue(true).
			Do(ctx)
		if err != nil {
			return err
		}
		if exception != nil {
			details, _ := json.Marshal(exception)
			return fmt.Errorf("runtime exception: %s", details)
		}
		if obj == nil || len(obj.Value) == 0 {
			result = nil
			return nil
		}
		return json.Unmarshal(obj.Value, &result)
	})); err != nil {
		return nil, err
	}
	return result, nil
}

func (m *Manager) NetworkRequests(ctx context.Context, filter string) ([]NetworkRequest, error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
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
	return requests, nil
}

func (m *Manager) Type(ctx context.Context, ref, text string) (ActionResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	if err := snapshot.WaitForActionable(tabCtx, ref, 5000); err != nil {
		return ActionResult{}, err
	}
	if err := snapshot.Focus(tabCtx, ref); err != nil {
		return ActionResult{}, err
	}
	before := m.cachedBefore(tabID, tabCtx)
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return input.InsertText(text).Do(ctx)
	})); err != nil {
		return ActionResult{}, err
	}
	m.settle(tabCtx, actionSettleDelayFast)
	return m.observeActionWithBefore(tabID, tabCtx, "typed into "+ref, before), nil
}

func (m *Manager) Fill(ctx context.Context, opts snapshot.FillOptions) (ActionResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

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
	if err := snapshot.WaitForActionable(tabCtx, ref, 5000); err != nil {
		return ActionResult{}, err
	}
	before := m.cachedBefore(tabID, tabCtx)
	if err := snapshot.Fill(tabCtx, ref, opts.Text, opts.Replace); err != nil {
		return ActionResult{}, err
	}
	m.settle(tabCtx, actionSettleDelayFast)
	return m.observeActionWithBefore(tabID, tabCtx, "filled "+ref, before), nil
}

func (m *Manager) UploadFile(ctx context.Context, opts snapshot.UploadOptions) (ActionResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	// Resolve the upload source on the daemon host: local path(s), inline
	// bytes_base64, or a remote URL. bytes/url sources are materialized to temp
	// files here and removed once the file input has been set.
	paths, cleanup, err := ResolveUploadPaths(ctx, opts)
	if err != nil {
		return ActionResult{}, err
	}
	defer cleanup()

	// File-chooser-interception mode: when a trigger is named, click it with the
	// native chooser intercepted and set the file on whatever input the chooser
	// reports. Handles SPAs that create the input on click (which would otherwise
	// freeze the CDP session behind a native OS dialog) and inputs in cross-origin
	// iframes (backendNodeId is frame-agnostic).
	if opts.ClickRef != "" || opts.ClickText != "" {
		return m.uploadFileViaChooser(tabID, tabCtx, opts, paths)
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
	if err := snapshot.SetFileInputFiles(tabCtx, ref, paths); err != nil {
		return ActionResult{}, err
	}
	m.settle(tabCtx, actionSettleDelayFast)
	return m.observeActionWithBefore(tabID, tabCtx, "uploaded file to "+ref, before), nil
}

// uploadFileViaChooser drives the file-chooser-interception upload path on the
// direct-CDP transport: enable native-dialog interception, listen for the
// Page.fileChooserOpened event, click the trigger, then set the file on the
// chooser's backendNodeId (frame-agnostic, so it reaches cross-origin iframes).
// Interception is ALWAYS disabled on exit so the user's manual uploads in this
// Chrome are unaffected.
func (m *Manager) uploadFileViaChooser(tabID string, tabCtx context.Context, opts snapshot.UploadOptions, paths []string) (ActionResult, error) {
	if err := chromedp.Run(tabCtx, page.SetInterceptFileChooserDialog(true)); err != nil {
		return ActionResult{}, fmt.Errorf("enable file chooser interception: %w", err)
	}
	defer func() {
		// Always restore manual uploads, even on error/cancel. Use a fresh context
		// so a cancelled tabCtx cannot leave interception stuck on.
		disableCtx, cancel := context.WithTimeout(tabCtx, 2*time.Second)
		defer cancel()
		if errors.Is(disableCtx.Err(), context.Canceled) {
			disableCtx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
		}
		_ = chromedp.Run(disableCtx, page.SetInterceptFileChooserDialog(false))
	}()

	// Register the chooser listener BEFORE clicking so we never miss the event.
	chooserCh := make(chan cdp.BackendNodeID, 1)
	var once sync.Once
	chromedp.ListenTarget(tabCtx, func(ev any) {
		if e, ok := ev.(*page.EventFileChooserOpened); ok {
			once.Do(func() { chooserCh <- e.BackendNodeID })
		}
	})

	before := m.cachedBefore(tabID, tabCtx)

	// Click the trigger that opens the (now intercepted) native chooser.
	if opts.ClickRef != "" {
		if err := snapshot.WaitForActionable(tabCtx, opts.ClickRef, 5000); err != nil {
			return ActionResult{}, err
		}
		if _, err := clickElementCenter(tabCtx, opts.ClickRef, 150*time.Millisecond); err != nil {
			return ActionResult{}, fmt.Errorf("click upload trigger %s: %w", opts.ClickRef, err)
		}
	} else {
		if _, err := snapshot.ClickText(tabCtx, snapshot.ClickTextOptions{Text: opts.ClickText, Role: opts.Role}); err != nil {
			return ActionResult{}, fmt.Errorf("click upload trigger %q: %w", opts.ClickText, err)
		}
	}

	// Wait for the captured Page.fileChooserOpened event (up to ~5s).
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

	if err := chromedp.Run(tabCtx, dom.SetFileInputFiles(paths).WithBackendNodeID(backendNodeID)); err != nil {
		return ActionResult{}, err
	}
	m.settle(tabCtx, actionSettleDelayFast)
	return m.observeActionWithBefore(tabID, tabCtx, "uploaded file via intercepted file chooser", before), nil
}

func (m *Manager) Select(ctx context.Context, ref, value string) (ActionResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()
	before := m.cachedBefore(tabID, tabCtx)
	if err := snapshot.Select(tabCtx, ref, value); err != nil {
		if !strings.Contains(err.Error(), "ref is not a select element") {
			return ActionResult{}, err
		}
		return m.selectCustomOption(tabID, tabCtx, ref, value, before)
	}
	m.settle(tabCtx, actionSettleDelayFast)
	return m.observeActionWithBefore(tabID, tabCtx, "selected "+ref, before), nil
}

func (m *Manager) selectCustomOption(tabID string, tabCtx context.Context, ref, value string, before *SemanticState) (ActionResult, error) {
	if elementValueMatches(tabCtx, ref, value) {
		return m.observeAction(tabID, tabCtx, "selected "+ref+" already "+value), nil
	}
	option, err := findOptionCandidate(tabCtx, value)
	if err != nil {
		if _, clickErr := clickElementCenter(tabCtx, ref, 125*time.Millisecond); clickErr != nil {
			return ActionResult{}, fmt.Errorf("open custom select %s: %w", ref, clickErr)
		}
		option, err = findOptionCandidate(tabCtx, value)
		if err != nil {
			return ActionResult{}, err
		}
	}
	if _, clickErr := clickElementCenter(tabCtx, option.Ref, 150*time.Millisecond); clickErr != nil {
		return ActionResult{}, fmt.Errorf("select option %s: %w", option.Ref, clickErr)
	}
	return m.observeActionWithBefore(tabID, tabCtx, "selected "+ref+" via option "+option.Ref, before), nil
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

func clickElementCenter(tabCtx context.Context, ref string, delay time.Duration) (string, error) {
	box, err := snapshot.ResolveOrRecoverBox(tabCtx, ref)
	if err != nil {
		return "", err
	}
	warning := ""
	if box.Recovered {
		warning = fmt.Sprintf("ref recovered: %s -> %s", box.OldRef, box.Ref)
	}
	// Fast path: actuate the click with a single in-page round-trip. CDP
	// Input.dispatchMouseEvent (chromedp.MouseClickXY) blocks on a renderer ack
	// that costs ~0.8-1.1s per click on heavy pages; the in-page
	// pointer/mouse/click sequence fires the same handlers in one
	// Runtime.evaluate (~ms). Mirrors the extension-bridge clickRef fast path.
	// Both paths hit-test by viewport point, so semantics match; trusted CDP
	// dispatch stays as the fallback when the point is not hit-testable in-page
	// (e.g. element scrolled out of the layout viewport, elementFromPoint null).
	if inPage, evalErr := snapshot.ClickXY(tabCtx, box.ViewportX, box.ViewportY); evalErr == nil && inPage.OK {
		settleAfterClick(tabCtx, delay)
		return warning, nil
	}
	if err := chromedp.Run(tabCtx, chromedp.MouseClickXY(box.ViewportX, box.ViewportY)); err != nil {
		return "", err
	}
	settleAfterClick(tabCtx, delay)
	return warning, nil
}

// settleAfterClick runs the event-driven in-page settle bounded by cap after a
// click actuation, degrading to a no-op on a zero cap and to a non-fatal full-cap
// wait on a settle error (e.g. a navigation the click triggered tore down the
// execution context — the click already happened). It mirrors the old
// chromedp.Sleep(cap) the click paths used, but returns early when the page
// settles fast. It is a free function (clickElementCenter is not a Manager method)
// so it cannot reuse Manager.settle, but shares snapshot.Settle.
func settleAfterClick(tabCtx context.Context, cap time.Duration) {
	if cap <= 0 {
		return
	}
	_, _ = snapshot.Settle(tabCtx, cap.Milliseconds())
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
	if key == "" {
		return ActionResult{}, errors.New("key is required")
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()
	desc := actions.DescribeKey(key)
	if desc.Key == "" {
		return ActionResult{}, errors.New("key is required")
	}
	before := m.cachedBefore(tabID, tabCtx)
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		down := input.DispatchKeyEvent(input.KeyDown).
			WithModifiers(input.Modifier(desc.Modifiers)).
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
			WithModifiers(input.Modifier(desc.Modifiers)).
			WithKey(desc.Key).
			WithCode(desc.Code).
			WithWindowsVirtualKeyCode(desc.WindowsVirtualKeyCode).
			WithNativeVirtualKeyCode(desc.WindowsVirtualKeyCode).
			Do(ctx)
	})); err != nil {
		return ActionResult{}, err
	}
	m.settle(tabCtx, actionSettleDelay)
	return m.observeActionWithBefore(tabID, tabCtx, "pressed "+key, before), nil
}

func (m *Manager) Scroll(ctx context.Context, direction string) (ActionResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	direction = strings.ToLower(strings.TrimSpace(direction))
	if direction == "" {
		direction = "down"
	}
	before := m.cachedBefore(tabID, tabCtx)
	scroll, err := snapshot.Scroll(tabCtx, direction)
	if err != nil {
		return ActionResult{}, err
	}
	m.settle(tabCtx, actionSettleDelay)
	message := fmt.Sprintf("scrolled %s target:%s", direction, scroll.Target)
	if scroll.Name != "" {
		message += " " + strconv.Quote(scroll.Name)
	}
	return m.observeActionWithBefore(tabID, tabCtx, message, before), nil
}

func (m *Manager) WaitFor(ctx context.Context, condition string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = m.timeout
	}
	// Buffer the Go-side context slightly beyond the in-page timeout so the
	// page's own timer resolves the wait before the CDP call is cancelled.
	_, tabCtx, cancel, err := m.activeContextWithTimeout(ctx, timeout+2*time.Second)
	if err != nil {
		return err
	}
	defer cancel()

	deadline := time.Now().Add(timeout)
	for {
		// Cooperative cancellation: a brw_cancel on the surrounding plan/batch
		// (or this tab) cancels the caller-supplied ctx, which unblocks a long
		// wait promptly instead of running it out to the full timeout.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for %q cancelled", condition)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timed out waiting for %q", condition)
		}
		// Event-driven: one awaited in-page promise that resolves the moment a
		// MutationObserver / nav event satisfies the condition. If Chrome tears
		// down the execution context during navigation, retry inside the same
		// caller-supplied deadline.
		matched, err := snapshot.WaitForCondition(tabCtx, condition, remaining.Milliseconds())
		if err == nil {
			if matched {
				return nil
			}
			return fmt.Errorf("timed out waiting for %q", condition)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("wait for %q cancelled", condition)
		}
		if !isTransientNavigationError(err) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
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

func (m *Manager) AssertVisible(ctx context.Context, ref string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	_, tabCtx, cancel, err := m.activeContextWithTimeout(ctx, timeout+2*time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	return snapshot.EvalAssert(tabCtx, snapshot.AssertVisibleScript, ref, timeout.Milliseconds())
}

func (m *Manager) AssertText(ctx context.Context, ref, expected string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	_, tabCtx, cancel, err := m.activeContextWithTimeout(ctx, timeout+2*time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	return snapshot.EvalAssert(tabCtx, snapshot.AssertTextScript, ref, expected, timeout.Milliseconds())
}

func (m *Manager) AssertValue(ctx context.Context, ref, expected string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	_, tabCtx, cancel, err := m.activeContextWithTimeout(ctx, timeout+2*time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	return snapshot.EvalAssert(tabCtx, snapshot.AssertValueScript, ref, expected, timeout.Milliseconds())
}

func (m *Manager) AssertHidden(ctx context.Context, ref string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	_, tabCtx, cancel, err := m.activeContextWithTimeout(ctx, timeout+2*time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	return snapshot.EvalAssert(tabCtx, snapshot.AssertHiddenScript, ref, timeout.Milliseconds())
}

func (m *Manager) CommitField(ctx context.Context, ref string) error {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	return snapshot.CommitField(tabCtx, ref)
}

func (m *Manager) ClickXY(ctx context.Context, x, y float64) (snapshot.ClickXYResult, error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return snapshot.ClickXYResult{}, err
	}
	defer cancel()
	return snapshot.ClickXY(tabCtx, x, y)
}

type ConsoleMessage struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

func (m *Manager) ConsoleMessages(ctx context.Context) ([]ConsoleMessage, error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	expr := `(function() {
		if (!window.__brwConsole) return [];
		var msgs = window.__brwConsole.slice();
		window.__brwConsole.length = 0;
		return msgs;
	})()`
	var msgs []ConsoleMessage
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(expr, &msgs)); err != nil {
		return nil, err
	}
	return msgs, nil
}

// screenshotMaxWidth caps the captured pixel width of plain (non-annotated)
// screenshots. A retina/HiDPI viewport otherwise yields a multi-megabyte image
// whose base64 dominates an agent's token budget. Capping the longest side and
// encoding JPEG keeps a visual-fallback capture cheap; resolution is the lever
// that survives the harness re-encoding the image to JPEG before the model sees
// it. Annotated Set-of-Marks captures are unaffected (they need crisp PNG labels).
const screenshotMaxWidth = 800

// screenshotJPEGQuality balances legibility against bytes for plain captures.
// Resolution is the only lever that survives the harness re-encoding every capture
// to JPEG before the model sees it, so we cap dimensions aggressively and keep
// quality modest while staying legible for layout/verification reads.
const screenshotJPEGQuality = 50

// screenshotAnnotateMaxDim caps the longest side (device px) of a Set-of-Marks
// annotated capture. The legend is ref-based (semantic), so resolution is purely
// visual — and the harness re-encodes our PNG to JPEG anyway, so a smaller source
// directly shrinks what the model receives. Kept a touch above the plain cap so
// ref badges stay readable.
const screenshotAnnotateMaxDim = 900

func (m *Manager) Screenshot(ctx context.Context) (Screenshot, error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return Screenshot{}, err
	}
	defer cancel()

	var data []byte
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		// Read the CSS viewport so we can clip-capture it at a scale that caps the
		// longest side at screenshotMaxWidth (scale<=1; never upscale).
		var dims []float64
		_ = chromedp.Evaluate(`[Math.round(window.innerWidth),Math.round(window.innerHeight)]`, &dims).Do(ctx)
		var vw, vh float64
		if len(dims) == 2 {
			vw, vh = dims[0], dims[1]
		}
		if vw <= 0 || vh <= 0 {
			// Fall back to a plain capture if viewport metrics are unavailable.
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
			WithClip(&page.Viewport{X: 0, Y: 0, Width: vw, Height: vh, Scale: scale}).Do(ctx)
		if capErr != nil {
			return capErr
		}
		data = d
		return nil
	})); err != nil {
		return Screenshot{}, err
	}
	return Screenshot{MIMEType: "image/jpeg", Data: data, Base64: base64.StdEncoding.EncodeToString(data)}, nil
}

// ScreenshotAnnotated captures a Set-of-Marks (SoM) screenshot: it takes an
// authoritative snapshot in the given mode (defaulting to "frontier"), draws a
// transient labelled box over each frontier element using the SAME refs the
// snapshot returned, captures the PNG via CDP, removes the overlay, and returns
// the PNG plus a ref->box legend. The overlay is removed in every path (success
// or error) so the page the agent then acts on is never mutated. Labels are the
// exact refs an agent passes to brw_click, so the vision-grounded marks and
// the semantic action surface stay in lockstep.
//
// Edge case: if the page navigates between overlay injection and the deferred
// removal, the removal runs against the new document and the injected nodes are
// left in the now-discarded old document. They are harmless (the old document is
// gone) and the next snapshot's pre-injection cleanup clears any residue, so
// back-to-back annotated captures on a navigating page may briefly co-exist with
// stale overlay nodes until the next snapshot.
func (m *Manager) ScreenshotAnnotated(ctx context.Context, aopts AnnotatedScreenshotOptions) (AnnotatedScreenshot, error) {
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
	m.refs.Observe(tabID, snap.Elements)

	// Resolve the optional crop clip. A ref scopes the crop to that element's box
	// (plus a small margin so the label badge above it is not clipped off);
	// an explicit Region clips to a given viewport rectangle. clip==nil means a
	// full-viewport capture (today's default). The clip is in top-level viewport
	// coordinates — the SAME space the overlay labels are painted at and the CDP
	// capture clips in — so the labels line up inside the crop.
	clip, clipErr := m.resolveAnnotationClip(tabCtx, aopts)
	if clipErr != nil {
		return AnnotatedScreenshot{}, clipErr
	}

	// Build marks (and the role/name half of the legend) from the snapshot. Only
	// in-viewport elements are worth labelling — a screenshot captures the current
	// viewport, so an off-screen ref would draw nothing. When a clip is set, also
	// drop elements whose box does not intersect the clip, so the legend matches
	// exactly what is visible in the tight crop (and the crop is not littered with
	// labels for elements painted outside it).
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
	// Always tear the overlay down, even when injection itself errored partway.
	defer func() { _, _ = snapshot.RemoveAnnotationOverlay(tabCtx) }()
	if err != nil {
		return AnnotatedScreenshot{}, err
	}

	var data []byte
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		// Resolve the capture rect: the explicit crop clip, or the full CSS viewport.
		capClip := clip
		if capClip == nil {
			var dims []float64
			_ = chromedp.Evaluate(`[Math.round(window.innerWidth),Math.round(window.innerHeight)]`, &dims).Do(ctx)
			if len(dims) == 2 && dims[0] > 0 && dims[1] > 0 {
				capClip = &page.Viewport{X: 0, Y: 0, Width: dims[0], Height: dims[1], Scale: 1}
			}
		}
		// Cap device pixels: on a HiDPI display a full-res Set-of-Marks PNG balloons
		// to hundreds of KB and dominates the agent's token budget. The legend is
		// ref-based (semantic), so the image is purely for reading labels — capping
		// the longest side keeps labels legible while cutting bytes ~4x. PNG is kept
		// so the drawn ref badges stay crisp (JPEG would ring around the text).
		if capClip != nil {
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

	legend := make(map[string]LegendEntry, len(boxes))
	for _, b := range boxes {
		if !b.OK {
			continue
		}
		// When clipped, only legend boxes that intersect the crop are meaningful.
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

// annotationClipMargin pads a ref-derived crop so the label badge (drawn ~14px
// above the box top-left) and a thin border are not sliced off the edge.
const annotationClipMargin = 18.0

// resolveAnnotationClip turns the requested ref/region into a CDP viewport clip,
// clamped to the page viewport. Returns nil for a full-viewport capture.
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
	// Clamp into the viewport so the clip never has a negative origin or extends
	// past the page (CDP tolerates it, but the crop dimensions stay honest).
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

// viewportSize reads the page's layout viewport dimensions; returns 0,0 on error
// so callers treat it as "unknown" and skip the upper clamp.
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

// boxIntersectsClip reports whether an annotation box overlaps the clip rectangle
// (both in top-level viewport space), used to prune the legend to the crop.
func boxIntersectsClip(b snapshot.AnnotationBox, clip *page.Viewport) bool {
	return b.X < clip.X+clip.Width && b.X+b.Width > clip.X &&
		b.Y < clip.Y+clip.Height && b.Y+b.Height > clip.Y
}

func (m *Manager) ScreenshotElement(ctx context.Context, ref string) (Screenshot, error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return Screenshot{}, err
	}
	defer cancel()

	box, err := snapshot.ResolveOrRecoverBox(tabCtx, ref)
	if err != nil {
		return Screenshot{}, err
	}
	clip := &page.Viewport{X: box.X, Y: box.Y, Width: box.Width, Height: box.Height, Scale: 1}
	var data []byte
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		data, err = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).WithClip(clip).Do(ctx)
		return err
	})); err != nil {
		return Screenshot{}, err
	}
	return Screenshot{MIMEType: "image/png", Data: data, Base64: base64.StdEncoding.EncodeToString(data)}, nil
}

func (m *Manager) observeAction(tabID string, tabCtx context.Context, message string) ActionResult {
	return m.observeActionWithBefore(tabID, tabCtx, message, nil)
}

func (m *Manager) ExecutePlan(ctx context.Context, steps []PlanStep) (PlanResult, error) {
	entry, release := m.cancels.register(ctx, cancelToken(ctx, ""))
	defer release()
	return runPlanSteps(entry.ctx, entry, steps, m.executePlanStep), nil
}

// runPlanSteps drives the cooperative-cancellation plan loop. It is split out so
// the cancellation control flow (stop cleanly between steps, report how far we
// got, never crash) can be exercised without a live browser by injecting a fake
// step runner. The production caller passes Manager.executePlanStep.
func runPlanSteps(ctx context.Context, c interface{ Cancelled() bool }, steps []PlanStep, run func(context.Context, int, PlanStep) PlanStepResult) PlanResult {
	result := PlanResult{OK: true, Steps: make([]PlanStepResult, 0, len(steps))}
	for i, step := range steps {
		// Cooperative cancellation: stop cleanly between steps and report how far
		// we got rather than crashing or surfacing a context error.
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
			// A cancel that landed mid-step surfaces as a step failure; report it
			// as a cancellation rather than an opaque error.
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
		actionResult, actionErr = m.Fill(ctx, snapshot.FillOptions{Ref: step.Ref, Text: step.Text, Replace: true})
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

// ExecuteBatch executes multiple actions sequentially without intermediate
// observations, then returns a single compact observation at the end. This is
// much more token-efficient than calling individual tools or brw_plan.
func (m *Manager) ExecuteBatch(ctx context.Context, steps []BatchStep) (BatchResult, error) {
	entry, release := m.cancels.register(ctx, cancelToken(ctx, ""))
	defer release()
	// Carry the tab id into the cancel-aware context so per-step tab resolution
	// and the wait loops still target the right tab after we replace ctx.
	ctx = entry.ctx

	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return BatchResult{}, err
	}
	defer func() { cancel() }()

	result := BatchResult{OK: true, Steps: make([]BatchStepResult, 0, len(steps)), TabID: tabID}
	for i, step := range steps {
		// Cooperative cancellation: stop cleanly between steps and report how far
		// we got. The single end-of-batch observation below still runs so the
		// caller gets current page state for where the run stopped.
		if entry.Cancelled() {
			result.Cancelled = true
			result.OK = false
			result.Error = "cancelled"
			break
		}
		sr := m.executeBatchStep(tabCtx, i, step)
		result.Steps = append(result.Steps, sr)
		if !sr.OK {
			if entry.Cancelled() {
				result.Cancelled = true
				result.OK = false
				result.Error = "cancelled"
				// A cancel mid-step does not count the interrupted step as complete.
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
				tabCtx = newTabCtx
				cancel = newCancel
				result.TabID = tabID
			}
		}
	}
	result.StepsCompleted = len(result.Steps)

	// Single observation at the end
	snap, snapErr := snapshot.EvaluateWithOptions(tabCtx, snapshot.SnapshotOptions{ViewportOnly: true})
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
	}
	return result, nil
}

func (m *Manager) executeBatchStep(tabCtx context.Context, index int, step BatchStep) BatchStepResult {
	sr := BatchStepResult{Index: index, Action: step.Action, OK: true}

	var actionErr error
	switch step.Action {
	case "click":
		if step.Ref == "" {
			actionErr = errors.New("click requires ref")
			break
		}
		actionErr = snapshot.WaitForActionable(tabCtx, step.Ref, 5000)
		if actionErr != nil {
			break
		}
		_, actionErr = m.Click(tabCtx, step.Ref)
	case "type":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("type requires ref and text")
			break
		}
		actionErr = snapshot.WaitForActionable(tabCtx, step.Ref, 5000)
		if actionErr != nil {
			break
		}
		_, actionErr = m.Type(tabCtx, step.Ref, step.Text)
	case "fill":
		actionErr = snapshot.WaitForActionable(tabCtx, step.Ref, 5000)
		if actionErr != nil {
			break
		}
		_, actionErr = m.Fill(tabCtx, snapshot.FillOptions{Ref: step.Ref, Text: step.Text, Replace: true})
	case "select":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("select requires ref and value")
			break
		}
		actionErr = snapshot.WaitForActionable(tabCtx, step.Ref, 5000)
		if actionErr != nil {
			break
		}
		_, actionErr = m.Select(tabCtx, step.Ref, step.Value)
	case "press":
		if step.Key == "" {
			actionErr = errors.New("press requires key")
			break
		}
		_, actionErr = m.Press(tabCtx, step.Key)
	case "scroll":
		_, actionErr = m.Scroll(tabCtx, step.Direction)
	case "hover":
		if step.Ref == "" {
			actionErr = errors.New("hover requires ref")
			break
		}
		actionErr = snapshot.WaitForActionable(tabCtx, step.Ref, 5000)
		if actionErr != nil {
			break
		}
		_, actionErr = m.Hover(tabCtx, step.Ref)
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
		_, actionErr = m.Open(tabCtx, step.URL)
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
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("assert_value requires ref and value")
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
	default:
		actionErr = fmt.Errorf("unknown action %q", step.Action)
	}

	if actionErr != nil {
		sr.OK = false
		sr.Error = actionErr.Error()
	}
	return sr
}

func (m *Manager) observeActionWithBefore(tabID string, tabCtx context.Context, message string, before *SemanticState) ActionResult {
	result := ActionResult{OK: true, Message: message, TabID: tabID}
	snap, err := snapshot.EvaluateWithOptions(tabCtx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		result.OK = false
		result.Message = message + "; observation failed: " + err.Error()
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

// cachedBefore returns the tab's most-recent post-action state as the baseline
// for the next action, avoiding a snapshot round-trip. Falls back to a live
// capture when no cached state exists (first action on a freshly opened tab).
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
	delete(m.versions, tabID)
	m.stateMu.Unlock()
}

func (m *Manager) recordTrace(entry TraceEntry) {
	m.traceMu.Lock()
	m.trace = append(m.trace, entry)
	if len(m.trace) > 500 {
		m.trace = m.trace[len(m.trace)-500:]
	}
	m.traceMu.Unlock()
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
}

func (m *Manager) Observe(ctx context.Context) (ObserveResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ObserveResult{}, err
	}
	defer cancel()

	snap, err := snapshot.EvaluateWithOptions(tabCtx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		return ObserveResult{}, err
	}
	m.refs.Observe(tabID, snap.Elements)

	focus := ""
	if snap.Metadata != nil {
		if f, ok := snap.Metadata["focused_ref"].(string); ok {
			focus = f
		}
	}

	after := NewSemanticState(snap)
	m.storeState(tabID, after)

	m.stateMu.Lock()
	version := m.versions[tabID]
	prev := m.lastState[tabID]
	m.stateMu.Unlock()

	changed := SummarizeElements(SelectFrontierElements(snap.Elements, focus, 12), 12)

	if prev != nil && prev.URL == after.URL && prev.Title == after.Title && prev.Focus == after.Focus && prev.Signature == after.Signature {
		changed = nil
	}

	return ObserveResult{
		Version: version,
		URL:     snap.URL,
		Title:   snap.Title,
		Focus:   focus,
		Changed: changed,
	}, nil
}

// SummarizeElements returns compact one-line summaries of the given elements,
// capped at limit entries. Used by both the Manager and the extension Bridge.
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

// MetadataInt64 extracts an int64 from a metadata value that may be int64, int,
// float64, or json.Number. Returns 0 for unrecognized types.
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
	timeoutCtx, cancel := context.WithTimeout(m.browserCtx, m.timeout)
	defer cancel()
	if ctx != nil {
		if deadline, ok := ctx.Deadline(); ok {
			var cancel2 context.CancelFunc
			timeoutCtx, cancel2 = context.WithDeadline(m.browserCtx, deadline)
			defer cancel2()
		}
	}
	return chromedp.Run(timeoutCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		c := chromedp.FromContext(ctx)
		if c == nil || c.Browser == nil {
			return errors.New("browser executor is not available")
		}
		return fn(cdp.WithExecutor(ctx, c.Browser))
	}))
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
	tabCtx, err := m.tabContext(tabID)
	if err != nil {
		return "", nil, nil, err
	}
	timeoutCtx, timeoutCancel := context.WithTimeout(tabCtx, m.timeout)
	return tabID, timeoutCtx, timeoutCancel, nil
}

func (m *Manager) activeContextWithTimeout(ctx context.Context, timeout time.Duration) (string, context.Context, context.CancelFunc, error) {
	tabID, err := m.ensureActive(ctx)
	if err != nil {
		return "", nil, nil, err
	}
	tabCtx, err := m.tabContext(tabID)
	if err != nil {
		return "", nil, nil, err
	}
	timeoutCtx, timeoutCancel := context.WithTimeout(tabCtx, timeout)
	return tabID, timeoutCtx, timeoutCancel, nil
}

func (m *Manager) tabContext(tabID string) (context.Context, error) {
	m.mu.RLock()
	if tab, ok := m.tabContexts[tabID]; ok {
		m.mu.RUnlock()
		return tab.ctx, nil
	}
	m.mu.RUnlock()
	// Validate the context before publishing it to the map so concurrent callers
	// never observe a half-initialized entry that gets cancelled on the error path.
	ctx, cancel := chromedp.NewContext(m.browserCtx, chromedp.WithTargetID(target.ID(tabID)))

	// Validate the context and force focus emulation on this target. Without it,
	// Chrome routes keyboard/mouse input through the OS-focused RenderWidgetHost,
	// so CDP Input.dispatchKeyEvent presses are silently dropped whenever the
	// daemon's Chrome window is not the frontmost OS window (the common case for a
	// background automation browser). Input.insertText bypasses this, which is why
	// typing worked but Enter/Tab/arrow presses did not submit React/SPA forms.
	// setFocusEmulationEnabled makes the renderer treat the page as always focused.
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return emulation.SetFocusEmulationEnabled(true).Do(ctx)
	})); err != nil {
		cancel()
		return nil, err
	}

	m.mu.Lock()
	// A concurrent call may have validated and inserted while we were unlocked;
	// prefer the first-writer's context and discard ours.
	if existing, ok := m.tabContexts[tabID]; ok {
		m.mu.Unlock()
		cancel()
		return existing.ctx, nil
	}
	m.tabContexts[tabID] = tabContext{ctx: ctx, cancel: cancel}
	m.mu.Unlock()

	// If download tracking is already armed, attach the target-level listener to
	// this newly created tab context so page-initiated downloads are observed.
	m.attachDownloadListenerIfEnabled(ctx)
	return ctx, nil
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
