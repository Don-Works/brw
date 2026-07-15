package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// CapturedRequest is one entry recorded by the in-page network interceptor.
// Bodies and response snippets are truncated in-page to keep the ring buffer
// and the drained payload bounded.
type CapturedRequest struct {
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	RequestHeaders  map[string]string `json:"request_headers,omitempty"`
	RequestBody     string            `json:"request_body,omitempty"`
	Status          int               `json:"status"`
	OK              bool              `json:"ok"`
	ResponseSnippet string            `json:"response_snippet,omitempty"`
	Transport       string            `json:"transport,omitempty"`
	Error           string            `json:"error,omitempty"`
	StartedAt       float64           `json:"started_at"`
	DurationMS      float64           `json:"duration_ms"`
}

// ReplayResult is the outcome of re-executing a single request in-page.
type ReplayResult struct {
	Status         int    `json:"status"`
	OK             bool   `json:"ok"`
	Body           string `json:"body,omitempty"`
	BodyOffset     int    `json:"body_offset,omitempty"`
	BodyBytes      int    `json:"body_bytes,omitempty"`
	BodyTotalBytes int    `json:"body_total_bytes,omitempty"`
	BodyTruncated  bool   `json:"body_truncated,omitempty"`
	NextOffset     int    `json:"next_offset,omitempty"`
	ContentType    string `json:"content_type,omitempty"`
	URL            string `json:"url,omitempty"`
	Method         string `json:"method,omitempty"`
	Error          string `json:"error,omitempty"`
}

// NetworkCaptureInstallScript injects an idempotent in-page interceptor that
// wraps window.fetch and XMLHttpRequest, recording recent requests into a
// bounded ring buffer on window. Modelled exactly on the injected console
// interceptor (extension/service_worker.js): a window guard makes it
// re-injection safe, the wrapped originals are preserved, and the buffer is
// capped so it cannot grow without bound. Pure in-page JS, so it works on both
// the direct-CDP and extension-bridge transports (the bridge has no CDP Network
// domain, so the interceptor MUST live in the page, not in CDP).
const NetworkCaptureInstallScript = `(function() {
  var MAX = 100;
  var BODY_CAP = 2048;
  if (window.__brwNet) return { installed: true, already: true };
  var buf = [];
  window.__brwNet = buf;
  function clip(s) {
    s = (s === undefined || s === null) ? '' : String(s);
    return s.length > BODY_CAP ? s.slice(0, BODY_CAP) + '…[truncated]' : s;
  }
  function push(entry) {
    buf.push(entry);
    if (buf.length > MAX) buf.shift();
  }
  function headerObj(h) {
    var out = {};
    try {
      if (!h) return out;
      if (typeof h.forEach === 'function') { h.forEach(function(v, k){ out[k] = String(v); }); return out; }
      if (Array.isArray(h)) { h.forEach(function(p){ if (p && p.length >= 2) out[p[0]] = String(p[1]); }); return out; }
      if (typeof h === 'object') { Object.keys(h).forEach(function(k){ out[k] = String(h[k]); }); }
    } catch (e) {}
    return out;
  }
  // ---- fetch ----
  if (typeof window.fetch === 'function' && !window.fetch.__brwWrapped) {
    var origFetch = window.fetch.bind(window);
    var wrappedFetch = function(input, init) {
      init = init || {};
      var url = (typeof input === 'string') ? input : (input && input.url) || String(input);
      var method = (init.method || (input && input.method) || 'GET').toUpperCase();
      var reqHeaders = headerObj(init.headers || (input && input.headers));
      var reqBody = '';
      try { if (typeof init.body === 'string') reqBody = init.body; } catch (e) {}
      var started = (performance && performance.now) ? performance.now() : Date.now();
      var entry = {
        method: method, url: String(url), request_headers: reqHeaders, request_body: clip(reqBody),
        status: 0, ok: false, response_snippet: '', transport: 'fetch', error: '',
        started_at: Date.now(), duration_ms: 0
      };
      push(entry);
      return origFetch(input, init).then(function(resp) {
        entry.status = resp.status; entry.ok = resp.ok;
        entry.duration_ms = ((performance && performance.now) ? performance.now() : Date.now()) - started;
        try {
          resp.clone().text().then(function(t){ entry.response_snippet = clip(t); }).catch(function(){});
        } catch (e) {}
        return resp;
      }).catch(function(err) {
        entry.error = String(err && err.message || err);
        entry.duration_ms = ((performance && performance.now) ? performance.now() : Date.now()) - started;
        throw err;
      });
    };
    wrappedFetch.__brwWrapped = true;
    window.fetch = wrappedFetch;
  }
  // ---- XMLHttpRequest ----
  if (window.XMLHttpRequest && window.XMLHttpRequest.prototype && !window.XMLHttpRequest.prototype.__brwWrapped) {
    var proto = window.XMLHttpRequest.prototype;
    var origOpen = proto.open;
    var origSend = proto.send;
    var origSetHeader = proto.setRequestHeader;
    proto.open = function(method, url) {
      this.__abMethod = String(method || 'GET').toUpperCase();
      this.__abURL = String(url || '');
      this.__abHeaders = {};
      return origOpen.apply(this, arguments);
    };
    proto.setRequestHeader = function(k, v) {
      try { this.__abHeaders = this.__abHeaders || {}; this.__abHeaders[k] = String(v); } catch (e) {}
      return origSetHeader.apply(this, arguments);
    };
    proto.send = function(body) {
      var xhr = this;
      var reqBody = '';
      try { if (typeof body === 'string') reqBody = body; } catch (e) {}
      var started = (performance && performance.now) ? performance.now() : Date.now();
      var entry = {
        method: xhr.__abMethod || 'GET', url: xhr.__abURL || '', request_headers: xhr.__abHeaders || {},
        request_body: clip(reqBody), status: 0, ok: false, response_snippet: '', transport: 'xhr', error: '',
        started_at: Date.now(), duration_ms: 0
      };
      push(entry);
      xhr.addEventListener('loadend', function() {
        entry.status = xhr.status || 0;
        entry.ok = xhr.status >= 200 && xhr.status < 300;
        entry.duration_ms = ((performance && performance.now) ? performance.now() : Date.now()) - started;
        try {
          var rt = (xhr.responseType === '' || xhr.responseType === 'text') ? xhr.responseText : '';
          entry.response_snippet = clip(rt);
        } catch (e) {}
      });
      return origSend.apply(this, arguments);
    };
    proto.__brwWrapped = true;
  }
  return { installed: true, already: false };
})()`

// sensitiveHeaderNames are request-header names whose VALUE is a bare credential
// with no debugging value beyond its presence. brw_network_capture is a debugging
// view of a page's own traffic, so it must not become a side-door that hands the
// agent (or a prompt injection steering it) a live bearer token or session cookie
// that the snapshot/read redaction and the cookie CDP denylist otherwise keep out
// of reach. Compared case-insensitively.
var sensitiveHeaderNames = map[string]bool{
	"authorization":        true,
	"proxy-authorization":  true,
	"cookie":               true,
	"set-cookie":           true,
	"x-api-key":            true,
	"api-key":              true,
	"x-auth-token":         true,
	"x-amz-security-token": true,
	"x-goog-api-key":       true,
}

// RedactCapturedCredentials blanks the VALUE of any sensitive request header in
// place while keeping the header NAME, so a captured request still shows that it
// carried e.g. an Authorization header (useful for debugging) without exposing the
// credential itself. Applied by both transports before captured requests leave the
// process. The request body is intentionally left untouched — it is the payload
// the caller explicitly asked to inspect, and field-level body redaction cannot be
// done safely without a schema.
func RedactCapturedCredentials(requests []CapturedRequest) []CapturedRequest {
	const placeholder = "[redacted]"
	for i := range requests {
		h := requests[i].RequestHeaders
		if h == nil {
			continue
		}
		for name := range h {
			if sensitiveHeaderNames[strings.ToLower(strings.TrimSpace(name))] {
				h[name] = placeholder
			}
		}
	}
	return requests
}

// NetworkCaptureDrainScript returns the recorded requests and clears the ring
// buffer, mirroring the console drain pattern (slice() then length = 0).
const NetworkCaptureDrainScript = `(function() {
  if (!window.__brwNet) return [];
  var msgs = window.__brwNet.slice();
  window.__brwNet.length = 0;
  return msgs;
})()`

// ReplayRequestScript re-executes a single request in-page via fetch and
// returns a bounded, byte-windowed {status, ok, body} result. The default 64 KiB
// window is large enough for normal JSON APIs (the old hard 4 KiB clipping broke
// parsing of larger JSON lists), while offset/maxBytes allow deterministic
// paging without ever returning a silently clipped body. The purchase guard is enforced
// Go-side before this script ever runs (browser.ReplayRequestParams.
// BlockedReplayReason, called by both transports) so a refusal is an explicit
// error, never a network call.
const ReplayRequestScript = `(function(opts) {
  opts = opts || {};
  var DEFAULT_BODY_BYTES = 65536;
  var MAX_BODY_BYTES = 1048576;
  function bodyWindow(s) {
    s = (s === undefined || s === null) ? '' : String(s);
    var bytes = new TextEncoder().encode(s);
    var requestedOffset = Math.max(0, Math.floor(Number(opts.offset || 0)) || 0);
    var maxBytes = Math.floor(Number(opts.maxBytes || 0)) || DEFAULT_BODY_BYTES;
    maxBytes = Math.max(1, Math.min(MAX_BODY_BYTES, maxBytes));
    var start = Math.min(requestedOffset, bytes.length);
    // If a caller supplied an arbitrary byte offset inside a UTF-8 continuation
    // sequence, advance to the next complete code point and report the actual
    // body_offset used.
    while (start < bytes.length && (bytes[start] & 0xC0) === 0x80) start++;
    var end = Math.min(bytes.length, start + maxBytes);
    // Do not end midway through a UTF-8 code point. next_offset is this adjusted
    // boundary, so subsequent windows concatenate without replacement glyphs.
    if (end < bytes.length) {
      while (end > start && (bytes[end] & 0xC0) === 0x80) end--;
      if (end === start) {
        end = Math.min(bytes.length, start + maxBytes);
        while (end < bytes.length && (bytes[end] & 0xC0) === 0x80) end++;
      }
    }
    return {
      body: new TextDecoder().decode(bytes.slice(start, end)),
      body_offset: start,
      body_bytes: end - start,
      body_total_bytes: bytes.length,
      body_truncated: start > 0 || end < bytes.length,
      next_offset: end < bytes.length ? end : 0
    };
  }
  var url = String(opts.url || '');
  var init = { method: String(opts.method || 'GET').toUpperCase() };
  if (opts.headers && typeof opts.headers === 'object') init.headers = opts.headers;
  if (typeof opts.body === 'string' && opts.body.length && init.method !== 'GET' && init.method !== 'HEAD') init.body = opts.body;
  return fetch(url, init).then(function(resp) {
    return resp.text().then(function(t) {
      var result = bodyWindow(t);
      result.status = resp.status;
      result.ok = resp.ok;
      result.url = url;
      result.method = init.method;
      result.content_type = resp.headers.get('content-type') || '';
      result.error = '';
      return result;
    }).catch(function() {
      return { status: resp.status, ok: resp.ok, body: '', body_bytes: 0, body_total_bytes: 0, url: url, method: init.method, content_type: resp.headers.get('content-type') || '', error: '' };
    });
  }).catch(function(err) {
    return { status: 0, ok: false, body: '', url: url, method: init.method, error: String(err && err.message || err) };
  });
})`

// InstallNetworkCapture injects the interceptor (idempotently) into the page.
func InstallNetworkCapture(ctx context.Context) error {
	var ignored json.RawMessage
	return chromedp.Run(ctx, chromedp.Evaluate(NetworkCaptureInstallScript, &ignored))
}

// RegisterNetworkCaptureOnNewDocument arms the interceptor to (re)install at
// document-start on every subsequent navigation/reload via
// Page.addScriptToEvaluateOnNewDocument, so capture survives full navigations
// instead of being wiped with the page's JS context. Direct-CDP transport only
// (the extension bridge has no CDP); the install script's window guard keeps it
// idempotent even if a normal in-page install also runs. Call once per tab.
func RegisterNetworkCaptureOnNewDocument(ctx context.Context) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(NetworkCaptureInstallScript).Do(ctx)
		return err
	}))
}

// CaptureNetwork installs the interceptor (idempotent) then drains and returns
// the recorded requests, optionally filtered by a case-insensitive URL
// substring filter applied Go-side.
func CaptureNetwork(ctx context.Context) ([]CapturedRequest, error) {
	if err := InstallNetworkCapture(ctx); err != nil {
		return nil, err
	}
	var requests []CapturedRequest
	if err := chromedp.Run(ctx, chromedp.Evaluate(NetworkCaptureDrainScript, &requests)); err != nil {
		return nil, err
	}
	return requests, nil
}

// ReplayRequest re-executes the given request in-page and returns the result.
// The async fetch promise is awaited via runtime.Evaluate with WithAwaitPromise.
func ReplayRequest(ctx context.Context, method, url string, headers map[string]string, body string, offset, maxBytes int) (ReplayResult, error) {
	opts := map[string]any{"method": method, "url": url, "headers": headers, "body": body, "offset": offset, "maxBytes": maxBytes}
	args, _ := json.Marshal(opts)
	expr := fmt.Sprintf("%s(%s)", ReplayRequestScript, args)
	var result ReplayResult
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		obj, exception, err := runtime.Evaluate(expr).
			WithReturnByValue(true).
			WithAwaitPromise(true).
			Do(ctx)
		if err != nil {
			return err
		}
		if exception != nil {
			details, _ := json.Marshal(exception)
			return fmt.Errorf("replay request failed: %s", details)
		}
		if obj == nil || len(obj.Value) == 0 {
			return nil
		}
		return json.Unmarshal(obj.Value, &result)
	})); err != nil {
		return ReplayResult{}, err
	}
	return result, nil
}
