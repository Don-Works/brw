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

	"github.com/Don-Works/brw/internal/actions"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/accessibility"
	"github.com/coder/websocket"
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

	connectedAt      time.Time
	lastSeenAt       time.Time
	disconnectedAt   time.Time
	disconnectReason string

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
	// Bound the websocket-upgrade handshake against slow-header clients; the
	// connection is hijacked into a long-lived WS afterward, so no read/write
	// timeout that would sever the live bridge.
	b.server = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
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
	connectedAt := b.connectedAt
	lastSeenAt := b.lastSeenAt
	disconnectedAt := b.disconnectedAt
	disconnectReason := b.disconnectReason
	pending := len(b.pending)
	b.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"connected":         connected,
		"hello":             hello,
		"active_tab_id":     active,
		"connected_at":      formatStatusTime(connectedAt),
		"last_seen_at":      formatStatusTime(lastSeenAt),
		"disconnected_at":   formatStatusTime(disconnectedAt),
		"disconnect_reason": disconnectReason,
		"pending":           pending,
	})
}

// effectiveExtensionID returns the extension id whose chrome-extension:// origin
// the bridge will accept. An explicit profile bridge_extension_id always wins;
// otherwise we fall back to the published default id (profilepolicy.
// DefaultBridgeExtensionID) rather than the chrome-extension://* wildcard, so an
// unconfigured bridge still pins to the real extension instead of accepting ANY
// installed extension. Returns "" only when neither is set, which is the sole
// case that falls back to the wildcard (with a loud warning).
func (b *Bridge) effectiveExtensionID() string {
	if b.allowedExtensionID != "" {
		return b.allowedExtensionID
	}
	return strings.TrimSpace(profilepolicy.DefaultBridgeExtensionID)
}

func (b *Bridge) handleExtension(w http.ResponseWriter, r *http.Request) {
	allowedID := b.effectiveExtensionID()
	originPatterns := []string{"chrome-extension://*"}
	if allowedID != "" {
		originPatterns = []string{"chrome-extension://" + allowedID}
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: originPatterns,
	})
	if err != nil {
		log.Printf("extension websocket accept: %v", err)
		return
	}
	conn.SetReadLimit(4 << 20)

	if allowedID == "" {
		log.Printf("WARNING: extension bridge accepting connections from any Chrome extension (chrome-extension://*); set a profile policy with bridge_extension_id to restrict")
	}

	b.mu.Lock()
	if b.conn != nil {
		_ = b.conn.Close(websocket.StatusNormalClosure, "replaced by new extension connection")
	}
	now := time.Now().UTC()
	b.conn = conn
	b.hello = hello{}
	b.connectedAt = now
	b.lastSeenAt = now
	b.disconnectedAt = time.Time{}
	b.disconnectReason = ""
	b.mu.Unlock()

	log.Printf("extension bridge connected")

	// Keepalive: ping the extension periodically so a half-open link (laptop
	// sleep, NAT timeout, dropped Wi-Fi) is detected promptly instead of hanging
	// until a request times out. A failed ping closes the conn, which unblocks
	// readLoop's conn.Read and drains b.pending. The pinger exits cleanly when the
	// read loop returns (pingCancel) so it never leaks.
	pingCtx, pingCancel := context.WithCancel(r.Context())
	go b.keepAlive(pingCtx, conn, pingKeepaliveInterval)

	readErr := b.readLoop(r.Context(), conn)
	pingCancel()
	reason := "closed"
	if readErr != nil {
		reason = readErr.Error()
	}
	log.Printf("extension bridge disconnected: %s", reason)

	b.mu.Lock()
	if b.conn == conn {
		b.conn = nil
	}
	b.disconnectedAt = time.Now().UTC()
	b.disconnectReason = reason
	for id, ch := range b.pending {
		delete(b.pending, id)
		ch <- response{ID: id, Error: "extension disconnected"}
		close(ch)
	}
	b.mu.Unlock()
}

// pingKeepaliveInterval is how often the bridge pings the connected extension to
// detect a half-open link. Each ping is bounded by its own short deadline so a
// dead link is surfaced well within the interval.
const (
	pingKeepaliveInterval = 30 * time.Second
	pingTimeout           = 10 * time.Second
)

// keepAlive pings the extension every interval. A ping that fails (or times out)
// means the link is dead/half-open: the conn is closed, which unblocks readLoop
// and drains b.pending. The goroutine exits when ctx is cancelled (the read loop
// returned) — no leak. It pings only while this conn is still the bridge's
// active conn, so a replaced connection's pinger goes quiet on its next tick.
// interval is a parameter (not the const directly) so tests can drive it fast.
func (b *Bridge) keepAlive(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	if interval <= 0 {
		interval = pingKeepaliveInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.mu.RLock()
			current := b.conn == conn
			b.mu.RUnlock()
			if !current {
				return
			}
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("extension bridge keepalive ping failed: %v", err)
				_ = conn.Close(websocket.StatusGoingAway, "keepalive ping failed")
				return
			}
		}
	}
}

func (b *Bridge) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			log.Printf("extension bridge read: %v", err)
			return err
		}
		var resp response
		if err := json.Unmarshal(data, &resp); err != nil {
			log.Printf("extension bridge invalid message: %v", err)
			continue
		}
		b.mu.Lock()
		if b.conn == conn {
			b.lastSeenAt = time.Now().UTC()
		}
		b.mu.Unlock()
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

func formatStatusTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
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
// direct-CDP Open() contract so an immediate brw_evaluate / brw_read on
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

func (b *Bridge) ListTabGroups(ctx context.Context) ([]browser.TabGroup, error) {
	raw, err := b.call(ctx, "list_tab_groups", nil)
	if err != nil {
		return nil, err
	}
	var groups []extTabGroup
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, err
	}
	out := make([]browser.TabGroup, 0, len(groups))
	for _, group := range groups {
		out = append(out, group.toBrowserTabGroup())
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

func (b *Bridge) GroupTabs(ctx context.Context, tabIDs []string, opts browser.TabGroupOptions) error {
	ids := make([]int, 0, len(tabIDs))
	for _, id := range tabIDs {
		ids = append(ids, parseTabID(id))
	}
	params := map[string]any{
		"tabIds": ids,
		"name":   opts.Name,
		"color":  opts.Color,
	}
	if opts.GroupID != "" {
		groupID, err := requireGroupID(opts.GroupID)
		if err != nil {
			return err
		}
		params["groupId"] = groupID
	}
	_, err := b.call(ctx, "group_tabs", params)
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

func (b *Bridge) OpenInGroup(ctx context.Context, url string, opts browser.TabGroupOptions) (browser.OpenResult, error) {
	if strings.TrimSpace(url) == "" {
		url = "about:blank"
	}
	if !strings.Contains(url, "://") && url != "about:blank" {
		url = "https://" + url
	}
	params := map[string]any{"url": url}
	if opts.GroupID != "" {
		groupID, err := requireGroupID(opts.GroupID)
		if err != nil {
			return browser.OpenResult{}, err
		}
		params["groupId"] = groupID
	}
	if opts.Name != "" {
		params["groupName"] = opts.Name
	}
	if opts.Color != "" {
		params["groupColor"] = opts.Color
	}
	raw, err := b.call(ctx, "open_tab", params)
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
	err := b.evaluate(ctx, readability.ReadExpr(), "", &read)
	return read, err
}

func (b *Bridge) ReadData(ctx context.Context) (snapshot.StructuredData, error) {
	var data snapshot.StructuredData
	err := b.evaluate(ctx, snapshot.StructuredDataScript, "", &data)
	return data, err
}

const (
	// observedActionSettle / batchActionSettle are the CAPS for the adaptive
	// settle (b.settle): the page-change poll never blocks longer than this, so
	// settling is never slower than the previous blind time.Sleep, only faster
	// when the page stabilises early.
	observedActionSettle = 75 * time.Millisecond
	batchActionSettle    = 25 * time.Millisecond
	waitForPollInterval  = 250 * time.Millisecond
	// settlePollStart / settlePollMax bound the adaptive settle poll cadence:
	// start tight so a quiescent page returns in ~one short interval, then back
	// off so a busy page does not spin. settleStableReads is how many consecutive
	// equal fingerprints count as "settled".
	settlePollStart   = 12 * time.Millisecond
	settlePollMax     = 40 * time.Millisecond
	settleStableReads = 2
	// waitForPollStart / waitForPollMax tighten WaitFor: poll quickly at first so
	// a condition that is already (or quickly) satisfied returns promptly, backing
	// off to the original coarse interval for long waits.
	waitForPollStart = 25 * time.Millisecond
	waitForPollMax   = waitForPollInterval
	// activeTabResolveAttempts/Backoff bound how hard contextTabID retries live
	// active-tab resolution before falling back to the last-known cached tab. The
	// MV3 service worker can be mid-reconnect when a call lands; a couple of quick
	// retries ride that out so we don't act on a stale tab.
	activeTabResolveAttempts = 3
	activeTabResolveBackoff  = 150 * time.Millisecond
)

// settleFingerprintExpr is a cheap in-page snapshot of "has the page changed?"
// signals: readyState, the DOM node count, the body text length, the active
// element tag, and the current URL. It is intentionally O(1)-ish (no full DOM
// serialization) so polling it a few times is far cheaper than a real snapshot.
const settleFingerprintExpr = `(function(){try{
  var ae=document.activeElement;
  return document.readyState+'|'+(document.getElementsByTagName('*').length)+'|'+((document.body&&document.body.innerText)?document.body.innerText.length:0)+'|'+(ae?ae.tagName+'#'+(ae.id||''):'')+'|'+location.href;
}catch(e){return 'err';}})()`

// settle replaces the previous blind time.Sleep(capDur) before observing an
// action. It polls a cheap in-page fingerprint and returns as soon as the page
// is stable (two consecutive equal reads) and ready, or when capDur elapses — so
// it is NEVER slower than the old fixed sleep, only faster on a quiescent page.
// It is cancellation-aware (returns immediately if ctx is done) and degrades to
// honouring the remaining cap if the fingerprint cannot be read (disconnected /
// mid-navigation).
func (b *Bridge) settle(ctx context.Context, capDur time.Duration) {
	if capDur <= 0 {
		return
	}
	deadline := time.Now().Add(capDur)
	interval := settlePollStart
	if interval > capDur {
		interval = capDur
	}
	prev := ""
	stable := 0
	read := func() (string, bool) {
		var fp string
		if err := b.evaluate(ctx, settleFingerprintExpr, "", &fp); err != nil || fp == "" || fp == "err" {
			return "", false
		}
		return fp, true
	}
	if fp, ok := read(); ok {
		prev = fp
		stable = 1
	}
	for time.Now().Before(deadline) {
		wait := interval
		if remaining := time.Until(deadline); wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		fp, ok := read()
		if !ok {
			// Cannot read the page (navigating / disconnected): keep honouring
			// the remaining cap as a plain wait, matching the old behaviour.
			continue
		}
		if fp == prev {
			stable++
			if stable >= settleStableReads && (strings.HasPrefix(fp, "complete") || strings.HasPrefix(fp, "interactive")) {
				return
			}
		} else {
			prev = fp
			stable = 1
		}
		if interval < settlePollMax {
			interval += interval / 2 // mild geometric backoff (12,18,27,40…)
			if interval > settlePollMax {
				interval = settlePollMax
			}
		}
	}
}

func (b *Bridge) Click(ctx context.Context, ref string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	if err := b.clickRef(ctx, ref); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
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
	b.settle(ctx, observedActionSettle)
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
	b.settle(ctx, observedActionSettle)
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
	    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
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
	b.settle(ctx, observedActionSettle)
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
	b.settle(ctx, observedActionSettle)
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
	// Resolve the upload source (local path(s), inline bytes_base64, or remote
	// URL). bytes/url sources are materialized to temp files on the daemon host
	// and removed once DOM.setFileInputFiles has consumed them.
	paths, cleanup, err := browser.ResolveUploadPaths(ctx, opts)
	if err != nil {
		return browser.ActionResult{}, err
	}
	defer cleanup()

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
		"objectGroup":   "brw-upload",
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
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, "uploaded file to "+ref, before), nil
}

func (b *Bridge) Select(ctx context.Context, ref, value string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	message, err := b.selectValue(ctx, ref, value)
	if err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
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
		b.settle(ctx, observedActionSettle)
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
	b.settle(ctx, observedActionSettle)
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
	b.settle(ctx, observedActionSettle)
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
	b.settle(ctx, observedActionSettle)
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

	// Resolve the active tab once for the whole plan and pin it into the step
	// context (re-pinned after focus_tab / open steps that move focus) so each
	// step's contextTabID() short-circuits instead of re-resolving per step.
	stepCtx := b.pinActiveTab(ctx)

	result := browser.PlanResult{OK: true, Steps: make([]browser.PlanStepResult, 0, len(steps))}
	for i, step := range steps {
		if entry.Cancelled() {
			result.Cancelled = true
			result.OK = false
			result.Error = "cancelled"
			result.StepsCompleted = len(result.Steps)
			return result, nil
		}
		stepResult := b.executePlanStep(stepCtx, i, step)
		stepCtx = b.retargetPinnedTab(ctx, stepCtx, step.Action)
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
		b.settle(ctx, batchActionSettle)
	case "type":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("type requires ref and text")
			break
		}
		actionErr = b.typeRef(ctx, step.Ref, step.Text)
		b.settle(ctx, batchActionSettle)
	case "fill":
		_, actionErr = b.fillOptions(ctx, snapshot.FillOptions{Ref: step.Ref, Text: step.Text, Replace: true})
		b.settle(ctx, batchActionSettle)
	case "select":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("select requires ref and value")
			break
		}
		_, actionErr = b.selectValue(ctx, step.Ref, step.Value)
		b.settle(ctx, batchActionSettle)
	case "press":
		if step.Key == "" {
			actionErr = errors.New("press requires key")
			break
		}
		actionErr = b.pressKey(ctx, step.Key)
		b.settle(ctx, batchActionSettle)
	case "scroll":
		_, actionErr = b.scrollDirection(ctx, step.Direction)
		b.settle(ctx, batchActionSettle)
	case "hover":
		if step.Ref == "" {
			actionErr = errors.New("hover requires ref")
			break
		}
		actionErr = b.hoverRef(ctx, step.Ref)
		b.settle(ctx, batchActionSettle)
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
	// Start with a tight poll so a quickly-satisfied condition returns promptly,
	// then back off geometrically to the original coarse interval for long waits.
	interval := waitForPollStart
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
		wait := interval
		if remaining := time.Until(deadline); wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %q cancelled", condition)
		case <-time.After(wait):
		}
		if interval < waitForPollMax {
			interval += interval / 2 // 25,37,56,84,126,189,250…
			if interval > waitForPollMax {
				interval = waitForPollMax
			}
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
func (b *Bridge) ScreenshotAnnotated(ctx context.Context, aopts browser.AnnotatedScreenshotOptions) (browser.AnnotatedScreenshot, error) {
	mode := aopts.Mode
	if strings.TrimSpace(mode) == "" {
		mode = snapshot.DefaultSnapshotMode
	}
	opts := snapshot.NormalizeOptions(snapshot.SnapshotOptions{Mode: mode})
	snap, err := b.Snapshot(ctx, opts)
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}

	tabID := b.contextTabID(ctx)

	// Resolve the optional crop clip (ref -> element box, or explicit region),
	// clamped to the viewport, in top-level viewport space — the same space the
	// overlay labels are painted at. nil means a full-viewport capture.
	clip, clipErr := b.resolveAnnotationClip(ctx, tabID, aopts)
	if clipErr != nil {
		return browser.AnnotatedScreenshot{}, clipErr
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

	capParams := map[string]any{"format": "png"}
	if clip != nil {
		capParams["clip"] = map[string]any{
			"x": clip.X, "y": clip.Y, "width": clip.Width, "height": clip.Height, "scale": 1,
		}
	}
	raw, err := b.cdp(ctx, tabID, "Page.captureScreenshot", capParams)
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
		if clip != nil && !annotationBoxIntersects(box, clip) {
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

// annotationClipMargin pads a ref-derived crop so the label badge and border are
// not sliced off the edge of the crop.
const annotationClipMargin = 18.0

// annotationClip is the bridge's resolved viewport clip for an annotated crop.
type annotationClip struct {
	X, Y, Width, Height float64
}

// resolveAnnotationClip turns the requested ref/region into a viewport clip,
// clamped to the page viewport. Returns nil for a full-viewport capture. Box and
// viewport resolution run over the bridge's own evaluate channel.
func (b *Bridge) resolveAnnotationClip(ctx context.Context, tabID string, aopts browser.AnnotatedScreenshotOptions) (*annotationClip, error) {
	var x, y, w, h float64
	switch {
	case strings.TrimSpace(aopts.Ref) != "":
		refJSON, _ := json.Marshal(aopts.Ref)
		expr := fmt.Sprintf("%s(%s)", snapshot.ResolveBoxScript, refJSON)
		var box snapshot.ElementBox
		if err := b.evaluate(ctx, expr, tabID, &box); err != nil {
			return nil, err
		}
		if !box.OK {
			return nil, fmt.Errorf("element ref %q not found or not visible", aopts.Ref)
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
	var dims struct {
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	_ = b.evaluate(ctx, `({w: window.innerWidth||document.documentElement.clientWidth||0, h: window.innerHeight||document.documentElement.clientHeight||0})`, tabID, &dims)
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if dims.W > 0 && x+w > dims.W {
		w = dims.W - x
	}
	if dims.H > 0 && y+h > dims.H {
		h = dims.H - y
	}
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("screenshot clip resolves to an empty region")
	}
	return &annotationClip{X: x, Y: y, Width: w, Height: h}, nil
}

// annotationBoxIntersects reports whether an overlay box overlaps the clip
// rectangle (both in top-level viewport space), used to prune the legend.
func annotationBoxIntersects(box snapshot.AnnotationBox, clip *annotationClip) bool {
	return box.X < clip.X+clip.Width && box.X+box.Width > clip.X &&
		box.Y < clip.Y+clip.Height && box.Y+box.Height > clip.Y
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
// (which skips the query entirely) or use brw_batch / brw_plan, which
// resolve the tab once for the whole sequence.
func (b *Bridge) contextTabID(ctx context.Context) string {
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		return tabID
	}
	// No explicit tab in context: resolve the browser's genuinely focused tab
	// from the extension rather than trusting the cached b.active reference,
	// which drifts when the user switches tabs manually in Chrome (the daemon
	// only updates b.active on explicit FocusTab/ListTabs/Open).
	//
	// Retry briefly before trusting the cache. The MV3 service worker sleeps when
	// Chrome is idle and may be mid-reconnect when a tool call arrives; a single
	// transient resolution failure must NOT silently drop us onto a stale cached
	// tab, because that is exactly how read/observe/snapshot end up resolving three
	// different tabs (one re-resolves live, another serves the stale cache). Three
	// quick attempts ride out a reconnect hiccup; only a genuinely unreachable
	// extension falls through to the last-known tab.
	for attempt := 0; attempt < activeTabResolveAttempts; attempt++ {
		if live := b.resolveActiveTabID(ctx); live != "" {
			return live
		}
		if attempt < activeTabResolveAttempts-1 {
			select {
			case <-ctx.Done():
				return b.activeTabID()
			case <-time.After(activeTabResolveBackoff):
			}
		}
	}
	return b.activeTabID()
}

// ResolveActiveTabID resolves the genuinely focused tab ONCE for a top-level
// tool call and returns it (or "" when it cannot be determined). The MCP / HTTP
// entry points call this when no explicit tab_id is supplied and pin the result
// into the request context via browser.WithTabID, so every downstream
// contextTabID() short-circuits instead of re-issuing get_active_tab_id 3-11x
// per logical tool call. It runs the same bounded retry as contextTabID so a
// mid-reconnect MV3 service worker does not drop the call onto a stale tab.
func (b *Bridge) ResolveActiveTabID(ctx context.Context) string {
	return b.contextTabID(ctx)
}

// pinActiveTab resolves the active tab once and returns a context with that tab
// pinned via browser.WithTabID. If an explicit tab is already in the context it
// is left untouched (the caller asked for a specific tab). If resolution fails
// (extension disconnected, mid-reconnect) the original context is returned so
// downstream calls keep their existing per-call resolution behaviour rather than
// pinning an empty tab.
func (b *Bridge) pinActiveTab(ctx context.Context) context.Context {
	if browser.TabIDFromContext(ctx) != "" {
		return ctx
	}
	if tabID := b.contextTabID(ctx); tabID != "" {
		return browser.WithTabID(ctx, tabID)
	}
	return ctx
}

// retargetPinnedTab re-pins the active tab after a step that legitimately moves
// the browser's focus (focus_tab, open). Without this, a focus_tab / open step
// mid-batch/plan would leave subsequent steps pinned to the STALE pre-focus tab
// for the rest of the sequence. base is the caller's context: for an
// auto-resolved sequence it carries NO tab (the MCP/HTTP entry excludes
// batch/plan from one-shot pinning), but for a caller-supplied tab_id it carries
// that explicit tab. stepCtx is the currently-pinned context, returned unchanged
// for non-retargeting steps so we never pay an extra resolution round trip. The
// bridge updates b.active inside FocusTab/Open, so re-resolving from base picks
// up the new focused tab.
func (b *Bridge) retargetPinnedTab(base, stepCtx context.Context, action string) context.Context {
	if action != "focus_tab" && action != "open" {
		return stepCtx
	}
	// An explicitly-supplied tab_id stays sticky for the whole sequence (matching
	// the pre-pin behaviour where contextTabID short-circuits on the caller's tab
	// regardless of focus_tab side effects): never let a focus_tab/open step
	// retarget it. base carries a tab ONLY when the caller passed one explicitly.
	if browser.TabIDFromContext(base) != "" {
		return stepCtx
	}
	if newTab := b.activeTabID(); newTab != "" {
		return browser.WithTabID(base, newTab)
	}
	return base
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
	    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
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
	ID             int    `json:"id"`
	URL            string `json:"url"`
	Title          string `json:"title"`
	Active         bool   `json:"active"`
	Highlighted    bool   `json:"highlighted"`
	WindowID       int    `json:"windowId"`
	WindowFocused  bool   `json:"windowFocused"`
	WindowType     string `json:"windowType"`
	GroupID        *int   `json:"groupId"`
	GroupTitle     string `json:"groupTitle"`
	GroupColor     string `json:"groupColor"`
	GroupCollapsed bool   `json:"groupCollapsed"`
	OpenerTabID    int    `json:"openerTabId"`
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
		ID:             strconv.Itoa(t.ID),
		URL:            t.URL,
		Title:          t.Title,
		Type:           tabType,
		WindowID:       t.WindowID,
		WindowType:     windowType,
		GroupID:        groupIDString(t.GroupID),
		GroupTitle:     t.GroupTitle,
		GroupColor:     t.GroupColor,
		GroupCollapsed: t.GroupCollapsed,
		Active:         t.Active,
		Highlighted:    t.Highlighted,
		WindowFocused:  t.WindowFocused,
		OpenerTabID:    openerID,
		Popup:          windowType == "popup" || t.OpenerTabID != 0,
	}
}

type extTabGroup struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Color     string `json:"color"`
	Collapsed bool   `json:"collapsed"`
	WindowID  int    `json:"windowId"`
	TabIDs    []int  `json:"tabIds"`
	TabCount  int    `json:"tabCount"`
}

func (g extTabGroup) toBrowserTabGroup() browser.TabGroup {
	tabIDs := make([]string, 0, len(g.TabIDs))
	for _, id := range g.TabIDs {
		tabIDs = append(tabIDs, strconv.Itoa(id))
	}
	return browser.TabGroup{
		ID:        strconv.Itoa(g.ID),
		Title:     g.Title,
		Color:     g.Color,
		Collapsed: g.Collapsed,
		WindowID:  g.WindowID,
		TabIDs:    tabIDs,
		TabCount:  g.TabCount,
	}
}

func groupIDString(id *int) string {
	if id == nil || *id < 0 {
		return ""
	}
	return strconv.Itoa(*id)
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

func requireGroupID(id string) (int, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return 0, errors.New("group id is required")
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid group id %q", id)
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

	// Resolve the active tab ONCE for the whole sequence and pin it into the
	// step context, so each step's contextTabID() short-circuits instead of
	// re-issuing get_active_tab_id per step (3-11x per step otherwise). A
	// focus_tab / open step legitimately moves the active tab, so we re-pin after
	// any retargeting step (see retargetPinnedTab).
	stepCtx := b.pinActiveTab(ctx)

	result := browser.BatchResult{OK: true, Steps: make([]browser.BatchStepResult, 0, len(steps)), TabID: browser.TabIDFromContext(stepCtx)}
	for i, step := range steps {
		if entry.Cancelled() {
			result.Cancelled = true
			result.OK = false
			result.Error = "cancelled"
			break
		}
		sr := b.executeBatchStep(stepCtx, i, step)
		stepCtx = b.retargetPinnedTab(ctx, stepCtx, step.Action)
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
	// Pin the observation to the tab the sequence ended on (the step pin, which
	// already tracked focus_tab/open moves) so the closing snapshot resolves the
	// active tab once via the pinned id instead of re-issuing get_active_tab_id
	// across its tryCached/store/evaluate sub-calls. Falls back to obsCtx when no
	// tab could be pinned, preserving the prior per-call behaviour.
	if pinned := browser.TabIDFromContext(stepCtx); pinned != "" {
		obsCtx = browser.WithTabID(obsCtx, pinned)
	} else {
		obsCtx = b.pinActiveTab(obsCtx)
	}
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
		b.settle(ctx, batchActionSettle)
	case "type":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("type requires ref and text")
			break
		}
		actionErr = b.typeRef(ctx, step.Ref, step.Text)
		b.settle(ctx, batchActionSettle)
	case "fill":
		_, actionErr = b.fillOptions(ctx, snapshot.FillOptions{Ref: step.Ref, Text: step.Text, Replace: true})
		b.settle(ctx, batchActionSettle)
	case "select":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("select requires ref and value")
			break
		}
		_, actionErr = b.selectValue(ctx, step.Ref, step.Value)
		b.settle(ctx, batchActionSettle)
	case "press":
		if step.Key == "" {
			actionErr = errors.New("press requires key")
			break
		}
		actionErr = b.pressKey(ctx, step.Key)
		b.settle(ctx, batchActionSettle)
	case "scroll":
		_, actionErr = b.scrollDirection(ctx, step.Direction)
		b.settle(ctx, batchActionSettle)
	case "hover":
		if step.Ref == "" {
			actionErr = errors.New("hover requires ref")
			break
		}
		actionErr = b.hoverRef(ctx, step.Ref)
		b.settle(ctx, batchActionSettle)
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
		if (!window.__brwConsole) return [];
		var msgs = window.__brwConsole.slice();
		window.__brwConsole.length = 0;
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

// Downloads is best-effort over the extension bridge. The bridge cannot observe
// Browser.downloadWillBegin / downloadProgress CDP events: those only fire on a
// debugger-attached target, and the extension bridge drives the page via the
// content/background scripts rather than a CDP debugger session, so no download
// lifecycle events reach us.
//
// Real capture here would mean adding chrome.downloads support on the extension
// side: declare the "downloads" permission in the manifest, register
// chrome.downloads.onCreated / onChanged listeners in the background script,
// forward those events over the bridge as a new message kind, and surface them
// through a new bridge RPC. That is a sizable cross-language change (extension
// JS + manifest + Go bridge plumbing), so it is intentionally NOT implemented
// here — see GitHub issue #6. Instead we return an empty, explicitly-unsupported
// result with Supported=false so callers can branch on the flag (rather than
// pattern-matching the prose Note) and fall back to the direct-CDP backend,
// which provides full download tracking via the Manager path.
func (b *Bridge) Downloads(ctx context.Context) (browser.DownloadsResult, error) {
	return browser.DownloadsResult{
		Downloads: []browser.DownloadEntry{},
		Count:     0,
		Supported: false,
		Note:      "Download capture is unavailable on the extension-bridge backend (it cannot observe CDP download events). Restart brw with the direct-CDP backend to track downloads via brw_downloads, or check Supported=false to detect this case programmatically.",
	}, nil
}

func (b *Bridge) GetTrace() browser.TraceResult { return browser.TraceResult{} }
func (b *Bridge) ClearTrace()                   {}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
