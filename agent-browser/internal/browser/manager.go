package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
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

func (m *Manager) Snapshot(ctx context.Context) (snapshot.PageSnapshot, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return snapshot.PageSnapshot{}, err
	}
	defer cancel()

	snap, err := snapshot.Evaluate(tabCtx)
	if err != nil {
		return snapshot.PageSnapshot{}, err
	}
	snapshot.EnrichAccessibility(tabCtx, &snap)
	m.refs.Observe(tabID, snap.Elements)
	return snap, nil
}

func (m *Manager) Read(ctx context.Context) (readability.PageRead, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return readability.PageRead{}, err
	}
	defer cancel()

	snap, err := snapshot.Evaluate(tabCtx)
	if err == nil {
		m.refs.Observe(tabID, snap.Elements)
	}
	return readability.Evaluate(tabCtx)
}

func (m *Manager) Click(ctx context.Context, ref string) error {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	box, err := snapshot.ResolveBox(tabCtx, ref)
	if err != nil {
		return err
	}
	return chromedp.Run(tabCtx, chromedp.MouseClickXY(box.ViewportX, box.ViewportY))
}

func (m *Manager) Type(ctx context.Context, ref, text string) error {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	if err := snapshot.Focus(tabCtx, ref); err != nil {
		return err
	}
	return chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return input.InsertText(text).Do(ctx)
	}))
}

func (m *Manager) Select(ctx context.Context, ref, value string) error {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	return snapshot.Select(tabCtx, ref, value)
}

func (m *Manager) Press(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("key is required")
	}
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	return chromedp.Run(tabCtx, chromedp.KeyEvent(key))
}

func (m *Manager) Scroll(ctx context.Context, direction string) error {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	direction = strings.ToLower(strings.TrimSpace(direction))
	if direction == "" {
		direction = "down"
	}
	dx, dy := 0, 0
	switch direction {
	case "up":
		dy = -700
	case "down":
		dy = 700
	case "left":
		dx = -700
	case "right":
		dx = 700
	default:
		return fmt.Errorf("unsupported scroll direction %q", direction)
	}
	var ignored any
	expr := fmt.Sprintf(`window.scrollBy({left:%d,top:%d,behavior:"smooth"}); true`, dx, dy)
	return chromedp.Run(tabCtx, chromedp.Evaluate(expr, &ignored))
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
	  if (condition === "ready" || condition === "load") return document.readyState === "complete" || document.readyState === "interactive";
	  if (condition.startsWith("url:")) return location.href.includes(condition.slice(4));
	  if (condition.startsWith("title:")) return document.title.includes(condition.slice(6));
	  if (condition.startsWith("text:")) return document.body && document.body.innerText.includes(condition.slice(5));
	  if (condition.startsWith("ref:")) return Boolean(document.querySelector('[data-agent-browser-ref="' + CSS.escape(condition.slice(4)) + '"]'));
	  return document.body && document.body.innerText.includes(condition);
	})(%s)`, condJSON)
	var ok bool
	err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ok))
	return ok, err
}
