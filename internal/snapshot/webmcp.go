package snapshot

import (
	"context"

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

// BuildPageToolsExpression (webmcp_invoke.go) lists what a document exposes and
// BuildPageToolInvokeExpression starts one of its tools; both read the registry
// this shim fills as well as a native navigator.modelContext.

// RegisterWebMCPOnNewDocument arms the WebMCP shim to install at document-start
// on every future navigation (direct-CDP only) so it captures tool registrations
// before the page's own scripts run. Call once per tab.
func RegisterWebMCPOnNewDocument(ctx context.Context) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(WebMCPInstallScript).Do(ctx)
		return err
	}))
}
