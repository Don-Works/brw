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

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/revitt/agent-browser/internal/actions"
	cdplaunch "github.com/revitt/agent-browser/internal/cdp"
	"github.com/revitt/agent-browser/internal/readability"
	"github.com/revitt/agent-browser/internal/snapshot"
	"github.com/revitt/agent-browser/internal/store"
)

type Manager struct {
	mu            sync.Mutex
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
}

type tabContext struct {
	ctx    context.Context
	cancel context.CancelFunc
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
		launcher:      launcher,
		allocCancel:   allocCancel,
		browserCtx:    browserCtx,
		browserCancel: browserCancel,
		tabContexts:   map[string]tabContext{},
		refs:          store.New(),
		timeout:       timeout,
		lastState:     map[string]*SemanticState{},
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
	_ = m.WaitFor(ctx, "load", 10*time.Second)
	// Do NOT activate the new tab here. OS foreground focus is reserved for the
	// explicit FocusTab/browser_focus_tab tool so automation never steals the
	// user's foreground — especially on the remote browser machine (max-air),
	// where an implicit activate raises Chrome over whatever the human is doing.
	// The tab is tracked as the active ref above; page tools bind to it via
	// chromedp.WithTargetID without needing OS activation.

	tab, err := m.tabByID(ctx, tabID)
	if err != nil {
		return OpenResult{Tab: Tab{ID: tabID, URL: url, Type: "page"}}, nil
	}
	return OpenResult{Tab: tab}, nil
}

func (m *Manager) OpenInGroup(ctx context.Context, url string, groupName string) (OpenResult, error) {
	return m.Open(ctx, url)
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
	return nil
}

func (m *Manager) Snapshot(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return snapshot.PageSnapshot{}, err
	}
	defer cancel()

	snap, err := snapshot.EvaluateWithOptions(tabCtx, opts)
	if err != nil {
		return snapshot.PageSnapshot{}, err
	}
	if opts.IncludeAX {
		snapshot.EnrichAccessibility(tabCtx, &snap)
	}
	m.refs.Observe(tabID, snap.Elements)
	return snap, nil
}

func (m *Manager) Find(ctx context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return snapshot.FindResult{}, err
	}
	defer cancel()

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

func (m *Manager) Click(ctx context.Context, ref string) (ActionResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	before := m.cachedBefore(tabID, tabCtx)
	if err := clickElementCenter(tabCtx, ref, 150*time.Millisecond); err != nil {
		return ActionResult{}, err
	}
	return m.observeActionWithBefore(tabID, tabCtx, "clicked "+ref, before), nil
}

func (m *Manager) Hover(ctx context.Context, ref string) (ActionResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	before := m.cachedBefore(tabID, tabCtx)
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	refJSON, _ := json.Marshal(ref)
	expr := fmt.Sprintf("%s(%s)", snapshot.HoverElementScript, refJSON)
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(expr, &result)); err != nil {
		return ActionResult{}, err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "hover failed"
		}
		return ActionResult{}, errors.New(result.Error)
	}
	if err := chromedp.Run(tabCtx, chromedp.Sleep(150*time.Millisecond)); err != nil {
		return ActionResult{}, err
	}
	return m.observeActionWithBefore(tabID, tabCtx, "hovered "+ref, before), nil
}

func (m *Manager) Evaluate(ctx context.Context, expression string) (any, error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	var result any
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(expression, &result, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true)
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

	if err := snapshot.Focus(tabCtx, ref); err != nil {
		return ActionResult{}, err
	}
	before := m.cachedBefore(tabID, tabCtx)
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return input.InsertText(text).Do(ctx)
	}), chromedp.Sleep(100*time.Millisecond)); err != nil {
		return ActionResult{}, err
	}
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
	before := m.cachedBefore(tabID, tabCtx)
	if err := snapshot.Fill(tabCtx, ref, opts.Text, opts.Replace); err != nil {
		return ActionResult{}, err
	}
	if err := chromedp.Run(tabCtx, chromedp.Sleep(100*time.Millisecond)); err != nil {
		return ActionResult{}, err
	}
	return m.observeActionWithBefore(tabID, tabCtx, "filled "+ref, before), nil
}

func (m *Manager) UploadFile(ctx context.Context, opts snapshot.UploadOptions) (ActionResult, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	paths, err := NormalizeUploadPaths(opts)
	if err != nil {
		return ActionResult{}, err
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
	if err := chromedp.Run(tabCtx, chromedp.Sleep(100*time.Millisecond)); err != nil {
		return ActionResult{}, err
	}
	return m.observeActionWithBefore(tabID, tabCtx, "uploaded file to "+ref, before), nil
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
	if err := chromedp.Run(tabCtx, chromedp.Sleep(100*time.Millisecond)); err != nil {
		return ActionResult{}, err
	}
	return m.observeActionWithBefore(tabID, tabCtx, "selected "+ref, before), nil
}

func (m *Manager) selectCustomOption(tabID string, tabCtx context.Context, ref, value string, before *SemanticState) (ActionResult, error) {
	if elementValueMatches(tabCtx, ref, value) {
		return m.observeAction(tabID, tabCtx, "selected "+ref+" already "+value), nil
	}
	option, err := findOptionCandidate(tabCtx, value)
	if err != nil {
		if err := clickElementCenter(tabCtx, ref, 125*time.Millisecond); err != nil {
			return ActionResult{}, fmt.Errorf("open custom select %s: %w", ref, err)
		}
		option, err = findOptionCandidate(tabCtx, value)
		if err != nil {
			return ActionResult{}, err
		}
	}
	if err := clickElementCenter(tabCtx, option.Ref, 150*time.Millisecond); err != nil {
		return ActionResult{}, fmt.Errorf("select option %s: %w", option.Ref, err)
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

func clickElementCenter(tabCtx context.Context, ref string, delay time.Duration) error {
	box, err := snapshot.ResolveBox(tabCtx, ref)
	if err != nil {
		return err
	}
	actions := []chromedp.Action{chromedp.MouseClickXY(box.ViewportX, box.ViewportY)}
	if delay > 0 {
		actions = append(actions, chromedp.Sleep(delay))
	}
	return chromedp.Run(tabCtx, actions...)
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
	}), chromedp.Sleep(150*time.Millisecond)); err != nil {
		return ActionResult{}, err
	}
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
	if err := chromedp.Run(tabCtx, chromedp.Sleep(100*time.Millisecond)); err != nil {
		return ActionResult{}, err
	}
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

	// Event-driven: a single awaited in-page promise that resolves the moment a
	// MutationObserver / nav event satisfies the condition, instead of a 250ms
	// CDP poll loop. One round-trip instead of N — the win compounds remotely.
	matched, err := snapshot.WaitForCondition(tabCtx, condition, timeout.Milliseconds())
	if err != nil {
		return err
	}
	if !matched {
		return fmt.Errorf("timed out waiting for %q", condition)
	}
	return nil
}

func (m *Manager) Screenshot(ctx context.Context) (Screenshot, error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return Screenshot{}, err
	}
	defer cancel()

	var data []byte
	if err := chromedp.Run(tabCtx, chromedp.CaptureScreenshot(&data)); err != nil {
		return Screenshot{}, err
	}
	return Screenshot{MIMEType: "image/png", Data: data, Base64: base64.StdEncoding.EncodeToString(data)}, nil
}

func (m *Manager) ScreenshotElement(ctx context.Context, ref string) (Screenshot, error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return Screenshot{}, err
	}
	defer cancel()

	box, err := snapshot.ResolveBox(tabCtx, ref)
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
	result := PlanResult{OK: true, Steps: make([]PlanStepResult, 0, len(steps))}
	for i, step := range steps {
		stepResult := m.executePlanStep(ctx, i, step)
		result.Steps = append(result.Steps, stepResult)
		if !stepResult.OK {
			result.OK = false
			failedAt := i
			result.FailedAt = &failedAt
			result.Error = stepResult.Error
			return result, nil
		}
	}
	return result, nil
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
		_, actionErr = m.Click(ctx, step.Ref)
	case "type":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("type requires ref and text")
			break
		}
		_, actionErr = m.Type(ctx, step.Ref, step.Text)
	case "fill":
		_, actionErr = m.Fill(ctx, snapshot.FillOptions{Ref: step.Ref, Text: step.Text, Replace: true})
	case "select":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("select requires ref and value")
			break
		}
		_, actionErr = m.Select(ctx, step.Ref, step.Value)
	case "press":
		if step.Key == "" {
			actionErr = errors.New("press requires key")
			break
		}
		_, actionErr = m.Press(ctx, step.Key)
	case "scroll":
		_, actionErr = m.Scroll(ctx, step.Direction)
	case "hover":
		if step.Ref == "" {
			actionErr = errors.New("hover requires ref")
			break
		}
		_, actionErr = m.Hover(ctx, step.Ref)
	case "wait":
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = m.timeout
		}
		actionErr = m.WaitFor(ctx, step.Condition, timeout)
	case "snapshot":
		snap, err := m.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
		if err != nil {
			actionErr = err
			break
		}
		sr.Snapshot = &snap
		sr.Message = "snapshot captured"
	case "open":
		if step.URL == "" {
			actionErr = errors.New("open requires url")
			break
		}
		_, actionErr = m.Open(ctx, step.URL)
	case "focus_tab":
		if step.ID == "" {
			actionErr = errors.New("focus_tab requires id")
			break
		}
		actionErr = m.FocusTab(ctx, step.ID)
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

func (m *Manager) observeActionWithBefore(tabID string, tabCtx context.Context, message string, before *SemanticState) ActionResult {
	result := ActionResult{OK: true, Message: message}
	snap, err := snapshot.EvaluateWithOptions(tabCtx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		result.Message = message + "; observation failed: " + err.Error()
		return result
	}
	m.refs.Observe(tabID, snap.Elements)
	result.URL = snap.URL
	result.Title = snap.Title
	if snap.Metadata != nil {
		result.Version = metadataInt64(snap.Metadata["version"])
		if focus, ok := snap.Metadata["focused_ref"].(string); ok {
			result.Focus = focus
		}
	}
	after := NewSemanticState(snap)
	ApplyStateDiff(&result, before, after)
	m.storeState(tabID, after)
	frontier := SelectFrontierElements(snap.Elements, result.Focus, 12)
	result.Elements = frontier
	result.Changed = summarizeElements(frontier, 12)
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
	m.stateMu.Unlock()
}

func (m *Manager) invalidateState(tabID string) {
	m.stateMu.Lock()
	delete(m.lastState, tabID)
	m.stateMu.Unlock()
}

func summarizeElements(elements []snapshot.Element, limit int) []string {
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

func metadataInt64(value any) int64 {
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
	return m.activeContextWithTimeout(ctx, m.timeout)
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
	m.mu.Lock()
	if tab, ok := m.tabContexts[tabID]; ok {
		m.mu.Unlock()
		return tab.ctx, nil
	}
	ctx, cancel := chromedp.NewContext(m.browserCtx, chromedp.WithTargetID(target.ID(tabID)))
	m.tabContexts[tabID] = tabContext{ctx: ctx, cancel: cancel}
	m.mu.Unlock()

	if err := chromedp.Run(ctx); err != nil {
		cancel()
		m.mu.Lock()
		delete(m.tabContexts, tabID)
		m.mu.Unlock()
		return nil, err
	}
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

