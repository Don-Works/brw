package mcp

import (
	"context"
	"encoding/json"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/devtools"
)

// devtoolsObserver resolves the optional observation capability. A transport
// without it answers with a named capability error rather than an empty page
// report that reads like a clean result.
func (s *Server) devtoolsObserver() (devtools.Observer, bool) {
	observer, ok := s.manager.(devtools.Observer)
	return observer, ok
}

func (s *Server) callVitals(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	observer, ok := s.devtoolsObserver()
	if !ok {
		return toolError(devtools.ErrUnsupported), nil
	}
	var req devtools.VitalsOptions
	if err := unmarshalArgs(args, &req); err != nil {
		return nil, invalid(err)
	}
	return toolJSON(observer.Vitals(ctx, req))
}

func (s *Server) callAccessibilityAudit(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	observer, ok := s.devtoolsObserver()
	if !ok {
		return toolError(devtools.ErrUnsupported), nil
	}
	var req devtools.AuditOptions
	if err := unmarshalArgs(args, &req); err != nil {
		return nil, invalid(err)
	}
	result, err := observer.AccessibilityAudit(ctx, req)
	if err != nil {
		return toolError(err), nil
	}
	return toolJSON(artifact.AttachAuditReport(ctx, s.artifactService(), result), nil)
}

func (s *Server) callHighlight(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	observer, ok := s.devtoolsObserver()
	if !ok {
		return toolError(devtools.ErrUnsupported), nil
	}
	var req devtools.HighlightOptions
	if err := unmarshalArgs(args, &req); err != nil {
		return nil, invalid(err)
	}
	return toolJSON(observer.Highlight(ctx, req))
}

// devtoolsTools are the catalogue entries for the three observations. They live
// beside their handlers rather than in the 2.6k-line catalogue so a reader can
// see what a tool promises and what it does in one place.
func devtoolsTools() []map[string]any {
	return []map[string]any{
		tool("brw_vitals", "Read the Core Web Vitals for the page currently loaded in the tab: LCP (largest contentful paint, ms), CLS (cumulative layout shift, unitless), INP (interaction to next paint, ms), TTFB (time to first byte, ms) and FCP, plus DOMContentLoaded, load and the navigation type. Each metric also comes back with a good/needs-improvement/poor rating against the published thresholds, and lcp_element names the block that painted last (with its brw ref when it has one) so you know what to make faster. This is a pure read — it registers performance observers, drains the buffered timeline and disconnects them, leaving nothing in the page. Works on a page brw did not open, because the browser buffers these entries from navigation start. Two honest limits: LCP is provisional until the first user interaction, and INP is null until something has been interacted with — the browser only retains interactions of about 104ms or slower, so a page whose interactions were all fast reports none rather than a small number. Raise settle_ms if a slow page reports null metrics you expected; it is capped at 5000.", object(map[string]any{
			"settle_ms": integerSchema("How long, in ms, to let the buffered performance timeline drain before answering. Defaults to 250 and is capped at 5000. Raise it on a page that is still loading."),
			"tab_id":    stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_a11y_audit", "Run a full axe-core accessibility audit against the page and return a bounded summary of what FAILED, worst impact first. Each failing rule comes back with its axe rule id, impact (critical/serious/moderate/minor), the plain-English help text, a help URL, how many elements failed it, and brw refs for the offending elements — pass those straight to brw_highlight, brw_click or brw_snapshot rather than re-resolving a CSS selector. The complete axe document, including every failing node and the incomplete (needs-review) results, is written to an artifact: read it with brw_artifact_read or search it with brw_artifact_search using the artifact_id in the answer. Do not expect the whole report inline; that is what the artifact is for. The engine is embedded in the brw binary and injected from there, so the audit fetches nothing over the network and works offline and on a locked-down origin. Scope a re-check after a fix with rules:[\"color-contrast\"], or a conformance pass with tags:[\"wcag2aa\"]. Its one effect on the page is the data-brw-ref attribute brw_snapshot already writes, stamped on the elements that failed so they have refs to return.", object(map[string]any{
			"tags":           map[string]any{"type": "array", "description": "Limit the run to axe rules carrying one of these tags, e.g. wcag2a, wcag2aa, wcag21aa, wcag22aa, best-practice. Omit to run every rule axe ships.", "items": map[string]any{"type": "string"}},
			"rules":          map[string]any{"type": "array", "description": "Limit the run to these exact axe rule ids, e.g. [\"color-contrast\"]. Use it to re-check one finding after a fix instead of paying for a whole audit. Takes precedence over tags.", "items": map[string]any{"type": "string"}},
			"include_passes": boolSchema("Keep the passing elements in the stored artifact. Off by default because on a real page they outweigh everything else; the pass COUNT is always reported either way."),
			"max_rules":      integerSchema("How many failing rules to describe in the summary. Defaults to 10, capped at 50. The totals and the artifact always cover everything; truncated says when the list was cut."),
			"max_refs":       integerSchema("How many offending element refs to list per rule. Defaults to 3, capped at 20."),
			"tab_id":         stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_highlight", "Draw a coloured outline around one or more elements so a HUMAN watching the browser can see what you are talking about — reviewing an accessibility finding, pointing at the field you are about to fill, confirming a ref before a destructive click. This is the one developer tool here that changes the page, and the change is deliberately confined and undoable: a single <div id=\"__brw_highlight_overlay\"> is appended to the document with pointer-events:none, the target elements themselves are never restyled or moved, and brw_highlight with clear:true removes it. It also disappears on its own if you pass duration_ms, and on the next navigation. Nothing about the page's own behaviour changes, so a highlight left up will not break a later click. Pass ref for one element or refs for several (up to 12); the answer reports, per ref, whether it resolved and whether it is currently on screen. Highlighting does NOT scroll by default, because moving someone's page is a side effect a read-shaped call should not take uninvited — pass scroll:true when you want the first element brought into view.", object(map[string]any{
			"ref":         stringSchema("Ref of the element to mark, from brw_snapshot, brw_find or brw_a11y_audit."),
			"refs":        map[string]any{"type": "array", "description": "Refs to mark together, up to 12. Combined with ref if both are given.", "items": map[string]any{"type": "string"}},
			"clear":       boolSchema("Remove every brw highlight from the page and draw nothing. This is the undo."),
			"label":       stringSchema("Short caption drawn above the first box, for a human reading the screen. Keep it free of secrets; it is visible in the page."),
			"color":       stringEnumSchema("Outline colour. Defaults to red.", devtools.HighlightColorNames()...),
			"duration_ms": integerSchema("Remove the overlay automatically after this many ms. Defaults to 0, which leaves it until a clear call or the next navigation. Capped at 300000."),
			"scroll":      boolSchema("Scroll the first matched element into view. Off by default: it moves the page."),
			"tab_id":      stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
	}
}
