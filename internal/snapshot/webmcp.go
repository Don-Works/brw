package snapshot

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// WebMCP (W3C Web Machine Context, draft 2026) lets a site expose callable tools
// to an agent via navigator.modelContext — far more token-efficient than DOM
// scraping. brw acts as the agent-side runtime: when enabled it installs a shim
// at document-start that captures the tools a cooperating site registers, and
// surfaces them through brw_page_tools / brw_call_page_tool. It is opt-in
// (--enable-webmcp) because providing navigator.modelContext is observable to the
// page; default-off brw never fabricates the API and only reads a native one.

// WebMCPInstallScript installs/augments navigator.modelContext at document-start
// and records registered tools into window.__brwWebMCPTools. If a native or
// polyfilled runtime already exists, its registration methods are wrapped (their
// behavior is preserved) so brw still sees the tools; otherwise a minimal runtime
// is provided so cooperating sites can register against brw. Idempotent.
const WebMCPInstallScript = `(function(){
  if (window.__brwWebMCPInstalled) return;
  window.__brwWebMCPInstalled = true;
  if (!window.__brwWebMCPTools) window.__brwWebMCPTools = [];
  function record(tool){
    if (!tool || !tool.name) return;
    var reg = window.__brwWebMCPTools;
    for (var i=0;i<reg.length;i++){ if (reg[i] && reg[i].name === tool.name){ reg[i] = tool; return; } }
    reg.push(tool);
  }
  function recordMany(arg){
    if (!arg) return;
    if (Array.isArray(arg)) { arg.forEach(record); return; }
    if (arg.tools && Array.isArray(arg.tools)) { arg.tools.forEach(record); return; }
    record(arg);
  }
  try {
    var existing = navigator.modelContext;
    if (existing) {
      ['registerTool','provideContext','provideTools','registerTools'].forEach(function(m){
        if (typeof existing[m] === 'function') {
          var orig = existing[m].bind(existing);
          existing[m] = function(a){ try { recordMany(a); } catch(_){} return orig(a); };
        }
      });
      return;
    }
    Object.defineProperty(navigator, 'modelContext', {
      configurable: true,
      value: {
        registerTool: function(t){ recordMany(t); return true; },
        provideContext: function(c){ recordMany(c); return true; },
        get tools(){ return window.__brwWebMCPTools.slice(); }
      }
    });
  } catch (_) {}
})()`

// PageToolsScript returns { supported, tools:[{name,description,inputSchema}] },
// merging brw's captured registry with any native navigator.modelContext tools.
const PageToolsScript = `(function(){
  var mc = (typeof navigator !== 'undefined') ? navigator.modelContext : null;
  var reg = window.__brwWebMCPTools || [];
  var tools = [];
  function add(t){
    if (!t || !t.name) return;
    tools.push({ name: String(t.name), description: String(t.description || ''), inputSchema: t.inputSchema || t.input_schema || null });
  }
  if (reg && reg.forEach) reg.forEach(add);
  try {
    if (mc) {
      var nt = mc.tools || (typeof mc.getTools === 'function' ? mc.getTools() : null) || (mc.context && mc.context.tools);
      if (nt && nt.forEach) nt.forEach(add);
    }
  } catch (_) {}
  var seen = {}, out = [];
  for (var i=0;i<tools.length;i++){ if (!seen[tools[i].name]){ seen[tools[i].name]=1; out.push(tools[i]); } }
  return { supported: !!(mc || out.length), tools: out };
})()`

// CallPageToolScript builds an async expression that invokes the named WebMCP
// page tool with args (already a JSON value) and returns { ok, result } or
// { ok:false, error }.
func CallPageToolScript(name string, args json.RawMessage) string {
	nameJSON, _ := json.Marshal(name)
	argsRaw := string(args)
	if len(args) == 0 || argsRaw == "null" {
		argsRaw = "{}"
	}
	return fmt.Sprintf(`(async function(){
  var NAME = %s;
  var ARGS = %s;
  var tool = null;
  var reg = window.__brwWebMCPTools || [];
  for (var i=0;i<reg.length;i++){ if (reg[i] && reg[i].name === NAME){ tool = reg[i]; break; } }
  if (!tool) {
    try {
      var mc = navigator.modelContext;
      var nt = mc && (mc.tools || (typeof mc.getTools === 'function' ? mc.getTools() : null));
      if (nt) for (var j=0;j<nt.length;j++){ if (nt[j] && nt[j].name === NAME){ tool = nt[j]; break; } }
    } catch (_) {}
  }
  if (!tool) return { ok:false, error: 'page tool not found: ' + NAME + ' (call brw_page_tools to list available tools)' };
  var fn = tool.execute || tool.call || tool.run || tool.handler;
  if (typeof fn !== 'function') return { ok:false, error: 'page tool has no callable execute: ' + NAME };
  try { return { ok:true, result: await fn.call(tool, ARGS) }; }
  catch (e) { return { ok:false, error: String(e && e.message || e) }; }
})()`, string(nameJSON), argsRaw)
}

// RegisterWebMCPOnNewDocument arms the WebMCP shim to install at document-start
// on every future navigation (direct-CDP only) so it captures tool registrations
// before the page's own scripts run. Call once per tab.
func RegisterWebMCPOnNewDocument(ctx context.Context) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(WebMCPInstallScript).Do(ctx)
		return err
	}))
}
