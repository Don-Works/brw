package snapshot

import (
	"context"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// ShadowPierceInstallScript monkeypatches Element.prototype.attachShadow so that
// CLOSED shadow roots keep a side reference (__brwShadow) on their host element,
// letting the snapshot/resolve walkers descend into otherwise-opaque custom web
// components (design systems built on closed roots — Shoelace, parts of MUI,
// etc.). The page's own el.shadowRoot stays null, so encapsulation is preserved
// for page scripts; only the brw walker reads __brwShadow.
//
// It only captures roots attached AFTER it runs. Registered at document-start via
// RegisterShadowPierceOnNewDocument it beats the page's own scripts and catches
// every closed root; the equivalent in-walker installer (__abEnsureShadowPierce
// in FrameWalkHelpers) is the transport-agnostic fallback that catches roots
// mounted after the first snapshot/click. Keep this logic in sync with
// __abEnsureShadowPierce. Idempotent + page-tamper-safe via the window guard.
const ShadowPierceInstallScript = `(function(){
  if (window.__brwShadowPierce) return;
  window.__brwShadowPierce = true;
  try {
    var o = Element.prototype.attachShadow;
    if (typeof o !== 'function') return;
    Element.prototype.attachShadow = function(init){
      var r = o.call(this, init);
      try { if (init && init.mode === 'closed') Object.defineProperty(this, '__brwShadow', { value: r, configurable: true }); } catch (_) {}
      return r;
    };
  } catch (_) {}
})()`

// RegisterShadowPierceOnNewDocument arms the closed-shadow piercer to install at
// document-start on every future navigation/reload via
// Page.addScriptToEvaluateOnNewDocument, so it runs before the page's own scripts
// and captures closed roots mounted during initial load. Direct-CDP transport
// only (the extension bridge has no CDP document-start hook); the in-walker
// installer keeps the bridge covered for post-load roots. Call once per tab; the
// install script's window guard keeps it idempotent against the in-walker path.
func RegisterShadowPierceOnNewDocument(ctx context.Context) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(ShadowPierceInstallScript).Do(ctx)
		return err
	}))
}
