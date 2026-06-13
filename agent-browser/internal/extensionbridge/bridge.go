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
	pending map[string]chan response
	writeMu sync.Mutex
	nextID  atomic.Uint64
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
	b.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"connected": connected,
		"hello":     hello,
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
	conn.SetReadLimit(64 << 20)

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
			return nil, errors.New(resp.Error)
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
	return browser.OpenResult{Tab: tab.toBrowserTab()}, nil
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
	for _, tab := range tabs {
		out = append(out, tab.toBrowserTab())
	}
	return out, nil
}

func (b *Bridge) FocusTab(ctx context.Context, id string) error {
	_, err := b.call(ctx, "focus_tab", map[string]any{"tabId": parseTabID(id)})
	return err
}

func (b *Bridge) CloseTab(ctx context.Context, id string) error {
	_, err := b.call(ctx, "close_tab", map[string]any{"tabId": parseTabID(id)})
	return err
}

func (b *Bridge) Snapshot(ctx context.Context) (snapshot.PageSnapshot, error) {
	var snap snapshot.PageSnapshot
	if err := b.evaluate(ctx, snapshot.SnapshotScript, "", &snap); err != nil {
		return snap, err
	}
	snap.Accessibility = snapshot.AccessibilitySummary{
		Available: false,
		Error:     "accessibility tree is unavailable through the Chrome extension bridge; use direct CDP attach for AX enrichment",
	}
	return snap, nil
}

func (b *Bridge) Read(ctx context.Context) (readability.PageRead, error) {
	var read readability.PageRead
	err := b.evaluate(ctx, readability.ReadScript, "", &read)
	return read, err
}

func (b *Bridge) Click(ctx context.Context, ref string) error {
	box, err := b.resolveBox(ctx, ref)
	if err != nil {
		if fallbackErr := b.activate(ctx, ref); fallbackErr == nil {
			return nil
		}
		return err
	}
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
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
		return errors.New(result.Error)
	}
	return nil
}

func (b *Bridge) Type(ctx context.Context, ref, text string) error {
	if err := b.focus(ctx, ref); err != nil {
		return err
	}
	_, err := b.cdp(ctx, "", "Input.insertText", map[string]any{"text": text})
	return err
}

func (b *Bridge) Select(ctx context.Context, ref, value string) error {
	refJSON, _ := json.Marshal(ref)
	valueJSON, _ := json.Marshal(value)
	var ignored any
	return b.evaluate(ctx, fmt.Sprintf("%s(%s,%s)", snapshot.SelectElementScript, refJSON, valueJSON), "", &ignored)
}

func (b *Bridge) Press(ctx context.Context, key string) error {
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

func (b *Bridge) Scroll(ctx context.Context, direction string) error {
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
	return b.evaluate(ctx, fmt.Sprintf(`window.scrollBy({left:%d,top:%d,behavior:"smooth"}); true`, dx, dy), "", &ignored)
}

func (b *Bridge) WaitFor(ctx context.Context, condition string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = b.timeout
	}
	deadline := time.Now().Add(timeout)
	for {
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
		time.Sleep(250 * time.Millisecond)
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
		return errors.New("runtime evaluation returned no by-value result")
	}
	return json.Unmarshal(payload.Result.Value, dst)
}

func (b *Bridge) cdp(ctx context.Context, tabID, method string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	req := map[string]any{"method": method, "params": params}
	if tabID != "" {
		req["tabId"] = parseTabID(tabID)
	}
	return b.call(ctx, "cdp", req)
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
	ID     int    `json:"id"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Active bool   `json:"active"`
}

func (t extTab) toBrowserTab() browser.Tab {
	return browser.Tab{
		ID:    strconv.Itoa(t.ID),
		URL:   t.URL,
		Title: t.Title,
		Type:  "page",
	}
}

func parseTabID(id string) int {
	n, _ := strconv.Atoi(id)
	return n
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

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
