package devtools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Don-Works/brw/internal/snapshot"
)

// MaxHighlightRefs bounds one call. A highlight is something a human looks at;
// past a dozen boxes the screen says nothing.
const MaxHighlightRefs = 12

// MaxHighlightDurationMS caps the auto-clear timer. An overlay that outlives
// the session it was drawn for is litter in someone's browser.
const MaxHighlightDurationMS = 300000

// MaxHighlightLabelBytes bounds the caption. It is drawn on one line above the
// first box, so a caption longer than this is unreadable on screen anyway.
const MaxHighlightLabelBytes = 120

// highlightColors is a closed set because the value is written into an inline
// style. A caller-supplied colour string would be caller-supplied CSS.
var highlightColors = map[string]string{
	"red":    "#e5484d",
	"orange": "#f76b15",
	"yellow": "#ffb224",
	"green":  "#30a46c",
	"blue":   "#0091ff",
	"purple": "#8e4ec6",
}

// HighlightColorNames lists the accepted colours in a stable order, for the
// tool schema and the error message.
func HighlightColorNames() []string {
	names := make([]string, 0, len(highlightColors))
	for name := range highlightColors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type HighlightOptions struct {
	// Ref and Refs are the same field for a caller with one element or several.
	Ref  string   `json:"ref,omitempty"`
	Refs []string `json:"refs,omitempty"`
	// Clear removes every brw highlight from the page and draws nothing.
	Clear bool   `json:"clear,omitempty"`
	Label string `json:"label,omitempty"`
	Color string `json:"color,omitempty"`
	// DurationMS auto-clears the overlay after this long. Zero leaves it until
	// a clear call or the next navigation.
	DurationMS int `json:"duration_ms,omitempty"`
	// Scroll brings the first matched element into view. Off by default: moving
	// the page is a side effect a read-shaped tool should not take uninvited.
	Scroll bool   `json:"scroll,omitempty"`
	TabID  string `json:"tab_id,omitempty"`
}

// Normalize folds ref into refs, applies defaults and rejects what the script
// must not be handed.
func (o HighlightOptions) Normalize() (HighlightOptions, error) {
	refs := make([]string, 0, len(o.Refs)+1)
	seen := map[string]bool{}
	for _, ref := range append([]string{o.Ref}, o.Refs...) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	o.Ref, o.Refs = "", refs
	if !o.Clear && len(refs) == 0 {
		return o, fmt.Errorf("highlight needs at least one ref, or clear:true")
	}
	if len(refs) > MaxHighlightRefs {
		return o, fmt.Errorf("highlight accepts at most %d refs, got %d", MaxHighlightRefs, len(refs))
	}
	color := strings.ToLower(strings.TrimSpace(o.Color))
	if color == "" {
		color = "red"
	}
	if _, ok := highlightColors[color]; !ok {
		return o, fmt.Errorf("unknown highlight colour %q: want one of %s", o.Color, strings.Join(HighlightColorNames(), ", "))
	}
	o.Color = color
	if o.DurationMS < 0 {
		o.DurationMS = 0
	}
	if o.DurationMS > MaxHighlightDurationMS {
		o.DurationMS = MaxHighlightDurationMS
	}
	o.Label = boundedText(o.Label, MaxHighlightLabelBytes)
	return o, nil
}

// HighlightedRef is what happened to one requested ref.
type HighlightedRef struct {
	Ref        string  `json:"ref"`
	Found      bool    `json:"found"`
	InViewport bool    `json:"in_viewport"`
	X          float64 `json:"x,omitempty"`
	Y          float64 `json:"y,omitempty"`
	Width      float64 `json:"width,omitempty"`
	Height     float64 `json:"height,omitempty"`
}

type HighlightResult struct {
	OK      bool             `json:"ok"`
	Cleared bool             `json:"cleared,omitempty"`
	Marked  []HighlightedRef `json:"marked,omitempty"`
	// Active is how many boxes the overlay is now drawing.
	Active     int    `json:"active"`
	ExpiresMS  int    `json:"expires_in_ms,omitempty"`
	Reversible string `json:"reversible"`
	Note       string `json:"note,omitempty"`
}

// BuildHighlightExpression renders the overlay call for one already-normalized
// options value.
func BuildHighlightExpression(opts HighlightOptions) string {
	args, _ := json.Marshal(map[string]any{
		"refs":        opts.Refs,
		"clear":       opts.Clear,
		"label":       opts.Label,
		"color":       highlightColors[opts.Color],
		"duration_ms": opts.DurationMS,
		"scroll":      opts.Scroll,
	})
	return fmt.Sprintf("%s(%s)", HighlightScript, args)
}

// HighlightHostID is the id of the single element the overlay adds to a page.
// It is named here so the tool description, the script and a test all say the
// same thing about what is added and what a clear removes.
const HighlightHostID = "__brw_highlight_overlay"

// HighlightScript draws a pointer-events-none overlay over one or more refs.
//
// It is the one observation in this package that changes what the page looks
// like, so the change is confined and undoable: a single fixed-position container with a
// reserved id is appended to the top document, the target elements are never
// touched, and a clear (or the duration timer, or any navigation) removes the
// container and the two listeners that keep it aligned. Nothing else in the
// page is read, moved or restyled.
const HighlightScript = `(function(opts) {` + snapshot.FrameWalkHelpers + `
  var HOST_ID = '` + HighlightHostID + `';
  var state = window.__brwHighlight || null;

  function teardown() {
    var removed = false;
    if (state) {
      try { if (state.timer) clearTimeout(state.timer); } catch (_) {}
      try { window.removeEventListener('scroll', state.reposition, true); } catch (_) {}
      try { window.removeEventListener('resize', state.reposition, true); } catch (_) {}
      state = null;
      window.__brwHighlight = null;
    }
    // Remove by id as well as by state: a reload drops the state object while
    // a bfcache restore can bring the node back, and an orphaned box that no
    // clear can reach is exactly the litter this tool must not leave.
    for (;;) {
      var host = document.getElementById(HOST_ID);
      if (!host) break;
      try { host.remove(); } catch (_) { break; }
      removed = true;
    }
    return removed;
  }

  function boxFor(ref) {
    var hit = null;
    try { hit = __abFindDeep(ref); } catch (_) { hit = null; }
    if (!hit || !hit.el) return { ref: ref, found: false, in_viewport: false };
    var r = hit.el.getBoundingClientRect();
    var left = r.left + hit.ox;
    var top = r.top + hit.oy;
    return {
      ref: ref,
      found: true,
      in_viewport: r.width > 0 && r.height > 0 &&
        top + r.height > 0 && left + r.width > 0 &&
        top < (window.innerHeight || 0) && left < (window.innerWidth || 0),
      x: Math.round(left * 10) / 10,
      y: Math.round(top * 10) / 10,
      width: Math.round(r.width * 10) / 10,
      height: Math.round(r.height * 10) / 10,
      el: hit.el
    };
  }

  if (opts.clear) {
    var cleared = teardown();
    return { ok: true, cleared: cleared, active: 0,
      reversible: 'nothing added; the overlay element and its listeners are gone' };
  }

  teardown();
  var host = document.createElement('div');
  host.id = HOST_ID;
  host.setAttribute('aria-hidden', 'true');
  host.style.cssText = 'position:fixed;left:0;top:0;width:0;height:0;margin:0;padding:0;border:0;' +
    'pointer-events:none;z-index:2147483647;';
  var marked = [];
  var boxes = [];
  for (var i = 0; i < opts.refs.length; i++) {
    var box = boxFor(opts.refs[i]);
    if (!box.found) { marked.push(box); continue; }
    var mark = document.createElement('div');
    mark.style.cssText = 'position:fixed;box-sizing:border-box;pointer-events:none;' +
      'border:2px solid ' + opts.color + ';border-radius:3px;' +
      'box-shadow:0 0 0 2px rgba(255,255,255,0.7);';
    host.appendChild(mark);
    boxes.push({ ref: box.ref, el: box.el, mark: mark });
    if (opts.label && boxes.length === 1) {
      var tag = document.createElement('div');
      tag.textContent = opts.label;
      tag.style.cssText = 'position:fixed;pointer-events:none;font:600 11px/1.4 system-ui,sans-serif;' +
        'color:#fff;background:' + opts.color + ';padding:1px 5px;border-radius:3px;white-space:nowrap;';
      host.appendChild(tag);
      boxes[0].tag = tag;
    }
    delete box.el;
    marked.push(box);
  }

  function reposition() {
    for (var j = 0; j < boxes.length; j++) {
      var entry = boxes[j];
      var rect;
      try { rect = entry.el.getBoundingClientRect(); } catch (_) { continue; }
      var offset = { ox: 0, oy: 0 };
      try {
        var again = __abFindDeep(entry.ref);
        if (again) offset = { ox: again.ox, oy: again.oy };
      } catch (_) {}
      entry.mark.style.left = (rect.left + offset.ox) + 'px';
      entry.mark.style.top = (rect.top + offset.oy) + 'px';
      entry.mark.style.width = rect.width + 'px';
      entry.mark.style.height = rect.height + 'px';
      if (entry.tag) {
        entry.tag.style.left = (rect.left + offset.ox) + 'px';
        entry.tag.style.top = Math.max(0, rect.top + offset.oy - 18) + 'px';
      }
    }
  }

  if (boxes.length) {
    if (opts.scroll) {
      try { boxes[0].el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' }); } catch (_) {}
    }
    (document.body || document.documentElement).appendChild(host);
    reposition();
    state = { reposition: reposition, timer: null };
    window.__brwHighlight = state;
    window.addEventListener('scroll', reposition, true);
    window.addEventListener('resize', reposition, true);
    if (opts.duration_ms > 0) state.timer = setTimeout(teardown, opts.duration_ms);
    // Re-read the boxes after the append: scroll:true moves the page, so the
    // coordinates measured before it would describe where the element was.
    for (var k = 0; k < marked.length; k++) {
      if (!marked[k].found) continue;
      var fresh = boxFor(marked[k].ref);
      delete fresh.el;
      marked[k] = fresh;
    }
  }

  return {
    ok: boxes.length > 0,
    marked: marked,
    active: boxes.length,
    expires_in_ms: boxes.length && opts.duration_ms > 0 ? opts.duration_ms : 0,
    reversible: 'one <div id="' + HOST_ID + '"> was added to the page; brw_highlight clear:true removes it and the target elements are unchanged',
    note: boxes.length ? '' : 'no ref resolved; nothing was drawn'
  };
})`
