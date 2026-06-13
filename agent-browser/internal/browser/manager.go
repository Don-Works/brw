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
	_ = m.FocusTab(ctx, tabID)

	tab, err := m.tabByID(ctx, tabID)
	if err != nil {
		return OpenResult{Tab: Tab{ID: tabID, URL: url, Type: "page"}}, nil
	}
	return OpenResult{Tab: tab}, nil
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

	before := captureSemanticState(tabCtx)
	if err := clickElementCenter(tabCtx, ref, 150*time.Millisecond); err != nil {
		return ActionResult{}, err
	}
	return m.observeActionWithBefore(tabID, tabCtx, "clicked "+ref, before), nil
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
	before := captureSemanticState(tabCtx)
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
	before := captureSemanticState(tabCtx)
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

	before := captureSemanticState(tabCtx)
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
	before := captureSemanticState(tabCtx)
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
	before := captureSemanticState(tabCtx)
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
	before := captureSemanticState(tabCtx)
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
	_, tabCtx, cancel, err := m.activeContextWithTimeout(ctx, timeout)
	if err != nil {
		return err
	}
	defer cancel()

	deadline := time.Now().Add(timeout)
	for {
		ok, err := evaluateCondition(tabCtx, condition)
		if err == nil && ok {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("timed out waiting for %q", condition)
		}
		if err := chromedp.Run(tabCtx, chromedp.Sleep(250*time.Millisecond)); err != nil {
			return err
		}
	}
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

func evaluateCondition(ctx context.Context, condition string) (bool, error) {
	condition = strings.TrimSpace(condition)
	if condition == "" || condition == "load" {
		condition = "ready"
	}
	condJSON, _ := json.Marshal(condition)
	expr := fmt.Sprintf(`(function(condition) {
	  function roots() {
	    const out = [document];
	    for (let i = 0; i < out.length; i++) {
	      const root = out[i];
	      if (!root.querySelectorAll) continue;
	      for (const el of Array.from(root.querySelectorAll('*'))) {
	        if (el.shadowRoot) out.push(el.shadowRoot);
	      }
	    }
	    return out;
	  }
	  function hasRef(ref) {
	    const selector = '[data-agent-browser-ref="' + CSS.escape(ref) + '"]';
	    return roots().some(root => root.querySelector && root.querySelector(selector));
	  }
	  if (condition === "ready" || condition === "load") return document.readyState === "complete" || document.readyState === "interactive";
	  if (condition.startsWith("url:")) return location.href.includes(condition.slice(4));
	  if (condition.startsWith("not_url:")) return !location.href.includes(condition.slice(8));
	  if (condition.startsWith("title:")) return document.title.includes(condition.slice(6));
	  if (condition.startsWith("not_title:")) return !document.title.includes(condition.slice(10));
	  if (condition.startsWith("text:")) return document.body && document.body.innerText.includes(condition.slice(5));
	  if (condition.startsWith("not_text:")) return !document.body || !document.body.innerText.includes(condition.slice(9));
	  if (condition.startsWith("ref:")) return hasRef(condition.slice(4));
	  if (condition.startsWith("not_ref:")) return !hasRef(condition.slice(8));
	  return document.body && document.body.innerText.includes(condition);
	})(%s)`, condJSON)
	var ok bool
	err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ok))
	return ok, err
}
