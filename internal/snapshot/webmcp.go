package snapshot

import (
	"context"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// WebMCP (W3C Web Machine Learning CG draft) lets a site expose callable tools
// to an agent through document.modelContext: registerTool(tool, {signal}),
// getTools() and executeTool(tool, input, {signal}). Chrome ships a native
// implementation behind a flag and in an origin trial; brw reads a native one
// whenever it exists, on every transport, without any flag.
//
// For browsers without it, --enable-webmcp installs WebMCPInstallScript at
// document-start so a cooperating site can still register tools against brw.
// It is opt-in because providing the API is observable to the page.

// WebMCPInstallScript provides document.modelContext (aliased as the legacy
// navigator.modelContext) and records registered tools into
// window.__brwWebMCPTools. It never replaces a native implementation: with a
// native getTools present it does nothing, and brw lists and invokes through the
// native API instead. A legacy navigator.modelContext polyfill is wrapped so its
// registrations are still seen. Idempotent.
const WebMCPInstallScript = `(function(){
  if (window.__brwWebMCPInstalled) return;
  window.__brwWebMCPInstalled = true;
  if (!window.__brwWebMCPTools) window.__brwWebMCPTools = [];
  var reg = window.__brwWebMCPTools;
  var events = null;
  function changed(){
    try { if (events && events.dispatchEvent) events.dispatchEvent(new Event('toolchange')); } catch (_) {}
  }
  function drop(tool){
    var i = reg.indexOf(tool);
    if (i >= 0) { reg.splice(i, 1); changed(); }
  }
  function record(tool, opts){
    if (!tool || !tool.name) return;
    var signal = opts && opts.signal;
    if (signal && signal.aborted) return;
    for (var i = reg.length - 1; i >= 0; i--) { if (reg[i] && reg[i].name === tool.name) reg.splice(i, 1); }
    reg.push(tool);
    if (signal && typeof signal.addEventListener === 'function') {
      signal.addEventListener('abort', function(){ drop(tool); });
    }
  }
  function recordMany(arg, opts){
    if (!arg) return;
    if (Array.isArray(arg)) { arg.forEach(function(t){ record(t); }); return; }
    if (arg.tools && Array.isArray(arg.tools)) { arg.tools.forEach(function(t){ record(t); }); return; }
    record(arg, opts);
  }
  function describe(tool){
    var schema = tool.inputSchema || tool.input_schema || null;
    var out = { name: String(tool.name), description: String(tool.description || ''), window: window };
    try { out.inputSchema = typeof schema === 'string' ? schema : JSON.stringify(schema || {}); } catch (_) { out.inputSchema = '{}'; }
    if (tool.annotations) out.annotations = tool.annotations;
    return out;
  }
  function find(name){
    for (var i = 0; i < reg.length; i++) { if (reg[i] && reg[i].name === name) return reg[i]; }
    return null;
  }
  try {
    var native = document.modelContext;
    if (native && typeof native.getTools === 'function') return;
    var legacy = navigator.modelContext;
    if (legacy) {
      ['registerTool','provideContext','provideTools','registerTools'].forEach(function(m){
        if (typeof legacy[m] === 'function') {
          var orig = legacy[m].bind(legacy);
          legacy[m] = function(a, o){ try { recordMany(a, o); } catch(_){} return orig(a, o); };
        }
      });
      return;
    }
    try { events = new EventTarget(); } catch (_) { events = null; }
    var mc = {
      __brw: true,
      registerTool: function(tool, opts){ recordMany(tool, opts); changed(); },
      provideContext: function(ctx){ recordMany(ctx); changed(); },
      getTools: function(){ return Promise.resolve(reg.map(describe)); },
      executeTool: function(tool, input, opts){
        var name = typeof tool === 'string' ? tool : (tool && tool.name);
        var found = find(name);
        if (!found) return Promise.reject(new Error('no such tool: ' + name));
        var fn = found.execute || found.call || found.run || found.handler;
        if (typeof fn !== 'function') return Promise.reject(new Error('tool has no execute: ' + name));
        var args = input;
        if (typeof input === 'string') { try { args = JSON.parse(input); } catch (_) {} }
        return Promise.resolve(fn.call(found, args, opts)).then(function(v){ return JSON.stringify(v === undefined ? null : v); });
      },
      addEventListener: function(type, fn, o){ if (events) events.addEventListener(type, fn, o); },
      removeEventListener: function(type, fn, o){ if (events) events.removeEventListener(type, fn, o); },
      get tools(){ return reg.slice(); }
    };
    // brw reads the runtime through __brwWebMCPRuntime; any read of the public
    // property is the page feature-detecting WebMCP, which is how a listing
    // knows tools may still be on their way from a lazily loaded chunk.
    Object.defineProperty(window, '__brwWebMCPRuntime', { configurable: true, value: mc });
    function touched(){ window.__brwWebMCPTouched = true; return mc; }
    Object.defineProperty(document, 'modelContext', { configurable: true, get: touched });
    Object.defineProperty(navigator, 'modelContext', { configurable: true, get: touched });
  } catch (_) {}
})()`

// BuildPageToolsExpression (webmcp_invoke.go) lists what a document exposes and
// BuildPageToolInvokeExpression starts one of its tools; both read the registry
// this shim fills as well as a native document.modelContext.

// RegisterWebMCPOnNewDocument arms the WebMCP shim to install at document-start
// on every future navigation (the CDP transports only) so it captures tool registrations
// before the page's own scripts run. Call once per tab.
func RegisterWebMCPOnNewDocument(ctx context.Context) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(WebMCPInstallScript).Do(ctx)
		return err
	}))
}
