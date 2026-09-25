package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Detached WebMCP invocation.
//
// A WebMCP tool is a function inside a document, and the page decides how long
// it takes: a checkout, an export, a search across a remote index. Invoking one
// synchronously pins the agent's turn to that duration and, when the wait is cut
// short, the work is still running in the page with no handle on it. So every
// invocation is registered in the document that started it and addressed by an
// id afterwards — started, polled, cancelled — with the synchronous call being
// nothing more than "start it and poll until the deadline".
//
// The registry lives on the PAGE (window.__brwWebMCPInvocations), not in the
// daemon, for the same reason the frame scope does: every transport already
// evaluates expressions in the page, so one implementation covers direct CDP and
// the extension bridge, with no second copy to keep in step.
//
// It also answers a question the daemon could not: whether the invocation is
// still there. The registry carries a per-document nonce that the invocation id
// embeds. A navigation replaces the global, so a poll that finds no document
// holding that nonce knows the tool's document is gone and says so, instead of
// reporting "running" until the caller's timeout expires on a page that stopped
// existing seconds ago.

// MaxPageToolInputBytes caps the JSON arguments one invocation may carry. The
// cap is enforced in the daemon BEFORE the expression is built, so an oversized
// payload never reaches the page. Arguments are refused, never truncated: a
// silently shortened argument is a different call than the one the agent asked
// for, and it would look successful.
const MaxPageToolInputBytes = 64 << 10

// Timeout bounds for a synchronous invocation or an explicit result wait.
const (
	DefaultPageToolTimeout = 30 * time.Second
	MaxPageToolTimeout     = 10 * time.Minute
)

// Poll pacing. The first polls are tight so a fast tool answers in roughly one
// extra round trip; the interval then backs off, because a tool that has run for
// a minute is not likely to finish in the next 100ms and every poll costs an
// evaluate over the transport.
const (
	firstPageToolPollInterval = 100 * time.Millisecond
	maxPageToolPollInterval   = time.Second
)

// Invocation statuses. running/done/failed/cancelled are the tool's own
// lifecycle; lost and unknown are what the page reports about the ID itself.
const (
	PageToolRunning   = "running"
	PageToolDone      = "done"
	PageToolFailed    = "failed"
	PageToolCancelled = "cancelled"
	PageToolLost      = "lost"
	PageToolUnknown   = "unknown"
	// not_found (no such tool or frame) and invalid_input (arguments the tool's
	// own schema refuses) are kept apart because they send an agent to different
	// fixes: list the page's tools again, or correct the arguments.
	PageToolNotFound = "not_found"
	PageToolRejected = "invalid_input"
	PageToolError    = "error"
)

// ErrPageToolInputTooLarge is returned instead of dispatching an oversized
// argument payload. Named so a caller can branch on it rather than matching text.
var ErrPageToolInputTooLarge = errors.New("webmcp page tool input too large")

// ErrPageToolIDUnrecognised is returned for an invocation id this runtime could
// never have minted, which is a different mistake from an id whose page is gone.
var ErrPageToolIDUnrecognised = errors.New("webmcp invocation id not recognised")

// pageToolIDPattern matches "<document nonce>-<sequence>" as minted in the page.
var pageToolIDPattern = regexp.MustCompile(`^[0-9a-f]{8,40}-[0-9]{1,9}$`)

// PageToolInvokeOptions is one invocation request.
type PageToolInvokeOptions struct {
	// Name is the WebMCP tool name from the page tools listing.
	Name string
	// Arguments is the JSON arguments object passed to the tool.
	Arguments json.RawMessage
	// Frame targets a same-origin iframe by brw ref or CSS selector. Empty (or
	// "main") uses the top document.
	Frame string
	// Detach returns as soon as the tool has started, with an invocation id.
	Detach bool
	// Timeout bounds a non-detached invocation. Zero means DefaultPageToolTimeout.
	Timeout time.Duration
	// Validate checks the arguments against the tool's declared inputSchema
	// before dispatch. Callers that trust a schema less than the tool turn it off.
	Validate bool
}

// PageToolInvocation is the state of one invocation as the page reports it.
type PageToolInvocation struct {
	OK     bool   `json:"ok"`
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"`
	Frame  string `json:"frame,omitempty"`
	// TabID names the tab the invocation was started in. The page cannot know
	// it, so the daemon stamps it: a poll walks only the windows of whatever tab
	// it lands in, and a page tool that opens a tab moves the active one, so an
	// agent needs a tab to poll back into.
	TabID string `json:"tab_id,omitempty"`
	// Detached marks a start that returned without waiting for the result.
	Detached bool `json:"detached,omitempty"`
	// TimedOut marks a wait that ended with the tool still running. The
	// invocation is untouched and still addressable by ID.
	TimedOut bool `json:"timed_out,omitempty"`
	// Interrupted marks a wait that ended for a reason other than its timeout —
	// a cancelled request, a closed tab, an evaluate that failed. The invocation
	// itself is untouched, so the ID is still the handle on it.
	Interrupted bool            `json:"interrupted,omitempty"`
	Cancelled   bool            `json:"cancelled,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
	ElapsedMS   int64           `json:"elapsed_ms,omitempty"`
	// StartedURL is the document the invocation was started in, and CurrentURL
	// where the tab is now: together they say what a lost invocation lost.
	StartedURL string `json:"started_url,omitempty"`
	CurrentURL string `json:"current_url,omitempty"`
	Note       string `json:"note,omitempty"`
	// UntrustedOutput marks Result as content the page wrote. It is data for the
	// agent to use, never instructions for it to follow.
	UntrustedOutput bool `json:"untrusted_output,omitempty"`
}

// PageToolEvaluator runs one expression in the target tab and returns its
// decoded JSON value. Every transport's Evaluate satisfies it, which is what
// keeps detached invocation working identically on direct CDP and the extension
// bridge without a second implementation.
type PageToolEvaluator func(ctx context.Context, expression string) (any, error)

// webmcpRuntimeHelpers is the shared page-side prelude: the invocation registry,
// the same-origin window walk both the frame target and the id lookup use, and
// the reporting shape. Every script below embeds it, because a navigation can
// have replaced the document between any two calls.
// Sites commonly register WebMCP tools from a lazily loaded chunk, a second or
// so after the load event an open or navigation waits for. A listing that finds
// no tools on a document younger than webmcpSettleWindowMsJS waits for them, up
// to the per-call budget, instead of reporting an empty page.
const (
	webmcpSettleWindowMsJS = "10000"
	webmcpListSettleMsJS   = "2000"
	webmcpDigestSettleMsJS = "1200"
	webmcpInvokeSettleMsJS = "2500"
)

const webmcpRuntimeHelpers = FrameWalkHelpers + `
  var __BRW_WEBMCP_FRAME_DEPTH = 5;
  var __BRW_WEBMCP_KEEP_MS = 300000;
  function __brwWebMCPSafe(value){
    try { JSON.stringify(value); return value; } catch (e) { return String(value); }
  }
  function __brwWebMCPState(win, create){
    var state = null;
    try { state = win.__brwWebMCPInvocations || null; } catch (e) { return null; }
    if (state || !create) return state;
    var nonce = '';
    try {
      var buf = new Uint8Array(8);
      win.crypto.getRandomValues(buf);
      for (var i = 0; i < buf.length; i++) nonce += ('0' + buf[i].toString(16)).slice(-2);
    } catch (e) { nonce = Date.now().toString(16) + Math.random().toString(16).slice(2, 10); }
    state = { nonce: nonce, seq: 0, calls: {} };
    try { win.__brwWebMCPInvocations = state; } catch (e) { return null; }
    return state;
  }
  function __brwWebMCPWindows(win, depth, out){
    out.push(win);
    if (depth >= __BRW_WEBMCP_FRAME_DEPTH) return out;
    var frames = null;
    try { frames = win.document.querySelectorAll('iframe,frame'); } catch (e) { return out; }
    for (var i = 0; i < frames.length; i++) {
      var child = null;
      try { child = frames[i].contentWindow; if (child && !child.document) child = null; } catch (e) { child = null; }
      if (child) __brwWebMCPWindows(child, depth + 1, out);
    }
    return out;
  }
  function __brwWebMCPFrameWindow(target){
    var t = String(target == null ? '' : target).trim();
    if (t === '' || t === 'main' || t === 'top') return { ok: true, win: window, frame: 'main' };
    // A ref resolves through the ONE shared lookup. Its private copy here walked
    // the same windows but knew nothing about shadow roots or frame-qualified
    // refs, so an f<i>:<ref> came back as "webmcp frame not found" instead of
    // naming the document brw cannot reach. A CSS selector still falls through to
    // the window walk, which is what that walk is for.
    var el = null;
    var hit = __abFindDeep(t);
    if (hit) el = hit.el;
    if (!el) {
      var wins = __brwWebMCPWindows(window, 0, []);
      for (var i = 0; i < wins.length && !el; i++) {
        var doc = null;
        try { doc = wins[i].document; } catch (e) { continue; }
        if (!doc) continue;
        try { el = doc.querySelector(t); } catch (e) { el = null; }
      }
    }
    if (!el) return { ok: false, error: 'webmcp frame not found: ' + t + ' (pass a brw ref or a CSS selector for the iframe, or "main" for the top document)' };
    var tag = String(el.tagName || '').toLowerCase();
    var frameEl = (tag === 'iframe' || tag === 'frame') ? el : null;
    if (!frameEl) {
      // A ref for an element INSIDE a frame identifies the frame that holds it,
      // which is what an agent holding a snapshot ref actually has.
      try { var owner = el.ownerDocument.defaultView; frameEl = (owner && owner.frameElement) ? owner.frameElement : null; } catch (e) { frameEl = null; }
    }
    if (!frameEl) return { ok: false, error: 'webmcp frame target ' + t + ' resolves to an element in the top document, not to a frame' };
    var win = null;
    try { win = frameEl.contentWindow; if (win && !win.document) win = null; } catch (e) { win = null; }
    if (!win) return { ok: false, error: 'webmcp frame is cross-origin: ' + t + ' (the browser isolates its document, so page tools declared inside it cannot be reached from the top frame)' };
    return { ok: true, win: win, frame: t };
  }
  function __brwWebMCPContext(win){
    var mc = null;
    try { mc = win.__brwWebMCPRuntime || null; } catch (e) { mc = null; }
    if (!mc) { try { mc = (win.document && win.document.modelContext) || null; } catch (e) { mc = null; } }
    if (!mc) { try { mc = (win.navigator && win.navigator.modelContext) || null; } catch (e) { mc = null; } }
    if (!mc) { try { mc = (win.navigator && win.navigator.modelContextTesting) || null; } catch (e) { mc = null; } }
    return mc;
  }
  function __brwWebMCPIsNative(mc){
    return !!(mc && !mc.__brw && (typeof mc.getTools === 'function' || typeof mc.listTools === 'function'));
  }
  function __brwWebMCPNativeTools(mc){
    return typeof mc.getTools === 'function' ? mc.getTools() : mc.listTools();
  }
  function __brwWebMCPSchema(schema){
    if (typeof schema === 'string') { try { return JSON.parse(schema); } catch (e) { return null; } }
    return (schema && typeof schema === 'object') ? schema : null;
  }
  var __BRW_WEBMCP_HINTS = ['readOnlyHint','destructiveHint','idempotentHint','openWorldHint','consequentialHint','untrustedContentHint'];
  function __brwWebMCPAnnotations(tool){
    var src = tool && tool.annotations;
    if (typeof src === 'string') { try { src = JSON.parse(src); } catch (e) { src = null; } }
    if (!src || typeof src !== 'object') return null;
    var out = null;
    for (var i = 0; i < __BRW_WEBMCP_HINTS.length; i++) {
      var k = __BRW_WEBMCP_HINTS[i], bare = k.slice(0, -4);
      var v = typeof src[k] === 'boolean' ? src[k] : src[bare];
      if (typeof v === 'boolean') { out = out || {}; out[k] = v; }
    }
    // Chromium's native getTools passes readOnlyHint and untrustedContentHint
    // through and drops consequentialHint, so a booking or checkout tool reads as
    // merely "not read-only". A tool that says it writes and says nothing about
    // consequence is treated as consequential, and marked as inferred.
    if (out && out.readOnlyHint === false && out.consequentialHint === undefined && out.destructiveHint === undefined) {
      out.consequentialHint = true;
      out.consequentialInferred = true;
    }
    return out;
  }
  function __brwWebMCPEntry(tool, extra){
    var entry = { name: String(tool.name), description: String(tool.description || ''),
                  inputSchema: __brwWebMCPSchema(tool.inputSchema || tool.input_schema || null),
                  annotations: __brwWebMCPAnnotations(tool), raw: tool, native: false, declarative: false, mc: null };
    if (extra) for (var k in extra) entry[k] = extra[k];
    return entry;
  }
  function __brwWebMCPSetValue(el, value){
    var proto = Object.getPrototypeOf(el);
    var desc = null;
    while (proto && !desc) { desc = Object.getOwnPropertyDescriptor(proto, 'value'); proto = Object.getPrototypeOf(proto); }
    if (desc && desc.set) desc.set.call(el, value); else el.value = value;
  }
  function __brwWebMCPFillForm(form, args, submit){
    var filled = [];
    var keys = Object.keys(args || {});
    for (var i = 0; i < keys.length; i++) {
      var key = keys[i], value = args[key], els = [];
      var all = form.elements || [];
      for (var j = 0; j < all.length; j++) { if (all[j].name === key) els.push(all[j]); }
      if (!els.length) continue;
      for (var n = 0; n < els.length; n++) {
        var el = els[n], type = String(el.type || '').toLowerCase();
        if (type === 'password' || type === 'file' || type === 'hidden') continue;
        if (type === 'checkbox') el.checked = !!value;
        else if (type === 'radio') el.checked = (el.value === String(value));
        else __brwWebMCPSetValue(el, value == null ? '' : String(value));
        try { el.dispatchEvent(new Event('input', { bubbles: true })); el.dispatchEvent(new Event('change', { bubbles: true })); } catch (e) {}
      }
      filled.push(key);
    }
    if (!submit) {
      return { filled: filled, submitted: false,
               note: 'form filled but not submitted: it does not declare toolautosubmit; submit it with brw_click once the user has agreed' };
    }
    var response = null, responded = false, ev = null;
    try { ev = new SubmitEvent('submit', { bubbles: true, cancelable: true }); } catch (e) { ev = new Event('submit', { bubbles: true, cancelable: true }); }
    try {
      Object.defineProperty(ev, 'agentInvoked', { value: true });
      Object.defineProperty(ev, 'respondWith', { value: function(p){ responded = true; response = p; } });
    } catch (e) {}
    var proceed = form.dispatchEvent(ev);
    if (responded) {
      return Promise.resolve(response).then(function(value){ return value === undefined ? { filled: filled, submitted: true } : value; });
    }
    if (proceed) form.submit();
    return { filled: filled, submitted: proceed };
  }
  function __brwWebMCPFormTools(win){
    var out = [];
    var forms = null;
    try { forms = win.document.querySelectorAll('form[toolname]'); } catch (e) { return out; }
    for (var i = 0; i < forms.length; i++) {
      (function(form){
        var name = String(form.getAttribute('toolname') || '').trim();
        if (!name) return;
        var props = {}, required = [], fields = form.elements || [];
        for (var j = 0; j < fields.length; j++) {
          var f = fields[j], type = String(f.type || '').toLowerCase();
          if (!f.name || /^(hidden|submit|button|reset|file|password|image)$/.test(type)) continue;
          var spec = props[f.name] || { type: (type === 'number' || type === 'range') ? 'number' : (type === 'checkbox' ? 'boolean' : 'string') };
          var d = f.getAttribute('toolparamdescription');
          if (d) spec.description = d;
          if (String(f.tagName).toUpperCase() === 'SELECT') {
            spec['enum'] = [];
            for (var o = 0; o < f.options.length; o++) spec['enum'].push(f.options[o].value);
          } else if (type === 'radio') {
            spec['enum'] = (spec['enum'] || []).concat([f.value]);
          }
          props[f.name] = spec;
          if (f.required && required.indexOf(f.name) < 0) required.push(f.name);
        }
        var auto = form.hasAttribute('toolautosubmit');
        var tool = { name: name, description: String(form.getAttribute('tooldescription') || ''),
                     inputSchema: { type: 'object', properties: props, required: required },
                     annotations: auto ? { consequentialHint: true } : { readOnlyHint: false },
                     execute: function(args){ return __brwWebMCPFillForm(form, args || {}, auto); } };
        out.push(__brwWebMCPEntry(tool, { declarative: true }));
      })(forms[i]);
    }
    return out;
  }
  async function __brwWebMCPToolList(win){
    var out = [], seen = {};
    function add(entry){
      if (!entry || !entry.name || seen[entry.name]) return;
      seen[entry.name] = 1;
      out.push(entry);
    }
    var registered = null;
    try { registered = win.__brwWebMCPTools || null; } catch (e) { registered = null; }
    if (registered && registered.forEach) registered.forEach(function(t){ if (t && t.name) add(__brwWebMCPEntry(t)); });
    var mc = __brwWebMCPContext(win);
    if (__brwWebMCPIsNative(mc)) {
      var tools = null;
      try { tools = await __brwWebMCPNativeTools(mc); } catch (e) { tools = null; }
      if (tools && tools.forEach) tools.forEach(function(t){
        if (!t || !t.name) return;
        // Chromium's getTools also returns same-origin child frames' tools;
        // those belong to the frame, which is listed with frame.
        try { if (t.window && t.window !== win) return; } catch (e) { return; }
        add(__brwWebMCPEntry(t, { native: true, mc: mc, declarative: !!t.backendNodeId }));
      });
    } else {
      try {
        var legacy = mc ? (mc.tools || (mc.context && mc.context.tools)) : null;
        if (legacy && legacy.forEach) legacy.forEach(function(t){ if (t && t.name) add(__brwWebMCPEntry(t)); });
      } catch (e) {}
      __brwWebMCPFormTools(win).forEach(add);
    }
    return out;
  }
  function __brwWebMCPSupported(win){
    return !!__brwWebMCPContext(win);
  }
  async function __brwWebMCPAwaitTools(win, wanted, maxMs, explicit){
    var list = await __brwWebMCPToolList(win);
    var start = Date.now();
    function satisfied(l){
      if (!wanted) return l.length > 0;
      for (var i = 0; i < l.length; i++) { if (l[i].name === wanted) return true; }
      return false;
    }
    var mc = __brwWebMCPContext(win);
    // An explicit listing or call waits on any young document: a site that
    // registers from a post-hydration effect has not touched the API yet when
    // the load event fires. The per-navigation digest waits only once the page
    // has feature-detected brw's runtime, so ordinary pages cost nothing.
    var expecting = !!mc && (!!explicit || (!!mc.__brw && !!win.__brwWebMCPTouched));
    while (!satisfied(list) && expecting && Date.now() - start < maxMs) {
      var age = 0;
      try { age = win.performance.now(); } catch (e) { age = ` + webmcpSettleWindowMsJS + `; }
      if (age >= ` + webmcpSettleWindowMsJS + ` && win.document.readyState === 'complete') break;
      await new Promise(function(r){ setTimeout(r, 100); });
      list = await __brwWebMCPToolList(win);
    }
    return list;
  }
  function __brwWebMCPStringInput(win){
    var m = null;
    try { m = /Chrom(?:e|ium)\/(\d+)/.exec(win.navigator.userAgent); } catch (e) { m = null; }
    return !!(m && Number(m[1]) < 155);
  }
  function __brwWebMCPOutput(value){
    if (typeof value !== 'string') return value;
    try { return JSON.parse(value); } catch (e) { return value; }
  }
  function __brwWebMCPDescribe(entry){
    var out = { name: entry.name, description: entry.description, inputSchema: entry.inputSchema };
    if (entry.annotations) out.annotations = entry.annotations;
    if (entry.declarative) out.declarative = true;
    return out;
  }
  function __brwWebMCPValidate(tool, args){
      var schema = tool.inputSchema;
    if (!schema || typeof schema !== 'object') return '';
    var isObject = (args !== null && typeof args === 'object' && !Array.isArray(args));
    if (!isObject) {
      if (schema.type === 'object') return 'webmcp input invalid: ' + tool.name + ' declares an object inputSchema but the arguments are not an object';
      return '';
    }
    var missing = [];
    var required = schema.required;
    if (required && required.length) {
      for (var i = 0; i < required.length; i++) {
        if (!Object.prototype.hasOwnProperty.call(args, required[i])) missing.push(required[i]);
      }
    }
    if (missing.length) return 'webmcp input invalid: ' + tool.name + ' requires ' + missing.join(', ');
    var props = schema.properties || {};
    var wrong = [];
    for (var key in args) {
      if (!Object.prototype.hasOwnProperty.call(args, key)) continue;
      var spec = props[key];
      if (!spec || typeof spec.type !== 'string') continue;
      var value = args[key], want = spec.type, fits = true;
      if (want === 'string') fits = (typeof value === 'string');
      else if (want === 'number') fits = (typeof value === 'number' && isFinite(value));
      else if (want === 'integer') fits = (typeof value === 'number' && isFinite(value) && Math.floor(value) === value);
      else if (want === 'boolean') fits = (typeof value === 'boolean');
      else if (want === 'array') fits = Array.isArray(value);
      else if (want === 'object') fits = (value !== null && typeof value === 'object' && !Array.isArray(value));
      if (!fits) wrong.push(key + ' should be ' + want);
    }
    if (wrong.length) return 'webmcp input invalid: ' + tool.name + ' got ' + wrong.join('; ');
    return '';
  }
  function __brwWebMCPReap(state){
    var now = Date.now();
    var ids = Object.keys(state.calls);
    for (var i = 0; i < ids.length; i++) {
      var rec = state.calls[ids[i]];
      // A finished invocation is kept so a retried poll still gets its result,
      // but not forever: a long-lived single-page app would otherwise hold every
      // result the agent ever collected.
      if (rec && rec.status !== 'running' && rec.finished && (now - rec.finished) > __BRW_WEBMCP_KEEP_MS) delete state.calls[ids[i]];
    }
  }
  function __brwWebMCPReport(rec){
    var out = { ok: rec.status === 'done', id: rec.id, name: rec.name, status: rec.status,
                frame: rec.frame || 'main',
                elapsed_ms: (rec.finished || Date.now()) - rec.started };
    if (rec.status === 'done') out.result = __brwWebMCPSafe(rec.result);
    if (rec.error) out.error = rec.error;
    if (rec.url) out.started_url = rec.url;
    try { out.current_url = location.href; } catch (e) {}
    return out;
  }
  function __brwWebMCPFind(id){
    var nonce = String(id).split('-')[0];
    var wins = __brwWebMCPWindows(window, 0, []);
    for (var i = 0; i < wins.length; i++) {
      var state = __brwWebMCPState(wins[i], false);
      if (!state) continue;
      if (Object.prototype.hasOwnProperty.call(state.calls, id)) return { found: true, rec: state.calls[id] };
      // The document that minted the id is still here, so the id is simply not
      // one it knows — a different answer from the document having gone.
      if (state.nonce === nonce) return { found: false, known: true };
    }
    return { found: false, known: false };
  }
  function __brwWebMCPMissing(id, known){
    var url = '';
    try { url = location.href; } catch (e) {}
    if (known) {
      return { ok: false, id: id, status: 'unknown', current_url: url,
               error: 'webmcp invocation not found: ' + id + ' (this document minted no such invocation; use the id brw_call_page_tool returned)' };
    }
    return { ok: false, id: id, status: 'lost', current_url: url,
             error: 'webmcp invocation lost: no document in the polled tab holds ' + id + ' (it navigated away before the tool finished, or the poll landed on a different tab: pass the tab_id the invocation reported)' };
  }
`

// BuildPageToolsExpression lists the WebMCP tools one document exposes, merging
// brw's captured registry with a native document.modelContext. frame targets a
// same-origin iframe; empty or "main" is the top document.
func BuildPageToolsExpression(frame string) string {
	frameJSON, _ := json.Marshal(frame)
	return `(async function(){` + webmcpRuntimeHelpers + `
  var target = __brwWebMCPFrameWindow(` + string(frameJSON) + `);
  if (!target.ok) return { supported: false, frame: String(` + string(frameJSON) + ` || 'main'), tools: [], error: target.error };
  var list = await __brwWebMCPAwaitTools(target.win, '', ` + webmcpListSettleMsJS + `, true);
  var tools = [];
  for (var i = 0; i < list.length; i++) tools.push(__brwWebMCPDescribe(list[i]));
  var mc = __brwWebMCPContext(target.win);
  var runtime = __brwWebMCPIsNative(mc) ? 'native' : (mc && mc.__brw ? 'brw' : (mc ? 'legacy' : (tools.length ? 'declarative' : 'none')));
  return { supported: !!(__brwWebMCPSupported(target.win) || tools.length), runtime: runtime, frame: target.frame, tools: tools };
})()`
}

// MaxPageToolSummaries caps the tools a page-state result lists, and
// pageToolSummaryChars the description kept for each. The full list with input
// schemas is one brw_page_tools call away.
const (
	MaxPageToolSummaries  = 20
	pageToolSummaryChars  = 120
	maxPageSurfaceEntries = 5
)

// PageToolSummary is one page tool as a page-state result reports it: enough to
// decide whether to call brw_page_tools, not enough to call the tool.
type PageToolSummary struct {
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	ReadOnly      bool   `json:"read_only,omitempty"`
	Consequential bool   `json:"consequential,omitempty"`
	Declarative   bool   `json:"declarative,omitempty"`
}

// PageSurfaces are the agent-facing endpoints a document declares in its own
// <link> elements. brw does not call or proxy them; it reports them so an agent
// can use them directly instead of driving the page.
type PageSurfaces struct {
	Markdown        []string `json:"markdown,omitempty"`
	LLMs            []string `json:"llms,omitempty"`
	APIDescriptions []string `json:"api_descriptions,omitempty"`
	MCP             []string `json:"mcp,omitempty"`
}

// Empty reports whether the document declared nothing.
func (p PageSurfaces) Empty() bool {
	return len(p.Markdown)+len(p.LLMs)+len(p.APIDescriptions)+len(p.MCP) == 0
}

// PageSurfaceDigest is what one evaluate learns about a document's agent
// surfaces: its page tools (capped) and its declared endpoints.
type PageSurfaceDigest struct {
	Tools      []PageToolSummary `json:"tools,omitempty"`
	ToolsTotal int               `json:"tools_total,omitempty"`
	Surfaces   PageSurfaces      `json:"surfaces"`
}

// BuildPageSurfacesExpression renders the digest script for the top document.
func BuildPageSurfacesExpression() string {
	return `(async function(){` + webmcpRuntimeHelpers + `
  var MAX = ` + strconv.Itoa(MaxPageToolSummaries) + `, CHARS = ` + strconv.Itoa(pageToolSummaryChars) + `, LINKS = ` + strconv.Itoa(maxPageSurfaceEntries) + `;
  var list = [];
  try { list = await __brwWebMCPAwaitTools(window, '', ` + webmcpDigestSettleMsJS + `, false); } catch (e) { list = []; }
  var tools = [];
  for (var i = 0; i < list.length && tools.length < MAX; i++) {
    var t = list[i], a = t.annotations || {};
    var d = String(t.description || '').split(/\r?\n/)[0].trim();
    if (d.length > CHARS) d = d.slice(0, CHARS - 1) + '\u2026';
    var row = { name: t.name };
    if (d) row.description = d;
    if (a.readOnlyHint === true) row.read_only = true;
    if (a.consequentialHint === true || a.destructiveHint === true) row.consequential = true;
    if (t.declarative) row.declarative = true;
    tools.push(row);
  }
  var surfaces = { markdown: [], llms: [], api_descriptions: [], mcp: [] };
  function put(key, href){
    if (!href || surfaces[key].length >= LINKS || surfaces[key].indexOf(href) >= 0) return;
    surfaces[key].push(href);
  }
  var links = [];
  try { links = document.querySelectorAll('link[rel][href]'); } catch (e) { links = []; }
  for (var j = 0; j < links.length; j++) {
    var l = links[j];
    var rel = ' ' + String(l.getAttribute('rel') || '').toLowerCase().split(/\s+/).join(' ') + ' ';
    var type = String(l.getAttribute('type') || '').toLowerCase().split(';')[0].trim();
    var href = '';
    try { href = l.href; } catch (e) { href = ''; }
    if (!/^https?:/i.test(href)) continue;
    if (rel.indexOf(' alternate ') >= 0 && (type === 'text/markdown' || type === 'text/x-markdown')) put('markdown', href);
    if (rel.indexOf(' llms ') >= 0 || rel.indexOf(' llms-txt ') >= 0 || rel.indexOf(' llms-full ') >= 0) put('llms', href);
    if (rel.indexOf(' service-desc ') >= 0 || rel.indexOf(' api-catalog ') >= 0 || /openapi|vnd\.oai/.test(type)) put('api_descriptions', href);
    if (rel.indexOf(' mcp ') >= 0 || type === 'application/mcp+json') put('mcp', href);
  }
  return { tools: tools, tools_total: list.length, surfaces: surfaces };
})()`
}

// ReadPageSurfaces runs BuildPageSurfacesExpression through eval.
func ReadPageSurfaces(ctx context.Context, eval PageToolEvaluator) (PageSurfaceDigest, error) {
	if eval == nil {
		return PageSurfaceDigest{}, errors.New("no page evaluator available")
	}
	raw, err := eval(ctx, BuildPageSurfacesExpression())
	if err != nil {
		return PageSurfaceDigest{}, err
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return PageSurfaceDigest{}, err
	}
	var digest PageSurfaceDigest
	if err := json.Unmarshal(encoded, &digest); err != nil {
		return PageSurfaceDigest{}, fmt.Errorf("page surfaces could not be decoded: %w", err)
	}
	if digest.ToolsTotal <= len(digest.Tools) {
		digest.ToolsTotal = 0
	}
	for _, list := range []*[]string{&digest.Surfaces.Markdown, &digest.Surfaces.LLMs, &digest.Surfaces.APIDescriptions, &digest.Surfaces.MCP} {
		if len(*list) == 0 {
			*list = nil
		}
	}
	if len(digest.Tools) == 0 {
		digest.Tools = nil
	}
	return digest, nil
}

// PageToolDescriptor is one entry of the brw_page_tools listing.
type PageToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	Annotations map[string]bool `json:"annotations,omitempty"`
	Declarative bool            `json:"declarative,omitempty"`
}

// Consequential reports whether the page says calling the tool has effects a
// person should agree to first.
func (d PageToolDescriptor) Consequential() bool {
	return d.Annotations["consequentialHint"] || d.Annotations["destructiveHint"]
}

// PageToolListing is the brw_page_tools result.
type PageToolListing struct {
	Supported bool                 `json:"supported"`
	Runtime   string               `json:"runtime,omitempty"`
	Frame     string               `json:"frame,omitempty"`
	Tools     []PageToolDescriptor `json:"tools"`
	Error     string               `json:"error,omitempty"`
}

// ListPageTools runs BuildPageToolsExpression through eval.
func ListPageTools(ctx context.Context, eval PageToolEvaluator, frame string) (PageToolListing, error) {
	if eval == nil {
		return PageToolListing{}, errors.New("no page evaluator available")
	}
	raw, err := eval(ctx, BuildPageToolsExpression(frame))
	if err != nil {
		return PageToolListing{}, err
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return PageToolListing{}, err
	}
	var listing PageToolListing
	if err := json.Unmarshal(encoded, &listing); err != nil {
		return PageToolListing{}, fmt.Errorf("page tools could not be decoded: %w", err)
	}
	return listing, nil
}

// BuildPageToolInvokeExpression renders the start-an-invocation script. It
// refuses an oversized argument payload here, before any expression exists,
// which is what "never reaches the page" means.
func BuildPageToolInvokeExpression(opts PageToolInvokeOptions) (string, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return "", errors.New("page tool name is required")
	}
	args := strings.TrimSpace(string(opts.Arguments))
	if args == "" || args == "null" {
		args = "{}"
	}
	if len(args) > MaxPageToolInputBytes {
		return "", fmt.Errorf("%w: %d bytes of arguments exceeds the %d byte cap; pass a URL or an id the page tool can fetch instead of inlining the payload",
			ErrPageToolInputTooLarge, len(args), MaxPageToolInputBytes)
	}
	if !json.Valid([]byte(args)) {
		return "", errors.New("page tool arguments are not valid json")
	}
	nameJSON, _ := json.Marshal(opts.Name)
	frameJSON, _ := json.Marshal(opts.Frame)
	validate := "false"
	if opts.Validate {
		validate = "true"
	}
	return `(async function(){` + webmcpRuntimeHelpers + `
  var NAME = ` + string(nameJSON) + `;
  var ARGS = ` + args + `;
  var FRAME = ` + string(frameJSON) + `;
  var VALIDATE = ` + validate + `;
  var target = __brwWebMCPFrameWindow(FRAME);
  if (!target.ok) return { ok: false, status: 'not_found', error: target.error };
  var win = target.win;
  var list = await __brwWebMCPAwaitTools(win, NAME, ` + webmcpInvokeSettleMsJS + `, true);
  var tool = null;
  for (var i = 0; i < list.length; i++) { if (list[i].name === NAME) { tool = list[i]; break; } }
  if (!tool) return { ok: false, status: 'not_found', frame: target.frame,
                      error: 'page tool not found: ' + NAME + ' (call brw_page_tools to list available page tools)' };
  var raw = tool.raw;
  var direct = raw && (raw.execute || raw.call || raw.run || raw.handler);
  var fn = null;
  if (typeof direct === 'function') {
    fn = function(args, options){ return options ? direct.call(raw, args, options) : direct.call(raw, args); };
  } else if (tool.native && typeof tool.mc.executeTool === 'function') {
    // Native executeTool answers a JSON string. Before Chromium 155 it took its
    // input as one too, and says "Failed to parse input arguments" when handed
    // an object, so that refusal is retried once with the string.
    var mc = tool.mc;
    var testing = typeof mc.getTools !== 'function';
    var stringFirst = testing || __brwWebMCPStringInput(win);
    fn = function(args, options){
      var subject = testing ? raw.name : raw;
      function run(input){ return Promise.resolve(mc.executeTool(subject, input, options)); }
      var first = run(stringFirst ? JSON.stringify(args) : args);
      if (stringFirst) return first.then(__brwWebMCPOutput);
      return first.catch(function(err){
        if (!/parse input/i.test(String(err && err.message || err))) throw err;
        return run(JSON.stringify(args));
      }).then(__brwWebMCPOutput);
    };
  }
  if (!fn) return { ok: false, status: 'not_found', frame: target.frame,
                      error: 'page tool has no callable execute: ' + NAME };
  if (VALIDATE) {
    var invalid = __brwWebMCPValidate(tool, ARGS);
    if (invalid) return { ok: false, status: 'invalid_input', frame: target.frame, error: invalid };
  }
  var state = __brwWebMCPState(win, true);
  if (!state) return { ok: false, status: 'error', frame: target.frame,
                      error: 'webmcp invocation registry could not be installed in this document' };
  __brwWebMCPReap(state);
  state.seq++;
  var id = state.nonce + '-' + state.seq;
  var rec = { id: id, name: NAME, status: 'running', started: Date.now(), finished: 0, frame: target.frame, url: '' };
  try { rec.url = win.location.href; } catch (e) {}
  state.calls[id] = rec;
  // A tool that honours an AbortSignal can stop its own work when the agent
  // cancels; one that ignores it still stops being waited on, because the record
  // is what decides the reported outcome.
  var controller = null;
  try { controller = (typeof win.AbortController === 'function') ? new win.AbortController() : null; } catch (e) { controller = null; }
  rec.abort = controller;
  function settle(status, value, message){
    if (rec.status !== 'running') return;
    rec.status = status;
    rec.finished = Date.now();
    if (status === 'done') rec.result = value; else rec.error = message;
  }
  try {
    var pending = controller ? fn(ARGS, { signal: controller.signal }) : fn(ARGS);
    Promise.resolve(pending).then(
      function(value){ settle('done', value); },
      function(err){ settle('failed', null, String(err && err.message || err)); });
  } catch (err) {
    settle('failed', null, String(err && err.message || err));
  }
  // fn can throw before this line, which has already settled rec to 'failed'.
  // Detached, this report is the whole answer, so it carries that outcome rather
  // than a hardcoded ok:true sending the agent to collect work that already failed.
  var out = { ok: rec.status === 'running', id: id, name: NAME, status: rec.status,
              frame: target.frame, started_url: rec.url };
  if (rec.error) out.error = rec.error;
  return out;
})()`, nil
}

// BuildPageToolResultExpression renders the poll script for one invocation id.
func BuildPageToolResultExpression(id string) string {
	idJSON, _ := json.Marshal(id)
	return `(function(){` + webmcpRuntimeHelpers + `
  var ID = ` + string(idJSON) + `;
  var hit = __brwWebMCPFind(ID);
  if (!hit.found) return __brwWebMCPMissing(ID, hit.known);
  return __brwWebMCPReport(hit.rec);
})()`
}

// BuildPageToolCancelExpression renders the cancel script for one invocation id.
func BuildPageToolCancelExpression(id string) string {
	idJSON, _ := json.Marshal(id)
	return `(function(){` + webmcpRuntimeHelpers + `
  var ID = ` + string(idJSON) + `;
  var hit = __brwWebMCPFind(ID);
  if (!hit.found) return __brwWebMCPMissing(ID, hit.known);
  var rec = hit.rec;
  if (rec.status !== 'running') {
    var settled = __brwWebMCPReport(rec);
    settled.cancelled = false;
    settled.note = 'invocation had already finished with status ' + rec.status;
    return settled;
  }
  rec.status = 'cancelled';
  rec.finished = Date.now();
  rec.error = 'webmcp invocation cancelled by the agent';
  try { if (rec.abort) rec.abort.abort(); } catch (e) {}
  var out = __brwWebMCPReport(rec);
  out.cancelled = true;
  out.note = 'the page tool was signalled through its AbortSignal; a tool that ignores the signal may still be running in the page, but nothing waits on it';
  return out;
})()`
}

// InvokePageTool starts a page tool and, unless detached, polls until it
// finishes or the timeout expires. A timed-out invocation is left running and
// stays addressable by its id rather than being abandoned.
func InvokePageTool(ctx context.Context, eval PageToolEvaluator, opts PageToolInvokeOptions) (PageToolInvocation, error) {
	expression, err := BuildPageToolInvokeExpression(opts)
	if err != nil {
		return PageToolInvocation{}, err
	}
	started, err := evaluatePageTool(ctx, eval, expression)
	if err != nil {
		return PageToolInvocation{}, err
	}
	if !started.OK || started.ID == "" {
		return started, nil
	}
	if opts.Detach {
		started.Detached = true
		started.Note = "started detached; collect it with brw_page_tool_result or stop it with brw_page_tool_cancel"
		return started, nil
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultPageToolTimeout
	}
	final, err := AwaitPageTool(ctx, eval, started.ID, timeout)
	if final.ID == "" {
		final.ID = started.ID
	}
	if final.Name == "" {
		final.Name = started.Name
	}
	if final.StartedURL == "" {
		final.StartedURL = started.StartedURL
	}
	if err != nil {
		return final, fmt.Errorf("webmcp invocation %s started but could not be collected: %w", started.ID, err)
	}
	return final, nil
}

// AwaitPageTool polls one invocation until it leaves the running state or the
// timeout expires. A zero timeout reads the current state once. However the wait
// ends, the report carries the invocation id: work that outlived the wait has to
// stay addressable rather than surviving only inside an error string.
func AwaitPageTool(ctx context.Context, eval PageToolEvaluator, id string, timeout time.Duration) (PageToolInvocation, error) {
	id, err := validatePageToolID(id)
	if err != nil {
		return PageToolInvocation{}, err
	}
	if timeout < 0 {
		timeout = 0
	}
	if timeout > MaxPageToolTimeout {
		timeout = MaxPageToolTimeout
	}
	deadline := time.Now().Add(timeout)
	wait := firstPageToolPollInterval
	expression := BuildPageToolResultExpression(id)

	last := PageToolInvocation{ID: id}
	var lastErr error
	for {
		got, err := evaluatePageTool(ctx, eval, expression)
		switch {
		case err == nil:
			lastErr = nil
			last = got
			if last.ID == "" {
				last.ID = id
			}
			if last.Status != PageToolRunning {
				return last, nil
			}
		case transientEvaluateFailure(err):
			// The evaluate landed mid-navigation and the old execution context
			// went away underneath it. The next poll runs in whatever document
			// the tab now has, which is where the invocation is reported lost.
			lastErr = err
		default:
			return interruptedPageTool(last, id), err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return interruptedPageTool(last, id), ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
		if wait > maxPageToolPollInterval {
			wait = maxPageToolPollInterval
		}
	}
	if lastErr != nil {
		return interruptedPageTool(last, id), lastErr
	}
	if timeout > 0 {
		last.TimedOut = true
		last.Note = fmt.Sprintf("still running after %dms; collect it later with brw_page_tool_result or stop it with brw_page_tool_cancel", timeout.Milliseconds())
	}
	return last, nil
}

// CancelPageTool stops waiting on an invocation and signals the page tool
// through its AbortSignal.
func CancelPageTool(ctx context.Context, eval PageToolEvaluator, id string) (PageToolInvocation, error) {
	id, err := validatePageToolID(id)
	if err != nil {
		return PageToolInvocation{}, err
	}
	return evaluatePageTool(ctx, eval, BuildPageToolCancelExpression(id))
}

// interruptedPageTool describes an invocation whose wait ended for a reason
// other than the timeout. Nothing was done to the invocation itself, so the
// report keeps the id and names the verbs that get back to it.
func interruptedPageTool(last PageToolInvocation, id string) PageToolInvocation {
	last.ID = id
	last.OK = false
	last.TimedOut = false
	last.Interrupted = true
	last.Note = "the wait ended early; the invocation itself was not stopped — collect it with brw_page_tool_result or stop it with brw_page_tool_cancel"
	return last
}

// validatePageToolID checks an invocation id and returns the trimmed form. The
// trimmed value is the one the page expression must embed: validating one string
// and then looking up another turns a padded id into a bogus "lost" report.
func validatePageToolID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("invocation id is required; it comes from brw_call_page_tool")
	}
	if !pageToolIDPattern.MatchString(id) {
		return "", fmt.Errorf("%w: %q was not minted by brw, so no page can be holding it", ErrPageToolIDUnrecognised, id)
	}
	return id, nil
}

// evaluatePageTool runs one page script and re-decodes its generic JSON value
// into the typed invocation report.
func evaluatePageTool(ctx context.Context, eval PageToolEvaluator, expression string) (PageToolInvocation, error) {
	if eval == nil {
		return PageToolInvocation{}, errors.New("no page evaluator available for webmcp invocation")
	}
	raw, err := eval(ctx, expression)
	if err != nil {
		return PageToolInvocation{}, err
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return PageToolInvocation{}, fmt.Errorf("webmcp invocation result could not be re-encoded: %w", err)
	}
	var out PageToolInvocation
	if err := json.Unmarshal(encoded, &out); err != nil {
		return PageToolInvocation{}, fmt.Errorf("webmcp invocation result could not be decoded: %w", err)
	}
	return out, nil
}

// transientEvaluateFailure reports whether an evaluate failed because the
// document it targeted was being replaced, which is worth one more poll rather
// than being surfaced as the invocation's outcome.
func transientEvaluateFailure(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"execution context",
		"context was destroyed",
		"cannot find context",
		"inspected target navigated",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}
