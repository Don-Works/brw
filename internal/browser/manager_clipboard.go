package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// ClipboardOptions selects a clipboard operation. Action is read or write;
// Text is the payload for a write.
type ClipboardOptions struct {
	Action string `json:"action"`
	Text   string `json:"text,omitempty"`
	TabID  string `json:"tab_id,omitempty"`
}

// ClipboardResult reports what the operation did. Text is set only by a read.
type ClipboardResult struct {
	OK     bool   `json:"ok"`
	Action string `json:"action"`
	Text   string `json:"text,omitempty"`
	Length int    `json:"length"`
	Origin string `json:"origin,omitempty"`
}

// clipboardScript reads or writes the system clipboard through the async
// Clipboard API and reports the failure in-band rather than as a rejected
// promise, so the caller sees the DOMException name instead of a stack.
//
// The write falls back to a selected off-screen textarea + execCommand('copy')
// because navigator.clipboard does not exist at all on a non-secure origin
// (plain http on anything but localhost), which is exactly where an intranet
// app under test lives. There is no equivalent read fallback: Chrome removed
// execCommand('paste') from page script, so a clipboard READ on a non-secure
// origin cannot be served and says so.
const clipboardScript = `(function(action, text){
  function fail(e){ return {ok:false, error: (e && (e.name ? e.name + ': ' + e.message : e.message)) || String(e)}; }
  function legacyWrite(value){
    try {
      var area = document.createElement('textarea');
      area.value = value;
      area.setAttribute('readonly', '');
      area.style.position = 'fixed';
      area.style.top = '-1000px';
      area.style.opacity = '0';
      (document.body || document.documentElement).appendChild(area);
      area.select();
      area.setSelectionRange(0, value.length);
      var copied = document.execCommand('copy');
      area.remove();
      return copied ? {ok:true} : {ok:false, error:'execCommand("copy") was refused'};
    } catch (e) { return fail(e); }
  }
  if (action === 'write') {
    if (!navigator.clipboard || !navigator.clipboard.writeText) return Promise.resolve(legacyWrite(text));
    return navigator.clipboard.writeText(text).then(function(){ return {ok:true}; }, function(e){
      var legacy = legacyWrite(text);
      return legacy.ok ? legacy : fail(e);
    });
  }
  if (!navigator.clipboard || !navigator.clipboard.readText) {
    return Promise.resolve({ok:false, error:'navigator.clipboard is unavailable on this page: reading the clipboard needs a secure context (https, or localhost)'});
  }
  return navigator.clipboard.readText().then(function(t){ return {ok:true, text:t}; }, fail);
})`

// Clipboard reads or writes the system clipboard from the page's own origin.
//
// Chrome gates clipboard access on a user gesture AND a permission. brw cannot
// produce a real gesture, so the permission is granted over CDP for the page's
// origin (Browser.setPermission — the per-permission successor to the
// grantPermissions command, which the pinned cdproto no longer generates) and
// the evaluation is marked as user-initiated. The read is the half that needs
// it: a write on a focused tab is allowed without any grant, a read is refused
// with "Read permission denied".
//
// Direct-CDP transport only. Browser-domain commands are not reachable through
// chrome.debugger, so the extension bridge cannot grant the permission.
func (m *Manager) Clipboard(ctx context.Context, opts ClipboardOptions) (ClipboardResult, error) {
	action := strings.ToLower(strings.TrimSpace(opts.Action))
	switch action {
	case "read", "write":
	case "":
		return ClipboardResult{}, errors.New("clipboard action is required: read or write")
	default:
		return ClipboardResult{}, fmt.Errorf("unknown clipboard action %q: use read or write", opts.Action)
	}

	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ClipboardResult{}, err
	}
	defer cancel()

	var origin string
	if err := chromedp.Run(tabCtx, chromedp.Evaluate("location.origin", &origin)); err != nil {
		return ClipboardResult{}, fmt.Errorf("read page origin: %w", err)
	}
	if origin == "" || origin == "null" {
		return ClipboardResult{}, errors.New("the clipboard is scoped to a page origin and this tab has none (about:blank, a data: URL, or a sandboxed document); open a real page first")
	}
	if err := m.grantClipboardPermissions(tabCtx, origin); err != nil {
		return ClipboardResult{}, err
	}
	// The async Clipboard API refuses to serve a document that does not have
	// focus. A tab brw drives is not necessarily the frontmost one.
	_ = chromedp.Run(tabCtx, chromedp.ActionFunc(func(c context.Context) error {
		return page.BringToFront().Do(c)
	}))

	actionJSON, _ := json.Marshal(action)
	textJSON, _ := json.Marshal(opts.Text)
	expr := fmt.Sprintf("%s(%s,%s)", clipboardScript, actionJSON, textJSON)

	var out struct {
		OK    bool   `json:"ok"`
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(c context.Context) error {
		obj, exception, err := runtime.Evaluate(expr).
			WithAwaitPromise(true).
			WithReturnByValue(true).
			WithUserGesture(true).
			Do(c)
		if err != nil {
			return err
		}
		if exception != nil {
			if msg := FormatRuntimeException(exception); msg != "" {
				return fmt.Errorf("clipboard %s failed: %s", action, msg)
			}
			return fmt.Errorf("clipboard %s failed", action)
		}
		if obj == nil || len(obj.Value) == 0 {
			return fmt.Errorf("clipboard %s returned nothing", action)
		}
		return json.Unmarshal(obj.Value, &out)
	})); err != nil {
		return ClipboardResult{}, err
	}
	if !out.OK {
		if out.Error == "" {
			out.Error = "clipboard access was refused"
		}
		return ClipboardResult{}, fmt.Errorf("clipboard %s failed: %s", action, out.Error)
	}

	result := ClipboardResult{OK: true, Action: action, Origin: origin}
	if action == "read" {
		result.Text = out.Text
		result.Length = len([]rune(out.Text))
	} else {
		result.Length = len([]rune(opts.Text))
	}
	// The trace records the operation and its size but never the payload: a
	// clipboard holds whatever the user last copied, which is as likely to be a
	// password as a URL, and no caller declared it sensitive.
	m.recordTrace(tabID, TraceEntry{
		Action:     "clipboard",
		Value:      action,
		OK:         true,
		DurationMS: time.Since(start).Milliseconds(),
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}

// grantClipboardPermissions authorizes clipboard read/write for one origin. The
// override is scoped to that origin and is not cleared afterwards: the only
// removal CDP offers is Browser.resetPermissions, which drops every override in
// the context, and a read-modify-write flow would then break on its second call.
func (m *Manager) grantClipboardPermissions(tabCtx context.Context, origin string) error {
	return chromedp.Run(tabCtx, chromedp.ActionFunc(func(c context.Context) error {
		for _, name := range []string{"clipboard-read", "clipboard-write"} {
			descriptor := &cdpbrowser.PermissionDescriptor{Name: name, AllowWithoutSanitization: true}
			if err := cdpbrowser.SetPermission(descriptor, cdpbrowser.PermissionSettingGranted).
				WithOrigin(origin).
				Do(c); err != nil {
				return fmt.Errorf("grant %s for %s: %w", name, origin, err)
			}
		}
		return nil
	}))
}
