package extensionbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/coder/websocket"
	"github.com/revitt/agent-browser/internal/actions"
	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/readability"
	"github.com/revitt/agent-browser/internal/snapshot"
)

type Bridge struct {
	addr               string
	timeout            time.Duration
	allowedExtensionID string
	server             *http.Server

	mu      sync.RWMutex
	conn    *websocket.Conn
	hello   hello
	active  string
	pending map[string]chan response
	writeMu sync.Mutex
	nextID  atomic.Uint64

	// cancels tracks in-flight long-running operations (plan / batch / wait
	// loops) keyed by an operation token so Cancel can stop a specific run
	// cooperatively. Mirrors the browser.Manager mechanism so cancellation
	// behaves identically across the CDP and extension transports.
	cancels *cancelRegistry
}

type hello struct {
	Source   string `json:"source,omitempty"`
	Version  string `json:"version,omitempty"`
	Chrome   string `json:"chrome,omitempty"`
	Platform string `json:"platform,omitempty"`
}

type request struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Params map[string]any `json:"params,omitempty"`
}

type response struct {
	ID     string          `json:"id,omitempty"`
	Type   string          `json:"type,omitempty"`
	TabID  int             `json:"tabId,omitempty"`
	OK     bool            `json:"ok,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Hello  hello           `json:"hello,omitempty"`
}

func New(addr string, timeout time.Duration, allowedExtensionID string) *Bridge {
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	b := &Bridge{
		addr:               addr,
		timeout:            timeout,
		allowedExtensionID: strings.TrimSpace(allowedExtensionID),
		pending:            map[string]chan response{},
		cancels:            newCancelRegistry(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/extension", b.handleExtension)
	mux.HandleFunc("/status", b.handleStatus)
	b.server = &http.Server{Addr: addr, Handler: mux}
	return b
}

func (b *Bridge) ListenAndServe() error {
	return b.server.ListenAndServe()
}

func (b *Bridge) Shutdown(ctx context.Context) error {
	b.mu.Lock()
	if b.conn != nil {
		_ = b.conn.Close(websocket.StatusNormalClosure, "shutdown")
		b.conn = nil
	}
	b.mu.Unlock()
	return b.server.Shutdown(ctx)
}

func (b *Bridge) handleStatus(w http.ResponseWriter, _ *http.Request) {
	b.mu.RLock()
	connected := b.conn != nil
	hello := b.hello
	active := b.active
	b.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"connected":     connected,
		"hello":         hello,
		"active_tab_id": active,
	})
}

func (b *Bridge) handleExtension(w http.ResponseWriter, r *http.Request) {
	originPatterns := []string{"chrome-extension://*"}
	if b.allowedExtensionID != "" {
		originPatterns = []string{"chrome-extension://" + b.allowedExtensionID}
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: originPatterns,
	})
	if err != nil {
		log.Printf("extension websocket accept: %v", err)
		return
	}
	conn.SetReadLimit(4 << 20)

	if b.allowedExtensionID == "" {
		log.Printf("WARNING: extension bridge accepting connections from any Chrome extension (chrome-extension://*); set a profile policy with bridge_extension_id to restrict")
	}

	b.mu.Lock()
	if b.conn != nil {
		_ = b.conn.Close(websocket.StatusNormalClosure, "replaced by new extension connection")
	}
	b.conn = conn
	b.hello = hello{}
	b.mu.Unlock()

	log.Printf("extension bridge connected")
	b.readLoop(r.Context(), conn)
	log.Printf("extension bridge disconnected")

	b.mu.Lock()
	if b.conn == conn {
		b.conn = nil
	}
	for id, ch := range b.pending {
		delete(b.pending, id)
		ch <- response{ID: id, Error: "extension disconnected"}
		close(ch)
	}
	b.mu.Unlock()
}

func (b *Bridge) readLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			log.Printf("extension bridge read: %v", err)
			return
		}
		var resp response
		if err := json.Unmarshal(data, &resp); err != nil {
			log.Printf("extension bridge invalid message: %v", err)
			continue
		}
		if resp.Type == "hello" {
			b.mu.Lock()
			b.hello = resp.Hello
			b.mu.Unlock()
			continue
		}
		if resp.Type == "active_tab" {
			if resp.TabID != 0 {
				b.setActiveTabID(strconv.Itoa(resp.TabID))
			}
			continue
		}
		if resp.ID == "" {
			continue
		}
		b.mu.Lock()
		ch := b.pending[resp.ID]
		delete(b.pending, resp.ID)
		b.mu.Unlock()
		if ch != nil {
			ch <- resp
			close(ch)
		}
	}
}

func (b *Bridge) call(ctx context.Context, typ string, params map[string]any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	b.mu.RLock()
	conn := b.conn
	b.mu.RUnlock()
	if conn == nil {
		return nil, errors.New("extension bridge is not connected; load/click the Chrome extension first")
	}

	id := strconv.FormatUint(b.nextID.Add(1), 10)
	ch := make(chan response, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()

	msg, err := json.Marshal(request{ID: id, Type: typ, Params: params})
	if err != nil {
		return nil, err
	}
	b.writeMu.Lock()
	err = conn.Write(timeoutCtx, websocket.MessageText, msg)
	b.writeMu.Unlock()
	if err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, err
	}

	select {
	case resp := <-ch:
		if !resp.OK {
			if resp.Error == "" {
				resp.Error = "extension bridge request failed"
			}
			return nil, fmt.Errorf("extension bridge: %s", resp.Error)
		}
		return resp.Result, nil
	case <-timeoutCtx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, timeoutCtx.Err()
	}
}

func (b *Bridge) Open(ctx context.Context, url string) (browser.OpenResult, error) {
	if strings.TrimSpace(url) == "" {
		url = "about:blank"
	}
	if !strings.Contains(url, "://") && url != "about:blank" {
		url = "https://" + url
	}
	raw, err := b.call(ctx, "open_tab", map[string]any{"url": url})
	if err != nil {
		return browser.OpenResult{}, err
	}
	var tab extTab
	if err := json.Unmarshal(raw, &tab); err != nil {
		return browser.OpenResult{}, err
	}
	out := tab.toBrowserTab()
	if out.ID != "" {
		b.setActiveTabID(out.ID)
	}
	ready := b.waitOpenReady(ctx, url, out.ID)
	return browser.OpenResult{Tab: out, Ready: ready}, nil
}

// waitOpenReady blocks until the freshly opened tab is usable, matching the
// direct-CDP Open() contract so an immediate browser_evaluate / browser_read on
// the new tab doesn't race the transient about:blank Chrome reports before the
// real navigation commits. Returns whether readiness was confirmed; a wait
// timeout is not fatal (the tab still exists), it just reports ready=false. The
// wait targets the specific new tab id (not the resolved active tab) because
// open_tab creates the tab inactive, so the focused tab is still the old one.
func (b *Bridge) waitOpenReady(ctx context.Context, url, tabID string) bool {
	if tabID == "" {
		return false
	}
	waitCtx := browser.WithTabID(ctx, tabID)
	if url == "about:blank" {
		return b.WaitFor(waitCtx, "ready", 5*time.Second) == nil
	}
	return b.WaitFor(waitCtx, "committed", 10*time.Second) == nil
}

func (b *Bridge) ListTabs(ctx context.Context) ([]browser.Tab, error) {
	raw, err := b.call(ctx, "list_tabs", nil)
	if err != nil {
		return nil, err
	}
	var tabs []extTab
	if err := json.Unmarshal(raw, &tabs); err != nil {
		return nil, err
	}
	out := make([]browser.Tab, 0, len(tabs))
	activeID := ""
	fallbackActiveID := ""
	hasFocusedWindow := false
	for _, tab := range tabs {
		outTab := tab.toBrowserTab()
		out = append(out, outTab)
		if outTab.WindowFocused {
			hasFocusedWindow = true
		}
		if outTab.Active && outTab.WindowFocused {
			activeID = outTab.ID
		}
		if fallbackActiveID == "" && outTab.Active {
			fallbackActiveID = outTab.ID
		}
	}
	if activeID == "" && !hasFocusedWindow && b.activeTabID() == "" {
		activeID = fallbackActiveID
	}
	if activeID != "" {
		b.setActiveTabID(activeID)
	}
	return out, nil
}

func (b *Bridge) FocusTab(ctx context.Context, id string) error {
	tabID, err := requireTabID(id)
	if err != nil {
		return err
	}
	raw, err := b.call(ctx, "focus_tab", map[string]any{"tabId": tabID})
	if err != nil {
		return err
	}
	var tab extTab
	if err := json.Unmarshal(raw, &tab); err == nil && tab.ID != 0 {
		b.setActiveTabID(strconv.Itoa(tab.ID))
		return nil
	}
	// Unmarshal failed or returned zero ID; fall through to use the original
	// id. The focus_tab call succeeded, so the focus did happen — we just
	// cannot confirm the tab metadata.
	if strings.TrimSpace(id) != "" {
		b.setActiveTabID(id)
	}
	return nil
}

func (b *Bridge) CloseTab(ctx context.Context, id string) error {
	tabID, err := requireTabID(id)
	if err != nil {
		return err
	}
	_, err = b.call(ctx, "close_tab", map[string]any{"tabId": tabID})
	if err == nil && strings.TrimSpace(id) == b.activeTabID() {
		b.setActiveTabID("")
	}
	return err
}

func (b *Bridge) GroupTabs(ctx context.Context, tabIDs []string, name string, color string) error {
	ids := make([]int, 0, len(tabIDs))
	for _, id := range tabIDs {
		ids = append(ids, parseTabID(id))
	}
	if color == "" {
		color = "blue"
	}
	_, err := b.call(ctx, "group_tabs", map[string]any{
		"tabIds": ids,
		"name":   name,
		"color":  color,
	})
	return err
}

func (b *Bridge) UngroupTabs(ctx context.Context, tabIDs []string) error {
	ids := make([]int, 0, len(tabIDs))
	for _, id := range tabIDs {
		ids = append(ids, parseTabID(id))
	}
	_, err := b.call(ctx, "ungroup_tabs", map[string]any{"tabIds": ids})
	return err
}

func (b *Bridge) OpenInGroup(ctx context.Context, url string, groupName string) (browser.OpenResult, error) {
	if strings.TrimSpace(url) == "" {
		url = "about:blank"
	}
	if !strings.Contains(url, "://") && url != "about:blank" {
		url = "https://" + url
	}
	raw, err := b.call(ctx, "open_tab", map[string]any{
		"url":       url,
		"groupName": groupName,
	})
	if err != nil {
		return browser.OpenResult{}, err
	}
	var tab extTab
	if err := json.Unmarshal(raw, &tab); err != nil {
		return browser.OpenResult{}, err
	}
	out := tab.toBrowserTab()
	if out.ID != "" {
		b.setActiveTabID(out.ID)
	}
	ready := b.waitOpenReady(ctx, url, out.ID)
	return browser.OpenResult{Tab: out, Ready: ready}, nil
}

func (b *Bridge) Snapshot(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	var snap snapshot.PageSnapshot
	opts.IncludeAX = false
	if cached, ok := b.tryCachedSnapshot(ctx, opts); ok {
		return cached, nil
	}
	optsJSON, _ := json.Marshal(opts)
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.SnapshotFunctionScript, optsJSON), "", &snap); err != nil {
		return snap, err
	}
	snap.Accessibility = snapshot.AccessibilitySummary{
		Available: false,
		Error:     "accessibility tree is unavailable through the Chrome extension bridge; use direct CDP attach for AX enrichment",
	}
	b.storeCachedSnapshot(ctx, opts, snap)
	return snap, nil
}

func (b *Bridge) tryCachedSnapshot(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, bool) {
	if opts.Mode == "all" || opts.IncludeHidden {
		return snapshot.PageSnapshot{}, false
	}
	tabID := b.contextTabID(ctx)
	raw, err := b.call(ctx, "cached_snapshot", map[string]any{
		"tabId":    parseTabID(tabID),
		"cacheKey": snapshotCacheKey(opts),
	})
	if err != nil {
		return snapshot.PageSnapshot{}, false
	}
	var resp struct {
		Cached   bool                  `json:"cached"`
		Snapshot snapshot.PageSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || !resp.Cached {
		return snapshot.PageSnapshot{}, false
	}
	return resp.Snapshot, true
}

func (b *Bridge) storeCachedSnapshot(ctx context.Context, opts snapshot.SnapshotOptions, snap snapshot.PageSnapshot) {
	tabID := b.contextTabID(ctx)
	if tabID == "" {
		return
	}
	_, _ = b.call(ctx, "snapshot_result", map[string]any{
		"tabId":    parseTabID(tabID),
		"cacheKey": snapshotCacheKey(opts),
		"snapshot": snap,
	})
}

func snapshotCacheKey(opts snapshot.SnapshotOptions) string {
	opts.IncludeAX = false
	data, _ := json.Marshal(opts)
	return string(data)
}

func (b *Bridge) Find(ctx context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{
		Query:         opts.Query,
		Text:          opts.Text,
		Role:          opts.Role,
		Limit:         opts.Limit,
		ViewportOnly:  opts.ViewportOnly,
		IncludeHidden: opts.IncludeHidden,
	})
	if err != nil {
		return snapshot.FindResult{}, err
	}
	return snapshot.FindResult{
		URL:      snap.URL,
		Title:    snap.Title,
		Elements: snap.Elements,
		Metadata: snap.Metadata,
	}, nil
}

func (b *Bridge) Read(ctx context.Context) (readability.PageRead, error) {
	var read readability.PageRead
	err := b.evaluate(ctx, readability.ReadScript, "", &read)
	return read, err
}

func (b *Bridge) ReadData(ctx context.Context) (snapshot.StructuredData, error) {
	var data snapshot.StructuredData
	err := b.evaluate(ctx, snapshot.StructuredDataScript, "", &data)
	return data, err
}

const (
	observedActionSettle = 75 * time.Millisecond
	batchActionSettle    = 25 * time.Millisecond
	waitForPollInterval  = 250 * time.Millisecond
)

func (b *Bridge) Click(ctx context.Context, ref string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	if err := b.clickRef(ctx, ref); err != nil {
		return browser.ActionResult{}, err
	}
	time.Sleep(observedActionSettle)
	return b.observeActionWithBefore(ctx, "clicked "+ref, before), nil
}

func (b *Bridge) ClickText(ctx context.Context, opts snapshot.ClickTextOptions) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	optsJSON, _ := json.Marshal(opts)
	var clicked snapshot.ClickXYResult
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.ClickTextScript, optsJSON), "", &clicked); err != nil {
		return browser.ActionResult{}, err
	}
	if !clicked.OK {
		if clicked.Error == "" {
			clicked.Error = "click text failed"
		}
		return browser.ActionResult{}, fmt.Errorf("click text: %s", clicked.Error)
	}
	time.Sleep(observedActionSettle)
	label := opts.Text
	if clicked.Name != "" {
		label = clicked.Name
	}
	return b.observeActionWithBefore(ctx, "clicked text "+strconv.Quote(label), before), nil
}

func (b *Bridge) Hover(ctx context.Context, ref string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	if err := b.hoverRef(ctx, ref); err != nil {
		return browser.ActionResult{}, err
	}
	time.Sleep(observedActionSettle)
	return b.observeActionWithBefore(ctx, "hovered "+ref, before), nil
}

func (b *Bridge) hoverRef(ctx context.Context, ref string) error {
	refJSON, _ := json.Marshal(ref)
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.HoverElementScript, refJSON), "", &result); err != nil {
		return err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "hover failed"
		}
		return fmt.Errorf("hover: %s", result.Error)
	}
	return nil
}

func (b *Bridge) Evaluate(ctx context.Context, expression string) (any, error) {
	var result json.RawMessage
	if err := b.evaluate(ctx, expression, "", &result); err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(result, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func (b *Bridge) NetworkRequests(ctx context.Context, filter string) ([]browser.NetworkRequest, error) {
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
	var requests []browser.NetworkRequest
	if err := b.evaluate(ctx, expr, "", &requests); err != nil {
		return nil, err
	}
	return requests, nil
}

func (b *Bridge) clickRef(ctx context.Context, ref string) error {
	box, err := b.resolveBox(ctx, ref)
	if err != nil {
		if fallbackErr := b.activate(ctx, ref); fallbackErr == nil {
			return nil
		}
		return err
	}
	// Fast path: actuate the click with a single in-page round-trip. CDP
	// Input.dispatchMouseEvent blocks on a renderer ack that can cost ~1.5s per
	// call on heavy pages (≈5s for the three-event sequence below); the in-page
	// pointer/mouse/click sequence fires the same handlers in one Runtime.evaluate
	// (~tens of ms). Trusted CDP dispatch stays as the fallback when the point is
	// not hit-testable in-page (e.g. element scrolled out of the layout viewport).
	xJSON, _ := json.Marshal(box.ViewportX)
	yJSON, _ := json.Marshal(box.ViewportY)
	var inPage snapshot.ClickXYResult
	if evalErr := b.evaluate(ctx, fmt.Sprintf("%s(%s,%s)", snapshot.ClickXYScript, xJSON, yJSON), "", &inPage); evalErr == nil && inPage.OK {
		return nil
	}
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		if _, err := b.cdp(ctx, "", "Input.dispatchMouseEvent", map[string]any{
			"type":   typ,
			"x":      box.ViewportX,
			"y":      box.ViewportY,
			"button": "left",
			"buttons": func() int {
				if typ == "mousePressed" {
					return 1
				}
				return 0
			}(),
			"clickCount": 1,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bridge) activate(ctx context.Context, ref string) error {
	refJSON, _ := json.Marshal(ref)
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	expr := fmt.Sprintf(`(function(ref) {
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
	  function findByRef(ref) {
	    const selector = '[data-agent-browser-ref="' + CSS.escape(ref) + '"]';
	    for (const root of roots()) {
	      const el = root.querySelector && root.querySelector(selector);
	      if (el) return el;
	    }
	    return null;
	  }
	  const el = findByRef(ref);
	  if (!el) return { ok: false, error: 'ref not found' };
	  if (el.closest('[hidden],[aria-hidden="true"]')) return { ok: false, error: 'ref hidden' };
	  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
	  if (typeof el.focus === 'function') el.focus({ preventScroll: true });
	  el.dispatchEvent(new MouseEvent('mouseover', { bubbles: true, cancelable: true, view: window }));
	  el.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true, view: window }));
	  el.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, cancelable: true, view: window }));
	  if (typeof el.click === 'function') el.click();
	  else el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, view: window }));
	  return { ok: true };
	})(%s)`, refJSON)
	if err := b.evaluate(ctx, expr, "", &result); err != nil {
		return err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "ref activation failed"
		}
		return fmt.Errorf("activate: %s", result.Error)
	}
	return nil
}

func (b *Bridge) Type(ctx context.Context, ref, text string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	if err := b.typeRef(ctx, ref, text); err != nil {
		return browser.ActionResult{}, err
	}
	time.Sleep(observedActionSettle)
	return b.observeActionWithBefore(ctx, "typed into "+ref, before), nil
}

func (b *Bridge) typeRef(ctx context.Context, ref, text string) error {
	if err := b.focus(ctx, ref); err != nil {
		return err
	}
	_, err := b.cdp(ctx, "", "Input.insertText", map[string]any{"text": text})
	return err
}

func (b *Bridge) Fill(ctx context.Context, opts snapshot.FillOptions) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	ref, err := b.fillOptions(ctx, opts)
	if err != nil {
		return browser.ActionResult{}, err
	}
	time.Sleep(observedActionSettle)
	return b.observeActionWithBefore(ctx, "filled "+ref, before), nil
}

func (b *Bridge) fillOptions(ctx context.Context, opts snapshot.FillOptions) (string, error) {
	ref, err := b.resolveFillRef(ctx, opts)
	if err != nil {
		return "", err
	}
	refJSON, _ := json.Marshal(ref)
	textJSON, _ := json.Marshal(opts.Text)
	replaceJSON, _ := json.Marshal(opts.Replace)
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s,%s,%s)", snapshot.FillElementScript, refJSON, textJSON, replaceJSON), "", &result); err != nil {
		return "", err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "fill failed"
		}
		return "", fmt.Errorf("fill: %s", result.Error)
	}
	return ref, nil
}

func (b *Bridge) resolveFillRef(ctx context.Context, opts snapshot.FillOptions) (string, error) {
	if opts.Ref != "" {
		return opts.Ref, nil
	}
	result, err := b.Find(ctx, snapshot.FindOptions{
		Query: opts.Query,
		Role:  opts.Role,
		Limit: 1,
	})
	if err != nil {
		return "", err
	}
	if len(result.Elements) == 0 {
		return "", fmt.Errorf("no fill target found for query %q", opts.Query)
	}
	return result.Elements[0].Ref, nil
}

func (b *Bridge) UploadFile(ctx context.Context, opts snapshot.UploadOptions) (browser.ActionResult, error) {
	paths, err := browser.NormalizeUploadPaths(opts)
	if err != nil {
		return browser.ActionResult{}, err
	}

	ref := opts.Ref
	if ref == "" {
		query := opts.Query
		if strings.TrimSpace(query) == "" {
			query = "file"
		}
		result, err := b.Find(ctx, snapshot.FindOptions{
			Query: query,
			Role:  opts.Role,
			Limit: 20,
		})
		if err != nil {
			return browser.ActionResult{}, err
		}
		for _, el := range result.Elements {
			if el.Tag == "input" && el.Type == "file" {
				ref = el.Ref
				break
			}
		}
		if ref == "" {
			return browser.ActionResult{}, fmt.Errorf("no file input found for query %q", query)
		}
	}

	refJSON, _ := json.Marshal(ref)
	raw, err := b.cdp(ctx, "", "Runtime.evaluate", map[string]any{
		"expression":    fmt.Sprintf("%s(%s)", snapshot.FileInputElementScript, refJSON),
		"returnByValue": false,
		"awaitPromise":  true,
		"objectGroup":   "agent-browser-upload",
	})
	if err != nil {
		return browser.ActionResult{}, err
	}
	var eval struct {
		Result struct {
			ObjectID string `json:"objectId"`
		} `json:"result"`
		ExceptionDetails any `json:"exceptionDetails,omitempty"`
	}
	if err := json.Unmarshal(raw, &eval); err != nil {
		return browser.ActionResult{}, err
	}
	if eval.ExceptionDetails != nil {
		details, _ := json.Marshal(eval.ExceptionDetails)
		return browser.ActionResult{}, fmt.Errorf("file input resolution failed: %s", details)
	}
	if eval.Result.ObjectID == "" {
		return browser.ActionResult{}, errors.New("file input resolution returned no object id")
	}
	defer func() {
		_, _ = b.cdp(ctx, "", "Runtime.releaseObject", map[string]any{"objectId": eval.Result.ObjectID})
	}()
	before := b.captureSemanticState(ctx)
	if _, err := b.cdp(ctx, "", "DOM.setFileInputFiles", map[string]any{
		"files":    paths,
		"objectId": eval.Result.ObjectID,
	}); err != nil {
		return browser.ActionResult{}, err
	}
	var ignored any
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.FileInputEventsScript, refJSON), "", &ignored); err != nil {
		return browser.ActionResult{}, err
	}
	time.Sleep(observedActionSettle)
	return b.observeActionWithBefore(ctx, "uploaded file to "+ref, before), nil
}

func (b *Bridge) Select(ctx context.Context, ref, value string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	message, err := b.selectValue(ctx, ref, value)
	if err != nil {
		return browser.ActionResult{}, err
	}
	time.Sleep(observedActionSettle)
	return b.observeActionWithBefore(ctx, message, before), nil
}

func (b *Bridge) selectValue(ctx context.Context, ref, value string) (string, error) {
	refJSON, _ := json.Marshal(ref)
	valueJSON, _ := json.Marshal(value)
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s,%s)", snapshot.SelectElementScript, refJSON, valueJSON), "", &result); err != nil {
		return "", err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "select failed"
		}
		if !strings.Contains(result.Error, "ref is not a select element") {
			return "", fmt.Errorf("select: %s", result.Error)
		}
		return b.selectCustomOption(ctx, ref, value)
	}
	return "selected " + ref, nil
}

func (b *Bridge) selectCustomOption(ctx context.Context, ref, value string) (string, error) {
	if b.elementValueMatches(ctx, ref, value) {
		return "selected " + ref + " already " + value, nil
	}
	option, err := b.findOptionCandidate(ctx, value)
	if err != nil {
		if err := b.clickRef(ctx, ref); err != nil {
			return "", fmt.Errorf("open custom select %s: %w", ref, err)
		}
		time.Sleep(observedActionSettle)
		option, err = b.findOptionCandidate(ctx, value)
		if err != nil {
			return "", err
		}
	}
	if err := b.clickRef(ctx, option.Ref); err != nil {
		return "", fmt.Errorf("select option %s: %w", option.Ref, err)
	}
	return "selected " + ref + " via option " + option.Ref, nil
}

func (b *Bridge) elementValueMatches(ctx context.Context, ref, value string) bool {
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{Limit: 0, ViewportOnly: false})
	if err != nil {
		return false
	}
	for _, el := range snap.Elements {
		if el.Ref == ref && browser.ElementMatchesOptionValue(el, value) {
			return true
		}
	}
	return false
}

func (b *Bridge) findOptionCandidate(ctx context.Context, value string) (snapshot.Element, error) {
	for _, opts := range []snapshot.SnapshotOptions{
		{Role: "option", Query: value, Limit: 100, ViewportOnly: false},
		{Role: "option", Limit: 200, ViewportOnly: false},
	} {
		snap, err := b.Snapshot(ctx, opts)
		if err != nil {
			return snapshot.Element{}, err
		}
		if option, ok := browser.SelectOptionCandidate(snap.Elements, value); ok {
			return option, nil
		}
	}
	return snapshot.Element{}, fmt.Errorf("no visible option found for %q", value)
}

func (b *Bridge) Press(ctx context.Context, key string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	if err := b.pressKey(ctx, key); err != nil {
		return browser.ActionResult{}, err
	}
	time.Sleep(observedActionSettle)
	return b.observeActionWithBefore(ctx, "pressed "+key, before), nil
}

func (b *Bridge) pressKey(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("key is required")
	}
	desc := actions.DescribeKey(key)
	if desc.Key == "" {
		return errors.New("key is required")
	}
	for _, typ := range []string{"keyDown", "keyUp"} {
		params := map[string]any{
			"type":                  typ,
			"modifiers":             desc.Modifiers,
			"key":                   desc.Key,
			"code":                  desc.Code,
			"windowsVirtualKeyCode": desc.WindowsVirtualKeyCode,
			"nativeVirtualKeyCode":  desc.WindowsVirtualKeyCode,
		}
		if typ == "keyDown" && desc.Text != "" {
			params["text"] = desc.Text
			params["unmodifiedText"] = desc.Text
		}
		if _, err := b.cdp(ctx, "", "Input.dispatchKeyEvent", params); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bridge) Scroll(ctx context.Context, direction string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	message, err := b.scrollDirection(ctx, direction)
	if err != nil {
		return browser.ActionResult{}, err
	}
	time.Sleep(observedActionSettle)
	return b.observeActionWithBefore(ctx, message, before), nil
}

// Navigate moves through the active tab's session history (back/forward) or
// reloads the current document via the in-page History/Location web APIs, then
// returns a post-navigation observation. Standards-only: history.back(),
// history.forward(), location.reload().
func (b *Bridge) Navigate(ctx context.Context, direction string) (browser.ActionResult, error) {
	dir, err := normalizeNavigateDirection(direction)
	if err != nil {
		return browser.ActionResult{}, err
	}
	before := b.captureSemanticState(ctx)
	if err := b.navigateDirection(ctx, dir); err != nil {
		return browser.ActionResult{}, err
	}
	// A history move / reload may tear down and rebuild the document; give it a
	// moment to settle, then wait for readiness before observing.
	time.Sleep(observedActionSettle)
	_ = b.WaitFor(ctx, "load", 10*time.Second)
	return b.observeActionWithBefore(ctx, "navigated "+dir, before), nil
}

func (b *Bridge) navigateDirection(ctx context.Context, dir string) error {
	var expr string
	switch dir {
	case navigateBack:
		expr = "(function(){history.back();return true;})()"
	case navigateForward:
		expr = "(function(){history.forward();return true;})()"
	case navigateReload:
		expr = "(function(){location.reload();return true;})()"
	default:
		return fmt.Errorf("direction must be one of back, forward, reload; got %q", dir)
	}
	var ok bool
	if err := b.evaluate(ctx, expr, "", &ok); err != nil {
		// A reload/history move can destroy the execution context mid-evaluate;
		// that is the expected outcome of navigation, not a failure.
		if isNavigationTeardownError(err) {
			return nil
		}
		return err
	}
	return nil
}

const (
	navigateBack    = "back"
	navigateForward = "forward"
	navigateReload  = "reload"
)

func normalizeNavigateDirection(direction string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(direction))
	switch d {
	case navigateBack, navigateForward, navigateReload:
		return d, nil
	default:
		return "", fmt.Errorf("direction must be one of back, forward, reload; got %q", direction)
	}
}

func isNavigationTeardownError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "execution context was destroyed") ||
		strings.Contains(msg, "cannot find context with specified id") ||
		strings.Contains(msg, "frame was detached") ||
		strings.Contains(msg, "no by-value result")
}

func (b *Bridge) scrollDirection(ctx context.Context, direction string) (string, error) {
	direction = strings.ToLower(strings.TrimSpace(direction))
	if direction == "" {
		direction = "down"
	}
	directionJSON, _ := json.Marshal(direction)
	var scroll snapshot.ScrollResult
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.ScrollPageScript, directionJSON), "", &scroll); err != nil {
		return "", err
	}
	if !scroll.OK {
		if scroll.Error == "" {
			scroll.Error = "scroll failed"
		}
		return "", fmt.Errorf("scroll: %s", scroll.Error)
	}
	message := fmt.Sprintf("scrolled %s target:%s", direction, scroll.Target)
	if scroll.Name != "" {
		message += " " + strconv.Quote(scroll.Name)
	}
	return message, nil
}

func (b *Bridge) ExecutePlan(ctx context.Context, steps []browser.PlanStep) (browser.PlanResult, error) {
	entry, release := b.cancels.register(ctx, cancelToken(ctx, ""))
	defer release()
	ctx = entry.ctx

	result := browser.PlanResult{OK: true, Steps: make([]browser.PlanStepResult, 0, len(steps))}
	for i, step := range steps {
		if entry.Cancelled() {
			result.Cancelled = true
			result.OK = false
			result.Error = "cancelled"
			result.StepsCompleted = len(result.Steps)
			return result, nil
		}
		stepResult := b.executePlanStep(ctx, i, step)
		result.Steps = append(result.Steps, stepResult)
		if !stepResult.OK {
			if entry.Cancelled() {
				result.Cancelled = true
				result.OK = false
				result.Error = "cancelled"
				result.StepsCompleted = i
				return result, nil
			}
			result.OK = false
			failedAt := i
			result.FailedAt = &failedAt
			result.Error = stepResult.Error
			result.StepsCompleted = i
			return result, nil
		}
	}
	result.StepsCompleted = len(result.Steps)
	return result, nil
}

func (b *Bridge) executePlanStep(ctx context.Context, index int, step browser.PlanStep) browser.PlanStepResult {
	sr := browser.PlanStepResult{Index: index, Action: step.Action, OK: true}

	if step.ExpectRef != "" {
		findResult, err := b.Find(ctx, snapshot.FindOptions{Query: step.ExpectRef, Limit: 1})
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
		actionErr = b.clickRef(ctx, step.Ref)
		time.Sleep(batchActionSettle)
	case "type":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("type requires ref and text")
			break
		}
		actionErr = b.typeRef(ctx, step.Ref, step.Text)
		time.Sleep(batchActionSettle)
	case "fill":
		_, actionErr = b.fillOptions(ctx, snapshot.FillOptions{Ref: step.Ref, Text: step.Text, Replace: true})
		time.Sleep(batchActionSettle)
	case "select":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("select requires ref and value")
			break
		}
		_, actionErr = b.selectValue(ctx, step.Ref, step.Value)
		time.Sleep(batchActionSettle)
	case "press":
		if step.Key == "" {
			actionErr = errors.New("press requires key")
			break
		}
		actionErr = b.pressKey(ctx, step.Key)
		time.Sleep(batchActionSettle)
	case "scroll":
		_, actionErr = b.scrollDirection(ctx, step.Direction)
		time.Sleep(batchActionSettle)
	case "hover":
		if step.Ref == "" {
			actionErr = errors.New("hover requires ref")
			break
		}
		actionErr = b.hoverRef(ctx, step.Ref)
		time.Sleep(batchActionSettle)
	case "wait":
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = b.timeout
		}
		actionErr = b.WaitFor(ctx, step.Condition, timeout)
	case "snapshot":
		snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
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
		_, actionErr = b.Open(ctx, step.URL)
	case "focus_tab":
		if step.ID == "" {
			actionErr = errors.New("focus_tab requires id")
			break
		}
		actionErr = b.FocusTab(ctx, step.ID)
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

func (b *Bridge) WaitFor(ctx context.Context, condition string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = b.timeout
	}
	deadline := time.Now().Add(timeout)
	for {
		// Cooperative cancellation: a Cancel on the surrounding plan/batch (or
		// this tab) cancels ctx, unblocking a long wait promptly.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for %q cancelled", condition)
		}
		ok, err := b.condition(ctx, condition)
		if err == nil && ok {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("timed out waiting for %q", condition)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %q cancelled", condition)
		case <-time.After(waitForPollInterval):
		}
	}
}

func (b *Bridge) Screenshot(ctx context.Context) (browser.Screenshot, error) {
	raw, err := b.cdp(ctx, "", "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return browser.Screenshot{}, err
	}
	return screenshotFromRaw(raw)
}

func (b *Bridge) ScreenshotElement(ctx context.Context, ref string) (browser.Screenshot, error) {
	box, err := b.resolveBox(ctx, ref)
	if err != nil {
		return browser.Screenshot{}, err
	}
	raw, err := b.cdp(ctx, "", "Page.captureScreenshot", map[string]any{
		"format": "png",
		"clip": map[string]any{
			"x":      box.X,
			"y":      box.Y,
			"width":  box.Width,
			"height": box.Height,
			"scale":  1,
		},
	})
	if err != nil {
		return browser.Screenshot{}, err
	}
	return screenshotFromRaw(raw)
}

// ScreenshotAnnotated draws a Set-of-Marks overlay (ref-labelled boxes over the
// in-viewport frontier elements), captures the page, removes the overlay, and
// returns the PNG plus a ref->box legend. It mirrors the direct-CDP manager path
// but runs the overlay JS over the bridge's own Runtime.evaluate channel. The
// overlay is removed in every path so the page the agent acts on is unmutated.
func (b *Bridge) ScreenshotAnnotated(ctx context.Context, mode string) (browser.AnnotatedScreenshot, error) {
	if strings.TrimSpace(mode) == "" {
		mode = snapshot.DefaultSnapshotMode
	}
	opts := snapshot.NormalizeOptions(snapshot.SnapshotOptions{Mode: mode})
	snap, err := b.Snapshot(ctx, opts)
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}

	tabID := b.contextTabID(ctx)
	marks := make([]snapshot.AnnotationMark, 0, len(snap.Elements))
	meta := make(map[string]snapshot.Element, len(snap.Elements))
	for _, el := range snap.Elements {
		if !el.InViewport {
			continue
		}
		marks = append(marks, snapshot.AnnotationMark{Ref: el.Ref, Name: el.Name, Role: el.Role})
		meta[el.Ref] = el
	}

	injectExpr, err := snapshot.InjectAnnotationOverlayExpr(marks)
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}
	var overlay snapshot.AnnotationOverlayResult
	err = b.evaluate(ctx, injectExpr, tabID, &overlay)
	// Always remove the overlay, even when injection errored partway.
	defer func() {
		var discard json.RawMessage
		_ = b.evaluate(ctx, snapshot.RemoveAnnotationOverlayExpr(), tabID, &discard)
	}()
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}

	raw, err := b.cdp(ctx, tabID, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}
	shot, err := screenshotFromRaw(raw)
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}

	legend := make(map[string]browser.LegendEntry, len(overlay.Legend))
	for _, box := range overlay.Legend {
		if !box.OK {
			continue
		}
		el := meta[box.Ref]
		legend[box.Ref] = browser.LegendEntry{
			Ref:    box.Ref,
			Name:   el.Name,
			Role:   el.Role,
			X:      box.X,
			Y:      box.Y,
			Width:  box.Width,
			Height: box.Height,
		}
	}

	return browser.AnnotatedScreenshot{
		MIMEType: shot.MIMEType,
		Data:     shot.Data,
		Base64:   shot.Base64,
		Legend:   legend,
	}, nil
}

func (b *Bridge) observeAction(ctx context.Context, message string) browser.ActionResult {
	return b.observeActionWithBefore(ctx, message, nil)
}

func (b *Bridge) observeActionWithBefore(ctx context.Context, message string, before *browser.SemanticState) browser.ActionResult {
	result := browser.ActionResult{OK: true, Message: message, TabID: b.contextTabID(ctx)}
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		result.OK = false
		result.Message = message + "; observation failed: " + err.Error()
		return result
	}
	result.URL = snap.URL
	result.Title = snap.Title
	if snap.Metadata != nil {
		result.Version = browser.MetadataInt64(snap.Metadata["version"])
		if focus, ok := snap.Metadata["focused_ref"].(string); ok {
			result.Focus = focus
		}
	}
	after := browser.NewSemanticState(snap)
	browser.ApplyStateDiff(&result, before, after)
	frontier := browser.SelectFrontierElements(snap.Elements, result.Focus, 12)
	result.Elements = frontier
	result.Changed = browser.SummarizeElements(frontier, 12)
	if tabs, err := b.ListTabs(ctx); err == nil {
		result.Targets = actionTargets(tabs, b.activeTabID(), 8)
	}
	return result
}

func (b *Bridge) captureSemanticState(ctx context.Context) *browser.SemanticState {
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		return nil
	}
	state := browser.NewSemanticState(snap)
	return &state
}

func (b *Bridge) evaluate(ctx context.Context, expression, tabID string, dst any) error {
	raw, err := b.cdp(ctx, tabID, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return err
	}
	var payload struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails any `json:"exceptionDetails,omitempty"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.ExceptionDetails != nil {
		details, _ := json.Marshal(payload.ExceptionDetails)
		return fmt.Errorf("runtime exception: %s", details)
	}
	if len(payload.Result.Value) == 0 {
		// Void/undefined results (e.g. location.reload(), assignments, calls that
		// return nothing) are a successful evaluation, not an error. Surface them
		// as JSON null rather than failing the whole tool call.
		return json.Unmarshal([]byte("null"), dst)
	}
	return json.Unmarshal(payload.Result.Value, dst)
}

func (b *Bridge) cdp(ctx context.Context, tabID, method string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	req := map[string]any{"method": method, "params": params}
	if strings.TrimSpace(tabID) == "" {
		tabID = b.contextTabID(ctx)
	}
	if tabID != "" {
		req["tabId"] = parseTabID(tabID)
	}
	raw, err := b.call(ctx, "cdp", req)
	if err != nil && tabID != "" && isBridgeTabLostError(err) {
		b.setActiveTabID("")
		delete(req, "tabId")
		return b.call(ctx, "cdp", req)
	}
	if err != nil && tabID != "" && isBridgeDebuggerDetachedError(err) {
		retryRaw, retryErr := b.call(ctx, "cdp", req)
		if retryErr == nil {
			return retryRaw, nil
		}
	}
	return raw, err
}

// contextTabID resolves the tab a page action targets.
//
// Latency profile: when no explicit tab_id is in the context, this makes one
// synchronous get_active_tab_id RPC to the extension per call (every Snapshot,
// Read, Click, etc.). That is a deliberate correctness-over-latency trade: the
// cached b.active reference drifts when the user switches tabs manually in
// Chrome, and acting on the wrong tab is worse than a sub-millisecond local-WS
// round-trip. Callers issuing rapid-fire actions should pass an explicit tab_id
// (which skips the query entirely) or use browser_batch / browser_plan, which
// resolve the tab once for the whole sequence.
func (b *Bridge) contextTabID(ctx context.Context) string {
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		return tabID
	}
	// No explicit tab in context: resolve the browser's genuinely focused tab
	// from the extension rather than trusting the cached b.active reference,
	// which drifts when the user switches tabs manually in Chrome (the daemon
	// only updates b.active on explicit FocusTab/ListTabs/Open). Falls back to
	// the cached value if the bridge isn't connected or the query fails, so
	// behaviour never regresses below the previous cached-only path.
	if live := b.resolveActiveTabID(ctx); live != "" {
		return live
	}
	return b.activeTabID()
}

// resolveActiveTabID asks the extension for the truly active/focused tab and
// updates the cached reference to match, healing drift. Returns "" when the
// bridge is disconnected or the query fails so the caller can fall back to the
// last-known cached value.
func (b *Bridge) resolveActiveTabID(ctx context.Context) string {
	b.mu.RLock()
	connected := b.conn != nil
	b.mu.RUnlock()
	if !connected {
		return ""
	}
	raw, err := b.call(ctx, "get_active_tab_id", nil)
	if err != nil {
		return ""
	}
	var resp struct {
		TabID int `json:"tabId"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.TabID == 0 {
		return ""
	}
	id := strconv.Itoa(resp.TabID)
	if id != b.activeTabID() {
		b.setActiveTabID(id)
	}
	return id
}

func isBridgeTabLostError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "no tab")
}

func isBridgeDebuggerDetachedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "detached while handling command") ||
		strings.Contains(msg, "debugger is not attached") ||
		strings.Contains(msg, "target closed")
}

func (b *Bridge) axNodes(ctx context.Context, tabID string) ([]*accessibility.Node, error) {
	raw, err := b.cdp(ctx, tabID, "Accessibility.getFullAXTree", nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Nodes []*accessibility.Node `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload.Nodes, nil
}

func (b *Bridge) resolveBox(ctx context.Context, ref string) (snapshot.ElementBox, error) {
	refJSON, _ := json.Marshal(ref)
	var box snapshot.ElementBox
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.ResolveBoxScript, refJSON), "", &box); err != nil {
		return box, err
	}
	if !box.OK {
		return box, fmt.Errorf("element ref %q not found or not visible", ref)
	}
	return box, nil
}

func (b *Bridge) focus(ctx context.Context, ref string) error {
	refJSON, _ := json.Marshal(ref)
	var ok bool
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.FocusElementScript, refJSON), "", &ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("element ref %q not found or could not be focused", ref)
	}
	return nil
}

func (b *Bridge) condition(ctx context.Context, condition string) (bool, error) {
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
	  // "committed" contract: BOTH the document must be interactive/complete AND the
	  // URL must be a real navigation target — not the transient "about:blank" nor the
	  // empty href that can appear during very early frame init. The && short-circuits,
	  // so order is immaterial: an empty/blank href fails the condition regardless of
	  // readyState.
	  if (condition === "committed") return (document.readyState === "complete" || document.readyState === "interactive") && location.href !== "about:blank" && location.href !== "";
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
	err := b.evaluate(ctx, expr, "", &ok)
	return ok, err
}

type extTab struct {
	ID            int    `json:"id"`
	URL           string `json:"url"`
	Title         string `json:"title"`
	Active        bool   `json:"active"`
	Highlighted   bool   `json:"highlighted"`
	WindowID      int    `json:"windowId"`
	WindowFocused bool   `json:"windowFocused"`
	WindowType    string `json:"windowType"`
	OpenerTabID   int    `json:"openerTabId"`
}

func (t extTab) toBrowserTab() browser.Tab {
	windowType := strings.TrimSpace(t.WindowType)
	tabType := "page"
	if windowType == "popup" {
		tabType = "popup"
	}
	openerID := ""
	if t.OpenerTabID != 0 {
		openerID = strconv.Itoa(t.OpenerTabID)
	}
	return browser.Tab{
		ID:            strconv.Itoa(t.ID),
		URL:           t.URL,
		Title:         t.Title,
		Type:          tabType,
		WindowID:      t.WindowID,
		WindowType:    windowType,
		Active:        t.Active,
		Highlighted:   t.Highlighted,
		WindowFocused: t.WindowFocused,
		OpenerTabID:   openerID,
		Popup:         windowType == "popup" || t.OpenerTabID != 0,
	}
}

func (b *Bridge) activeTabID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.active
}

func (b *Bridge) setActiveTabID(id string) {
	b.mu.Lock()
	b.active = strings.TrimSpace(id)
	b.mu.Unlock()
}

func actionTargets(tabs []browser.Tab, activeID string, limit int) []browser.Tab {
	if limit <= 0 || len(tabs) == 0 {
		return nil
	}
	out := make([]browser.Tab, 0, min(limit, len(tabs)))
	seen := map[string]bool{}
	add := func(tab browser.Tab) {
		if tab.ID == "" || seen[tab.ID] || len(out) >= limit {
			return
		}
		seen[tab.ID] = true
		out = append(out, tab)
	}
	for _, tab := range tabs {
		if tab.ID == activeID {
			add(tab)
		}
	}
	for _, tab := range tabs {
		if tab.Popup || tab.WindowType == "popup" {
			add(tab)
		}
	}
	for _, tab := range tabs {
		if tab.Active && tab.WindowFocused {
			add(tab)
		}
	}
	for _, tab := range tabs {
		if tab.Active {
			add(tab)
		}
	}
	return out
}

func parseTabID(id string) int {
	n, _ := strconv.Atoi(id)
	return n
}

// requireTabID validates a caller-supplied tab id for operations that target a
// specific tab (focus/close). An empty or non-numeric id used to be silently
// coerced to 0 by parseTabID, which the extension rejected with the opaque "No
// tab with id: 0" — surfacing here as a clear, actionable error instead so a
// batched script fails loudly at the offending step.
func requireTabID(id string) (int, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return 0, errors.New("tab id is required")
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid tab id %q", id)
	}
	return n, nil
}

func screenshotFromRaw(raw json.RawMessage) (browser.Screenshot, error) {
	var payload struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return browser.Screenshot{}, err
	}
	if payload.Data == "" {
		return browser.Screenshot{}, errors.New("screenshot returned no data")
	}
	data, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil {
		return browser.Screenshot{}, err
	}
	return browser.Screenshot{MIMEType: "image/png", Data: data, Base64: payload.Data}, nil
}

func (b *Bridge) ExecuteBatch(ctx context.Context, steps []browser.BatchStep) (browser.BatchResult, error) {
	// Keep the caller context for the final observation so a cancelled run can
	// still report current page state; step execution uses the cancel-aware ctx.
	obsCtx := ctx
	entry, release := b.cancels.register(ctx, cancelToken(ctx, ""))
	defer release()
	ctx = entry.ctx

	result := browser.BatchResult{OK: true, Steps: make([]browser.BatchStepResult, 0, len(steps)), TabID: b.contextTabID(ctx)}
	for i, step := range steps {
		if entry.Cancelled() {
			result.Cancelled = true
			result.OK = false
			result.Error = "cancelled"
			break
		}
		sr := b.executeBatchStep(ctx, i, step)
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
	}
	result.StepsCompleted = len(result.Steps)
	snap, snapErr := b.Snapshot(obsCtx, snapshot.SnapshotOptions{ViewportOnly: true})
	if snapErr == nil {
		result.URL = snap.URL
		result.Title = snap.Title
		if snap.Metadata != nil {
			if v, ok := snap.Metadata["version"].(float64); ok {
				result.Version = int64(v)
			}
			if focus, ok := snap.Metadata["focused_ref"].(string); ok {
				result.Focus = focus
			}
		}
	}
	return result, nil
}

func (b *Bridge) executeBatchStep(ctx context.Context, index int, step browser.BatchStep) browser.BatchStepResult {
	sr := browser.BatchStepResult{Index: index, Action: step.Action, OK: true}
	var actionErr error
	switch step.Action {
	case "click":
		if step.Ref == "" {
			actionErr = errors.New("click requires ref")
			break
		}
		actionErr = b.clickRef(ctx, step.Ref)
		time.Sleep(batchActionSettle)
	case "type":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("type requires ref and text")
			break
		}
		actionErr = b.typeRef(ctx, step.Ref, step.Text)
		time.Sleep(batchActionSettle)
	case "fill":
		_, actionErr = b.fillOptions(ctx, snapshot.FillOptions{Ref: step.Ref, Text: step.Text, Replace: true})
		time.Sleep(batchActionSettle)
	case "select":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("select requires ref and value")
			break
		}
		_, actionErr = b.selectValue(ctx, step.Ref, step.Value)
		time.Sleep(batchActionSettle)
	case "press":
		if step.Key == "" {
			actionErr = errors.New("press requires key")
			break
		}
		actionErr = b.pressKey(ctx, step.Key)
		time.Sleep(batchActionSettle)
	case "scroll":
		_, actionErr = b.scrollDirection(ctx, step.Direction)
		time.Sleep(batchActionSettle)
	case "hover":
		if step.Ref == "" {
			actionErr = errors.New("hover requires ref")
			break
		}
		actionErr = b.hoverRef(ctx, step.Ref)
		time.Sleep(batchActionSettle)
	case "wait":
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 10 * time.Second
		}
		actionErr = b.WaitFor(ctx, step.Condition, timeout)
	case "open":
		if step.URL == "" {
			actionErr = errors.New("open requires url")
			break
		}
		_, actionErr = b.Open(ctx, step.URL)
	case "focus_tab":
		if step.ID == "" {
			actionErr = errors.New("focus_tab requires id")
			break
		}
		actionErr = b.FocusTab(ctx, step.ID)
	case "assert_visible":
		if step.Ref == "" {
			actionErr = errors.New("assert_visible requires ref")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = b.AssertVisible(ctx, step.Ref, timeout)
	case "assert_text":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("assert_text requires ref and text")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = b.AssertText(ctx, step.Ref, step.Text, timeout)
	case "assert_value":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("assert_value requires ref and value")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = b.AssertValue(ctx, step.Ref, step.Value, timeout)
	case "assert_hidden":
		if step.Ref == "" {
			actionErr = errors.New("assert_hidden requires ref")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = b.AssertHidden(ctx, step.Ref, timeout)
	default:
		actionErr = fmt.Errorf("unknown action %q", step.Action)
	}
	if actionErr != nil {
		sr.OK = false
		sr.Error = actionErr.Error()
	}
	return sr
}

func (b *Bridge) Observe(ctx context.Context) (browser.ObserveResult, error) {
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		return browser.ObserveResult{}, err
	}
	focus := ""
	if snap.Metadata != nil {
		if f, ok := snap.Metadata["focused_ref"].(string); ok {
			focus = f
		}
	}
	changed := make([]string, 0)
	for _, el := range snap.Elements {
		if el.Visible {
			summary := el.Role + " " + el.Ref + " " + el.Name
			if el.Value != "" {
				summary += " value:" + el.Value
			}
			changed = append(changed, summary)
		}
	}
	if len(changed) > 12 {
		changed = changed[:12]
	}
	return browser.ObserveResult{
		Version: 1,
		URL:     snap.URL,
		Title:   snap.Title,
		Focus:   focus,
		Changed: changed,
	}, nil
}

func (b *Bridge) AssertVisible(ctx context.Context, ref string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return b.evalAssert(ctx, snapshot.AssertVisibleScript, ref, timeout.Milliseconds())
}

func (b *Bridge) AssertText(ctx context.Context, ref, text string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return b.evalAssert(ctx, snapshot.AssertTextScript, ref, text, timeout.Milliseconds())
}

func (b *Bridge) AssertValue(ctx context.Context, ref, value string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return b.evalAssert(ctx, snapshot.AssertValueScript, ref, value, timeout.Milliseconds())
}

func (b *Bridge) AssertHidden(ctx context.Context, ref string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return b.evalAssert(ctx, snapshot.AssertHiddenScript, ref, timeout.Milliseconds())
}

func (b *Bridge) evalAssert(ctx context.Context, script string, args ...any) error {
	marshaled := make([]string, len(args))
	for i, arg := range args {
		value, _ := json.Marshal(arg)
		marshaled[i] = string(value)
	}
	var ok bool
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", script, strings.Join(marshaled, ",")), "", &ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("assertion did not pass within timeout")
	}
	return nil
}

func (b *Bridge) CommitField(ctx context.Context, ref string) error {
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	refJSON, _ := json.Marshal(ref)
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.CommitFieldScript, refJSON), "", &result); err != nil {
		return err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "commit failed"
		}
		return fmt.Errorf("commit: %s", result.Error)
	}
	return nil
}

// Notify raises a desktop notification at a human hand-off point by sending a
// "notify" command over the bridge. The extension turns it into a
// chrome.notifications.create call, which surfaces even when the agent tab is
// backgrounded. The result reports the honest delivery channel.
func (b *Bridge) Notify(ctx context.Context, opts browser.NotifyOptions) (browser.NotifyResult, error) {
	opts, err := browser.NormalizeNotifyOptions(opts)
	if err != nil {
		return browser.NotifyResult{}, err
	}
	raw, err := b.call(ctx, "notify", map[string]any{
		"kind":    opts.Kind,
		"title":   opts.Title,
		"message": opts.Message,
	})
	if err != nil {
		return browser.NotifyResult{}, err
	}
	var result browser.NotifyResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return browser.NotifyResult{}, err
	}
	if result.Delivery == "" {
		result.Delivery = "extension"
	}
	return result, nil
}

func (b *Bridge) ConsoleMessages(ctx context.Context) ([]browser.ConsoleMessage, error) {
	expr := `(function() {
		if (!window.__agentBrowserConsole) return [];
		var msgs = window.__agentBrowserConsole.slice();
		window.__agentBrowserConsole.length = 0;
		return msgs;
	})()`
	raw, err := b.call(ctx, "cdp", map[string]any{"method": "Runtime.evaluate", "params": map[string]any{"expression": expr, "returnByValue": true}})
	if err != nil {
		return nil, err
	}
	var evalResult struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &evalResult); err != nil {
		return nil, err
	}
	var msgs []browser.ConsoleMessage
	if len(evalResult.Result.Value) > 0 {
		if err := json.Unmarshal(evalResult.Result.Value, &msgs); err != nil {
			return nil, fmt.Errorf("parse console messages: %w", err)
		}
	}
	return msgs, nil
}

func (b *Bridge) ClickXY(ctx context.Context, x, y float64) (snapshot.ClickXYResult, error) {
	var result snapshot.ClickXYResult
	xJSON, _ := json.Marshal(x)
	yJSON, _ := json.Marshal(y)
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s,%s)", snapshot.ClickXYScript, xJSON, yJSON), "", &result); err != nil {
		return snapshot.ClickXYResult{}, err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "click failed"
		}
		return result, fmt.Errorf("click xy: %s", result.Error)
	}
	return result, nil
}

// Downloads is best-effort over the extension bridge. The bridge cannot
// observe Browser.downloadWillBegin / downloadProgress events without extension
// changes, so it returns an empty list plus an explanatory note rather than
// faking results. The direct-CDP Manager path provides full download tracking.
func (b *Bridge) Downloads(ctx context.Context) (browser.DownloadsResult, error) {
	return browser.DownloadsResult{
		Downloads: []browser.DownloadEntry{},
		Count:     0,
		Note:      "download tracking is not available over the extension bridge; use the direct-CDP backend for browser_downloads",
	}, nil
}

func (b *Bridge) GetTrace() browser.TraceResult { return browser.TraceResult{} }
func (b *Bridge) ClearTrace()                   {}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
