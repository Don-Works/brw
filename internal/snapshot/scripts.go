package snapshot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// snapshotInstallTarget is the window property the DOM walker is cached under so
// it ships once per document instead of once per call. The name carries a
// per-process random token so a page cannot predefine, shadow, or spoof the
// snapshot entry point (it would have to guess the token), and the install path
// overwrites it unconditionally so a hostile/colliding page value cannot wedge
// snapshots. Stable for the process so the fast path keeps hitting.
var snapshotInstallTarget = "window.__brw_snap_" + randomToken()

func randomToken() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// RNG failure is effectively impossible; fall back to a fixed suffix so
		// the property name stays a valid identifier.
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}

const SnapshotFunctionScript = `(function(opts) {` + FrameWalkHelpers + `
  opts = opts || {};
  // Arm the __abRoots memo for this single synchronous walk: the DOM cannot mutate
  // mid-call, so the many root lookups below safely share one frame walk.
  __abRootsCacheArmed = true;
  const state = window.__brw || (window.__brw = { next: 1, byKey: {}, byRef: {} });
  const includeHidden = Boolean(opts.include_hidden);
  const textContent = Boolean(opts.text_content);
  const selectorParts = [
    'a[href]',
    'button',
    'input:not([type="hidden"])',
    'textarea',
    'select',
    '[role]',
    '[aria-live]',
    '[aria-invalid="true"]',
    '[contenteditable="true"]',
    '[tabindex]',
    'summary',
    'label',
    'img[alt]',
    '[title]',
    '[aria-label]',
    '[draggable="true"]'
  ];
  if (includeHidden) selectorParts.push('input[type="hidden"]');
  // When text_content matching is requested, also capture common prose-bearing
  // elements (headings, paragraphs, list items, etc.) so visible page text — not
  // just interactive-element metadata — becomes searchable. These extra elements
  // are still subject to the usefulness filter unless they actually match the
  // query/text, so they don't bloat unfiltered output.
  if (textContent) {
    selectorParts.push('h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'p', 'li', 'dt', 'dd', 'td', 'th', 'blockquote', 'figcaption', 'caption');
  }
  const selector = selectorParts.join(',');

  function clean(s) {
    return String(s || '').replace(/\s+/g, ' ').trim();
  }

  function roots() {
    return __abRootList();
  }

  function all(selector) {
    const out = [];
    for (const root of roots()) {
      if (root.querySelectorAll) out.push(...Array.from(root.querySelectorAll(selector)));
    }
    return out;
  }

  function winFor(el) {
    var doc = el && el.ownerDocument;
    return (doc && doc.defaultView) || window;
  }

  // isElementLike performs a realm-agnostic instanceof: elements inside an iframe
  // belong to that frame's realm, so a top-window 'el instanceof HTMLElement'
  // check returns false for them. Test against the element's OWN window
  // constructors (falling back to nodeType for safety).
  function isElementLike(el) {
    if (!el) return false;
    var w = winFor(el);
    if (w && (w.HTMLElement && el instanceof w.HTMLElement)) return true;
    if (w && (w.SVGElement && el instanceof w.SVGElement)) return true;
    return el.nodeType === 1;
  }

  function visible(el) {
    if (!el || el.nodeType !== 1) return false;
    if (el.closest('[hidden],[aria-hidden="true"]')) return false;
    const style = winFor(el).getComputedStyle(el);
    if (!style || style.display === 'none' || style.visibility === 'hidden' || Number(style.opacity) === 0) return false;
    const rects = el.getClientRects();
    return rects && rects.length > 0 && Array.from(rects).some(r => r.width > 0 && r.height > 0);
  }

  function inViewport(el) {
    const w = winFor(el);
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && r.bottom >= 0 && r.right >= 0 && r.top <= w.innerHeight && r.left <= w.innerWidth;
  }

  function labelText(el) {
    if (!el) return '';
    if (el.labels && el.labels.length) return clean(Array.from(el.labels).map(l => l.innerText || l.textContent).join(' '));
    if (el.id) {
      const root = el.getRootNode && el.getRootNode();
      const labelRoot = root && root.querySelector ? root : document;
      const label = labelRoot.querySelector('label[for="' + CSS.escape(el.id) + '"]');
      if (label) return clean(label.innerText || label.textContent);
    }
    const parent = el.closest('label');
    if (parent) return clean(parent.innerText || parent.textContent);
    return '';
  }

  function sensitive(el) {
    if (!el || !el.tagName) return false;
    const tag = el.tagName.toLowerCase();
    if (tag !== 'input' && tag !== 'textarea' && tag !== 'select') return false;
    const type = (el.getAttribute('type') || '').toLowerCase();
    if (type === 'password' || type === 'hidden') return true;
    const ac = (el.getAttribute('autocomplete') || '').toLowerCase();
    const sensitiveHints = ['current-password', 'new-password', 'one-time-code', 'cc-number', 'cc-csc', 'cc-exp', 'cc-exp-month', 'cc-exp-year', 'cc-name', 'cc-type', 'cc-given-name', 'cc-family-name', 'cc-number'];
    for (const hint of sensitiveHints) {
      if (ac.includes(hint)) return true;
    }
    return false;
  }

  function textFor(el) {
    const tag = el.tagName.toLowerCase();
    if (tag === 'input') return clean(el.value || el.getAttribute('value') || '');
    return clean(el.innerText || el.textContent || '');
  }

  function nameFor(el) {
    return clean(
      el.getAttribute('aria-label') ||
      labelText(el) ||
      el.getAttribute('alt') ||
      el.getAttribute('title') ||
      el.getAttribute('placeholder') ||
      el.getAttribute('name') ||
      (sensitive(el) ? '' : textFor(el))
    );
  }

  function roleFor(el) {
    const explicit = clean(el.getAttribute('role'));
    if (explicit) return explicit.split(/\s+/)[0];
    const tag = el.tagName.toLowerCase();
    const type = (el.getAttribute('type') || '').toLowerCase();
    if (tag === 'a' && el.href) return 'link';
    if (tag === 'button' || type === 'button' || type === 'submit' || type === 'reset' || type === 'image') return 'button';
    if (tag === 'textarea' || el.isContentEditable) return 'textbox';
    if (tag === 'select') return el.multiple ? 'listbox' : 'combobox';
    if (tag === 'input') {
      if (type === 'hidden') return 'hidden';
      if (type === 'checkbox') return 'checkbox';
      if (type === 'radio') return 'radio';
      if (type === 'range') return 'slider';
      if (type === 'number') return 'spinbutton';
      if (type === 'search') return 'searchbox';
      return 'textbox';
    }
    if (tag === 'summary') return 'button';
    if (tag === 'label') return 'label';
    if (tag === 'img') return 'image';
    return 'generic';
  }

  function pathFor(el) {
    const parts = [];
    let n = el;
    while (n && n.nodeType === Node.ELEMENT_NODE && n !== document.documentElement) {
      const tag = n.tagName.toLowerCase();
      let idx = 1;
      let p = n.previousElementSibling;
      while (p) {
        if (p.tagName === n.tagName) idx++;
        p = p.previousElementSibling;
      }
      parts.push(tag + ':nth-of-type(' + idx + ')');
      n = n.parentElement;
      if (parts.length > 8) break;
    }
    return parts.reverse().join('>');
  }

  // keyFor builds the CANONICAL key in the legacy positional layout
  // (tag|role|name|id|name_attr|href|type|aria-controls|path). This layout is
  // load-bearing: the ref-recovery paths split on '|' and read positional
  // indices (parts[0]=tag, parts[1]=role, parts[2]=name, parts[6]=type), so the
  // stored key must keep this shape. It is what gets written to state.byRef[ref].
  function keyFor(el, role, name) {
    const tag = el.tagName.toLowerCase();
    const stable = [
      tag,
      role,
      name,
      el.id || '',
      el.getAttribute('name') || '',
      el.getAttribute('href') || '',
      el.getAttribute('type') || '',
      el.getAttribute('aria-controls') || '',
      pathFor(el)
    ];
    return stable.join('|');
  }

  // stableKeyFor builds a render-stable identity that excludes the two mutable
  // inputs that cause refs to renumber on SPA re-renders: innerText-derived name
  // (button/heading/list text changes on re-render) and pathFor() (sibling
  // reorder / conditional mount shifts nth-of-type indices). It is composed only
  // from attributes that survive re-render. To avoid COLLAPSING genuinely
  // distinct siblings that share tag+role and have no distinguishing attribute
  // (e.g. a list of bare <button> with no id/name/href/aria), it returns '' —
  // meaning "no reliable stable identity" — unless the element carries at least
  // one strong identity attribute (id, name, href, or aria-label). When it
  // returns '', refFor falls back to the legacy key, preserving today's behavior
  // exactly for those elements.
  function stableKeyFor(el, role) {
    const id = el.id || '';
    const nameAttr = el.getAttribute('name') || '';
    const href = el.getAttribute('href') || '';
    const ariaLabel = el.getAttribute('aria-label') || '';
    if (!id && !nameAttr && !href && !ariaLabel) return '';
    const tag = el.tagName.toLowerCase();
    return [
      'S',
      tag,
      role,
      id,
      nameAttr,
      href,
      el.getAttribute('type') || '',
      ariaLabel,
      el.getAttribute('aria-controls') || '',
      el.getAttribute('aria-describedby') || ''
    ].join('|');
  }

  // refFor resolves an element to a persistent ref. Lookup order:
  //   1. an already-stamped data-brw-ref attribute (authoritative),
  //   2. the render-stable key (new; survives SPA re-renders),
  //   3. the legacy key (backward compat with refs assigned before this change).
  // The canonical legacy key is always stored in state.byRef[ref] so recovery
  // keeps working; the stable key is also indexed in state.byKey when present.
  function refFor(el, key, stableKey) {
    const existing = el.getAttribute('data-brw-ref');
    if (existing) {
      state.byRef[existing] = key;
      state.byKey[key] = existing;
      if (stableKey) state.byKey[stableKey] = existing;
      return existing;
    }
    const prior = (stableKey && state.byKey[stableKey]) || state.byKey[key];
    const ref = prior || ('e' + state.next++);
    state.byKey[key] = ref;
    if (stableKey) state.byKey[stableKey] = ref;
    state.byRef[ref] = key;
    try { el.setAttribute('data-brw-ref', ref); } catch (_) {}
    return ref;
  }

  function disabled(el) {
    return Boolean(el.disabled || el.getAttribute('aria-disabled') === 'true');
  }

  // islandScore ranks a visual island by size and viewport intersection so the
  // most significant painted regions surface first and small/offscreen ones drop
  // out under the cap. Returns 0 when nothing intersects the viewport.
  function islandScore(rect, area) {
    if (area <= 0) return 0;
    var visibleArea = Math.max(0, Math.min(rect.right, window.innerWidth) - Math.max(rect.left, 0)) *
                      Math.max(0, Math.min(rect.bottom, window.innerHeight) - Math.max(rect.top, 0));
    if (visibleArea === 0) return 0;
    var score = Math.min(area / 10000, 500); // base score by size, normalized to 10kpx units
    score += visibleArea / 5000;             // boost for viewport intersection
    if (rect.left < window.innerWidth * 0.1 && rect.top < window.innerHeight * 0.2) {
      score += 100; // boost above-fold, left-aligned (typical primary content)
    }
    return score;
  }

  // detectVisualIslands finds semantically-opaque visual content the DOM walker
  // skips: canvas/svg/video tags, large images, large background-image boxes with
  // no text/interactive children, and conservatively-detected custom-rendered
  // widgets. Each island scores by islandScore(); the top N (default 10) survive.
  // Returns [{ el, visualType, score, visualHint }] sorted by score desc.
  function detectVisualIslands(opts) {
    var islands = [];
    var visualSelector = 'canvas, svg, video, img';
    var seen = new Set();

    // 1. tag-based visual elements (canvas / svg / video / large img)
    for (var el of all(visualSelector)) {
      if (!isElementLike(el) || seen.has(el)) continue;
      if (!visible(el) || !inViewport(el)) continue;
      var tag = el.tagName.toLowerCase();
      var r = el.getBoundingClientRect();
      var area = r.width * r.height;
      var hint = clean(el.getAttribute('alt') || el.getAttribute('aria-label') || el.getAttribute('title') || '');
      var visualType = null;
      var score = 0;
      if (tag === 'canvas') { visualType = 'canvas'; score = islandScore(r, area); }
      else if (tag === 'svg') { visualType = 'svg'; score = islandScore(r, area); }
      else if (tag === 'video') { visualType = 'video'; score = islandScore(r, area); }
      else if (tag === 'img') {
        if (area > 50000) { visualType = 'image'; score = islandScore(r, area); }
        else continue; // skip thumbnail bloat
      }
      if (visualType && score > 0) {
        islands.push({ el: el, visualType: visualType, score: score, visualHint: hint });
        seen.add(el);
      }
    }

    // 2. background-image elements: large box, no text, no interactive children
    for (var el2 of all('*')) {
      if (seen.has(el2) || !isElementLike(el2) || !visible(el2) || !inViewport(el2)) continue;
      var style = winFor(el2).getComputedStyle(el2);
      if (!style) continue;
      var bgImage = style.backgroundImage;
      if (!bgImage || bgImage === 'none') continue;
      var r2 = el2.getBoundingClientRect();
      var area2 = r2.width * r2.height;
      if (area2 < 50000) continue;
      var hasText = clean(el2.innerText || '').length > 10;
      var hasInteractive = el2.querySelector('button, a[href], input, [role=button]');
      if (hasText || hasInteractive) continue;
      var score2 = islandScore(r2, area2);
      if (score2 > 0) {
        var hint2 = clean(el2.getAttribute('aria-label') || el2.getAttribute('title') || '');
        islands.push({ el: el2, visualType: 'bg_image', score: score2, visualHint: hint2 });
        seen.add(el2);
      }
    }

    // 3. custom-rendered widgets: large, styled, no semantic role, minimal
    // children/text. Conservative — scoped to inline-styled boxes only.
    for (var el3 of all('[style*="background"], [style*="border"], [style*="shadow"]')) {
      if (seen.has(el3) || !isElementLike(el3) || !visible(el3) || !inViewport(el3)) continue;
      var r3 = el3.getBoundingClientRect();
      var area3 = r3.width * r3.height;
      if (area3 < 20000) continue;
      var roleAttr = el3.getAttribute('role');
      var hasRole = roleAttr && roleAttr !== 'generic';
      var hasChildren = el3.querySelector('*');
      var hasText3 = clean(el3.innerText || '').length > 5;
      if (!hasRole && (!hasChildren || !hasText3)) {
        var style3 = winFor(el3).getComputedStyle(el3);
        var isStyledBox = (style3.backgroundColor && style3.backgroundColor !== 'rgba(0, 0, 0, 0)') ||
                          (style3.borderWidth && style3.borderWidth !== '0px') ||
                          (style3.boxShadow && style3.boxShadow !== 'none');
        if (isStyledBox) {
          var score3 = islandScore(r3, area3) * 0.8; // less certain — reduce
          if (score3 > 0) {
            var hint3 = clean(el3.getAttribute('aria-label') || el3.getAttribute('title') || '');
            islands.push({ el: el3, visualType: 'custom', score: score3, visualHint: hint3 });
            seen.add(el3);
          }
        }
      }
    }

    islands.sort(function(a, b) { return b.score - a.score; });
    var ilimit = Number(opts.visual_islands_limit || 0) || 10;
    return islands.slice(0, ilimit);
  }

	function structuralSignals(el, role, active) {
		const signals = [];
		const tag = el.tagName.toLowerCase();
		if (active === el) signals.push('focused');
    else if (active && el.contains && el.contains(active)) signals.push('focus-within');
    if (el.getAttribute('aria-expanded') === 'true') signals.push('expanded');
    if (el.getAttribute('aria-haspopup')) signals.push('has-popup');
    if (el.getAttribute('aria-controls')) signals.push('controls');
    if (el.getAttribute('aria-activedescendant')) signals.push('active-descendant-owner');
    if (el.id && active && active.getAttribute && active.getAttribute('aria-activedescendant') === el.id) {
      signals.push('active-descendant');
    }
    if (el.matches && el.matches(':invalid')) signals.push('invalid');
    if (el.getAttribute('aria-invalid') === 'true') signals.push('invalid');
    if (el.required || el.getAttribute('aria-required') === 'true') signals.push('required');
    if (el.getAttribute('aria-live') && el.getAttribute('aria-live') !== 'off') signals.push('live');
    if (['dialog', 'alertdialog', 'alert', 'status', 'log', 'listbox', 'menu', 'menuitem', 'option'].includes(role)) {
      signals.push('frontier-role');
    }
    if (tag === 'canvas' || tag === 'svg' || tag === 'video' || tag === 'iframe' || tag === 'embed' || tag === 'object') {
      signals.push('visual-island');
    }
    if (tag === 'img' && el.width > 100 && el.height > 100) {
      signals.push('visual-island');
    }
    return signals;
  }

  function deepActive(root) {
    let active = root && root.activeElement;
    while (active && active.shadowRoot && active.shadowRoot.activeElement) {
      active = active.shadowRoot.activeElement;
    }
    return active;
  }

  const query = clean(opts.query || '').toLowerCase();
  const textFilter = clean(opts.text || '').toLowerCase();
  const roleFilter = clean(opts.role || '').toLowerCase();
  const mode = clean(opts.mode || '').toLowerCase();
  const frontierMode = mode === 'frontier';
  const formLensMode = mode === 'form_lens';
  // modeTag is the full 3-way mode tag used in BOTH the since-options cache
  // signature and the output metadata. It must not collapse form_lens into
  // 'all': form_lens emits a different element set (form roles only) plus
  // validity, so sharing a signature with 'all' would let the since-delta key
  // treat an 'all' snapshot and a form_lens snapshot as identical options and
  // return a stale delta.
  const modeTag = frontierMode ? 'frontier' : (formLensMode ? 'form_lens' : 'all');
  const viewportOnly = Boolean(opts.viewport_only) || frontierMode;
  const limit = Math.max(0, Number(opts.limit || 0));
  const active = deepActive(document);

  const formRoles = new Set(['textbox', 'searchbox', 'combobox', 'listbox', 'checkbox', 'radio', 'slider', 'spinbutton', 'button', 'switch', 'option', 'menuitem']);
  const seen = new Set();
  const elements = [];
  const elByIndex = new Map();
  for (const el of all(selector)) {
    if (!isElementLike(el)) continue;
    if (seen.has(el)) continue;
    seen.add(el);
    const role = roleFor(el);
    // Salient-image gate: <img> is surfaced as role "image" so agents can target
    // it for hover/click/drag (e.g. hover-reveal avatars, product tiles, map pins)
    // — a class a real accessibility tree exposes and we previously dropped. Bound
    // the noise: keep only visible images with a meaningful box; a named (alt)
    // image clears a lower bar than an unnamed one, so icons/spacers/tracking
    // pixels don't flood the frontier on image-heavy pages.
    if (role === 'image') {
      const ir = el.getBoundingClientRect();
      const iarea = ir.width * ir.height;
      const inamed = !!clean(el.getAttribute('alt') || el.getAttribute('aria-label') || el.getAttribute('title'));
      if (!visible(el) || iarea < (inamed ? 400 : 2500)) continue;
    }
    const name = nameFor(el);
    const isFocusable = el.tabIndex >= 0;
    // textContent value for prose matching: only computed when requested, capped
    // so a huge container's innerText cannot explode the per-element cost.
    const proseText = textContent ? clean(el.innerText || el.textContent || '').slice(0, 2000) : '';
    const hasImgAlt = el.tagName === 'IMG' && !!clean(el.getAttribute('alt'));
    const isUseful = role !== 'generic' || isFocusable || typeof el.onclick === 'function' || el.draggable === true || hasImgAlt || (textContent && proseText.length > 0);
    if (!isUseful) continue;
    if (formLensMode && !formRoles.has(role)) continue;
    const key = keyFor(el, role, name);
    const stableKey = stableKeyFor(el, role);
    const ref = refFor(el, key, stableKey);
    const checked = ('checked' in el) ? Boolean(el.checked) : null;
    const selected = ('selected' in el) ? Boolean(el.selected) : (el.getAttribute('aria-selected') === 'true' ? true : (el.getAttribute('aria-selected') === 'false' ? false : null));
    const expanded = el.getAttribute('aria-expanded') === 'true' ? true : (el.getAttribute('aria-expanded') === 'false' ? false : null);
    const signals = structuralSignals(el, role, active);
    const isSensitive = sensitive(el);
    const rawValue = ('value' in el) ? clean(el.value) : clean(el.getAttribute('data-value') || el.getAttribute('value') || '');
    const item = {
      ref,
      role,
      name,
      tag: el.tagName.toLowerCase(),
      type: (el.getAttribute('type') || '').toLowerCase(),
      href: el.href || el.getAttribute('href') || '',
      value: isSensitive ? '' : rawValue,
      visible: visible(el),
      in_viewport: inViewport(el),
      disabled: disabled(el),
      required: Boolean(el.required || el.getAttribute('aria-required') === 'true'),
      controls: el.getAttribute('aria-controls') || '',
      signals,
      source: ['dom'],
      key,
      _frontier_score: frontierScore(role, name, signals, visible(el), inViewport(el), disabled(el))
    };
    if (isSensitive) item.sensitive = true;
    if (checked !== null) item.checked = checked;
    if (selected !== null) item.selected = selected;
    if (expanded !== null) item.expanded = expanded;
    if (formLensMode && el.validity) {
      item.valid = el.validity.valid;
      if (!el.validity.valid) {
        item.validation_message = el.validationMessage || '';
      }
    }
    const haystack = [
      item.ref,
      item.role,
      item.name,
      item.tag,
      item.type,
      item.href,
      item.value,
      proseText
    ].join(' ').toLowerCase();
    if (viewportOnly && !item.in_viewport) continue;
    if (roleFilter && item.role !== roleFilter) continue;
    if (textFilter && !haystack.includes(textFilter)) continue;
    if (query && !haystack.includes(query)) continue;
    if (query || textFilter) {
      var needle = (query || textFilter);
      var reasons = [];
      if (item.name.toLowerCase().indexOf(needle) !== -1) reasons.push('name');
      if (item.role.toLowerCase().indexOf(needle) !== -1) reasons.push('role');
      if (item.ref.toLowerCase().indexOf(needle) !== -1) reasons.push('ref');
      if (item.value.toLowerCase().indexOf(needle) !== -1) reasons.push('value');
      if (item.tag.toLowerCase().indexOf(needle) !== -1) reasons.push('tag');
      if (item.type.toLowerCase().indexOf(needle) !== -1) reasons.push('type');
      if (item.href.toLowerCase().indexOf(needle) !== -1) reasons.push('href');
      if (textContent && proseText.toLowerCase().indexOf(needle) !== -1) reasons.push('text');
      item.match_reasons = reasons;
    }
    elByIndex.set(elements.length, el);
    elements.push(item);
    if (!frontierMode && limit > 0 && elements.length >= limit) break;
  }

  // Disambiguate ref collisions caused by stableKeyFor collapsing siblings.
  // When multiple elements share a ref, append name-based suffixes when names
  // differ (e.g. e6_edit, e6_delete) or numeric suffixes when names match
  // (e.g. e6_edit_1, e6_edit_2). Collisions among same-name siblings are
  // common on pages like challenging_dom where 10 "edit" links share the
  // same stableKey.
  {
    const refGroups = new Map();
    for (let i = 0; i < elements.length; i++) {
      const r = elements[i].ref;
      let g = refGroups.get(r);
      if (!g) { g = []; refGroups.set(r, g); }
      g.push(i);
    }
    for (const [r, group] of refGroups) {
      if (group.length < 2) continue;
      const byName = new Map();
      for (const idx of group) {
        const n = elements[idx].name || '';
        let arr = byName.get(n);
        if (!arr) { arr = []; byName.set(n, arr); }
        arr.push(idx);
      }
      for (const [n, indices] of byName) {
        const slug = n ? n.toLowerCase().replace(/[^a-z0-9]+/g, '_').replace(/^_+|_+$/g, '').slice(0, 20) : '';
        const needsIndex = indices.length > 1;
        for (let j = 0; j < indices.length; j++) {
          const idx = indices[j];
          let suffix;
          if (slug && needsIndex) suffix = '_' + slug + '_' + (j + 1);
          else if (slug) suffix = '_' + slug;
          else suffix = '_' + (j + 1);
          const newRef = r + suffix;
          elements[idx].ref = newRef;
          const domEl = elByIndex.get(idx);
          if (domEl) { try { domEl.setAttribute('data-brw-ref', newRef); } catch (_) {} }
          const k = elements[idx].key;
          if (k) { state.byKey[k] = newRef; state.byRef[newRef] = k; }
        }
      }
    }
  }

  function hasSignal(signals, name) {
    return Array.isArray(signals) && signals.includes(name);
  }

  function frontierScore(role, name, signals, visible, inViewport, disabled) {
    let score = 0;
    if (hasSignal(signals, 'focused')) score += 1000;
    if (hasSignal(signals, 'focus-within')) score += 900;
    if (hasSignal(signals, 'invalid')) score += 850;
    if (hasSignal(signals, 'expanded')) score += 760;
    if (hasSignal(signals, 'has-popup')) score += 720;
    if (hasSignal(signals, 'active-descendant') || hasSignal(signals, 'active-descendant-owner')) score += 680;
    if (hasSignal(signals, 'live')) score += 620;
    if (hasSignal(signals, 'frontier-role')) score += 580;
    if (visible) score += 180;
    if (inViewport) score += 220;
    if (!disabled) score += 80;
    if (name) score += 50;
    const rolePriority = {
      button: 180,
      textbox: 170,
      searchbox: 170,
      combobox: 160,
      listbox: 150,
      option: 140,
      checkbox: 135,
      radio: 135,
      menuitem: 130,
      link: 110,
      tab: 100,
      slider: 90,
      spinbutton: 90,
      image: 85,
      status: 80,
      alert: 80
    };
    return score + (rolePriority[role] || 0);
  }

  const totalCandidates = elements.length;
  if (frontierMode) {
    elements.sort((a, b) => {
      const diff = (b._frontier_score || 0) - (a._frontier_score || 0);
      if (diff !== 0) return diff;
      return String(a.ref || '').localeCompare(String(b.ref || ''));
    });
  }

  // Optionally collect visual islands and merge them into the element list. They
  // reuse the stable ref system (refFor) so a ref minted for a canvas survives
  // re-renders exactly like a DOM-element ref, and carry source:["visual"] plus a
  // visual_type/visual_hint so the model can reason about otherwise-opaque paint.
  let visualElements = [];
  if (Boolean(opts.visual_islands)) {
    const islands = detectVisualIslands(opts);
    for (const island of islands) {
      const el = island.el;
      const role = 'generic'; // visual islands are not interactive by definition
      const name = island.visualHint || '';
      const key = keyFor(el, role, name);
      const stableKey = stableKeyFor(el, role);
      const ref = refFor(el, key, stableKey);
      visualElements.push({
        ref,
        role: 'generic',
        name,
        tag: el.tagName.toLowerCase(),
        type: '',
        href: '',
        value: '',
        visible: true,
        in_viewport: true,
        disabled: false,
        required: false,
        controls: '',
        signals: ['visual-island'],
        source: ['visual'],
        visual_type: island.visualType,
        visual_hint: island.visualHint || '',
        key
      });
    }
  }

  // Merge: DOM elements first, then visual islands, then apply the cap. In
  // frontier mode the merged limit is the frontier limit; in unbounded modes a
  // limit of 0 means "return everything".
  const allElements = visualElements.length ? elements.concat(visualElements) : elements;
  const returned = limit > 0 ? allElements.slice(0, limit) : allElements;
  for (const item of returned) delete item._frontier_score;

  state.version = (state.version || 0) + 1;

  // --- since-delta (see snapshot.SnapshotDelta) ---------------------------------
  // A delta is only emitted when opts.since matches the PRIOR snapshot's version
  // AND the option envelope is identical (otherwise added/removed would reflect
  // option changes, not page changes). 'removed' is computed from genuine DOM
  // presence — every data-brw-ref node still in the tree — so an element merely
  // scrolled out of the viewport / past the limit is never falsely "removed".
  function __brwOptsSignature() {
    return [modeTag, opts.query || '', opts.text || '',
      opts.role || '', limit, Boolean(opts.viewport_only), includeHidden,
      textContent, Boolean(opts.visual_islands),
      (opts.visual_islands_limit === undefined ? '' : opts.visual_islands_limit)].join('');
  }
  function __brwFingerprint(it) {
    var __fp = {};
    for (var __k in it) { if (__k !== 'key' && __k !== 'ref') __fp[__k] = it[__k]; }
    return JSON.stringify(__fp);
  }
  function __brwDomRefs() {
    var refs = {}, roots = __abRootList();
    for (var i = 0; i < roots.length; i++) {
      var nodes;
      try { nodes = roots[i].querySelectorAll('[data-brw-ref]'); } catch (_) { continue; }
      for (var j = 0; j < nodes.length; j++) {
        var r = nodes[j].getAttribute('data-brw-ref');
        if (r) refs[r] = 1;
      }
    }
    return refs;
  }
  const __brwOptsKey = __brwOptsSignature();
  const __brwReturnedFP = {};
  for (const it of returned) __brwReturnedFP[it.ref] = __brwFingerprint(it);
  const __brwDomRefSet = __brwDomRefs();
  let __brwDelta = null;
  const __brwPrev = state.delta;
  if (opts.since && __brwPrev && __brwPrev.version === opts.since && __brwPrev.optsKey === __brwOptsKey) {
    const added = [], changed = [], removed = [];
    for (const ref in __brwReturnedFP) {
      if (!(ref in __brwPrev.returnedFP)) added.push(ref);
      else if (__brwReturnedFP[ref] !== __brwPrev.returnedFP[ref]) changed.push(ref);
    }
    for (const ref in __brwPrev.domRefs) {
      if (!(ref in __brwDomRefSet)) removed.push(ref);
    }
    __brwDelta = { added: added, removed: removed, changed: changed };
  }
  // Persist this snapshot's state for the NEXT delta request (single generation).
  state.delta = { version: state.version, optsKey: __brwOptsKey, returnedFP: __brwReturnedFP, domRefs: __brwDomRefSet };
  // On a delta, elements carries ONLY the added+changed elements (a change set);
  // removed refs travel in the delta object.
  let __brwOutElements = returned;
  if (__brwDelta) {
    const __brwChangeSet = {};
    for (const r of __brwDelta.added) __brwChangeSet[r] = 1;
    for (const r of __brwDelta.changed) __brwChangeSet[r] = 1;
    __brwOutElements = returned.filter(function(it) { return __brwChangeSet[it.ref]; });
  }
  // -----------------------------------------------------------------------------

  const focusedRef = active && active.getAttribute ? (active.getAttribute('data-brw-ref') || '') : '';

  // Coverage signal: detect "sparse semantic surface over a content-heavy
  // viewport" — the custom-component / CSR case where the DOM walker finds few
  // useful interactive elements even though the page is visibly painted and
  // interactive. When that happens we emit low_semantic_coverage:true plus a
  // hint steering the agent to a Set-of-Marks screenshot (brw_screenshot
  // annotate:true) and coordinate clicks. Generic + conservative: it fires only
  // when the in-viewport semantic element count is low AND the viewport carries
  // substantial content (lots of DOM nodes or visible text), so well-populated
  // pages never trip it.
  function coverageSignal() {
    var inViewportCount = 0;
    for (var i = 0; i < elements.length; i++) {
      if (elements[i].in_viewport) inViewportCount++;
    }
    var domNodes = 0, textLen = 0;
    try { domNodes = document.getElementsByTagName('*').length; } catch (_) {}
    try { textLen = clean((document.body && document.body.innerText) || '').length; } catch (_) {}
    // "content-heavy" = a real page worth of DOM or visible prose. Thresholds are
    // intentionally high so a near-empty page (which legitimately has few
    // elements) does not falsely report low coverage.
    var contentHeavy = domNodes >= 150 || textLen >= 400;
    var low = contentHeavy && inViewportCount <= 5;
    return { low: low, in_viewport_count: inViewportCount, dom_node_count: domNodes };
  }
  const coverage = coverageSignal();

  const metadata = {
    generated_at: new Date().toISOString(),
    element_count: __brwOutElements.length,
    total_candidates: totalCandidates,
    visual_island_count: visualElements.length,
    truncated: limit > 0 && (totalCandidates + visualElements.length) > returned.length,
    mode: modeTag,
    include_hidden: includeHidden,
    version: state.version,
    focused_ref: focusedRef,
    delta: Boolean(__brwDelta),
    low_semantic_coverage: coverage.low
  };
  if (coverage.low) {
    metadata.coverage_hint = 'Sparse semantic surface for a content-heavy page (likely custom web components or client-side rendering). Use brw_screenshot with annotate:true (Set-of-Marks) to read ref labels off the image, or click by coordinates; a region/ref-scoped annotated crop keeps the image small.';
  }
  if (__abInaccessibleFrames && __abInaccessibleFrames.length) {
    metadata.cross_origin_frames = __abInaccessibleFrames;
    metadata.cross_origin_note = 'One or more cross-origin iframes are present; their DOM is isolated by the browser and cannot be read as semantic refs. To act inside a listed frame, brw_screenshot it and use brw_click_xy at its box, or open the frame URL directly.';
  }

  return {
    url: location.href,
    title: document.title || '',
    elements: __brwOutElements,
    metadata: metadata,
    delta: __brwDelta || undefined
  };
})`

const SnapshotScript = SnapshotFunctionScript + `({})`

const ResolveBoxScript = `(function(ref) {` + FrameWalkHelpers + `
  const hit = __abFindDeep(ref);
  if (!hit) return { ok: false, ref };
  const el = hit.el;
  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  const r = el.getBoundingClientRect();
  // r is in the element's own (possibly framed) viewport; hit.ox/oy translate to
  // the top-level viewport that CDP MouseClickXY operates in. x/y add scroll so
  // they stay document-absolute for the top document.
  return {
    ok: r.width > 0 && r.height > 0,
    ref,
    x: r.left + hit.ox + window.scrollX,
    y: r.top + hit.oy + window.scrollY,
    width: r.width,
    height: r.height,
    viewport_x: r.left + hit.ox + r.width / 2,
    viewport_y: r.top + hit.oy + r.height / 2
  };
})`

const ResolveOrRecoverBoxScript = `(function(ref) {` + FrameWalkHelpers + `
  const state = window.__brw || (window.__brw = { next: 1, byKey: {}, byRef: {} });
  function roots() {
    return __abRootList();
  }
  // offsetFor returns the cumulative top-level viewport offset for an element by
  // matching it against the frame-aware root descriptors. 0,0 for the top document.
  function offsetFor(el) {
    var root = (el.getRootNode && el.getRootNode()) || document;
    var entries = __abRoots();
    for (var i = 0; i < entries.length; i++) {
      if (entries[i].root === root) return { ox: entries[i].ox, oy: entries[i].oy };
    }
    return { ox: 0, oy: 0 };
  }
  function findByRef(ref) {
    var hit = __abFindDeep(ref);
    return hit ? hit.el : null;
  }
  function clean(s) { return String(s || '').replace(/\s+/g, ' ').trim(); }
  function labelText(el) {
    if (!el) return '';
    if (el.labels && el.labels.length) return clean(Array.from(el.labels).map(function(l){ return l.innerText || l.textContent; }).join(' '));
    if (el.id) {
      var root = el.getRootNode && el.getRootNode();
      var labelRoot = root && root.querySelector ? root : document;
      var label = labelRoot.querySelector('label[for="' + CSS.escape(el.id) + '"]');
      if (label) return clean(label.innerText || label.textContent);
    }
    var parent = el.closest('label');
    if (parent) return clean(parent.innerText || parent.textContent);
    return '';
  }
  function nameFor(el) {
    return clean(el.getAttribute('aria-label') || labelText(el) || el.getAttribute('alt') || el.getAttribute('title') || el.getAttribute('placeholder') || el.getAttribute('name') || (el.innerText || el.textContent || ''));
  }
  function roleFor(el) {
    var explicit = clean(el.getAttribute('role'));
    if (explicit) return explicit.split(/\s+/)[0];
    var tag = el.tagName.toLowerCase();
    var type = (el.getAttribute('type') || '').toLowerCase();
    if (tag === 'a' && el.href) return 'link';
    if (tag === 'button' || type === 'button' || type === 'submit' || type === 'reset') return 'button';
    if (tag === 'textarea' || el.isContentEditable) return 'textbox';
    if (tag === 'select') return el.multiple ? 'listbox' : 'combobox';
    if (tag === 'input') {
      if (type === 'checkbox') return 'checkbox';
      if (type === 'radio') return 'radio';
      if (type === 'range') return 'slider';
      if (type === 'number') return 'spinbutton';
      if (type === 'search') return 'searchbox';
      return 'textbox';
    }
    return 'generic';
  }
  function recoverByRoleAndName(role, name, tag, type) {
    var best = null;
    var bestScore = 0;
    for (var i = 0; i < roots().length; i++) {
      var root = roots()[i];
      if (!root.querySelectorAll) continue;
      var candidates = root.querySelectorAll('*');
      for (var j = 0; j < candidates.length; j++) {
        var el = candidates[j];
        if (!el || el.nodeType !== 1) continue;
        var elRole = roleFor(el);
        var elName = nameFor(el);
        if (elRole !== role) continue;
        var score = 0;
        if (elName === name) score += 100;
        else if (name && elName.toLowerCase().indexOf(name.toLowerCase()) !== -1) score += 50;
        if (tag && el.tagName.toLowerCase() === tag) score += 20;
        if (type && (el.getAttribute('type') || '').toLowerCase() === type) score += 10;
        if (score > bestScore && score >= 50) {
          bestScore = score;
          best = el;
        }
      }
    }
    return best;
  }
  function refFor(el, key) {
    var existing = el.getAttribute('data-brw-ref');
    if (existing) {
      state.byRef[existing] = key;
      state.byKey[key] = existing;
      return existing;
    }
    var prior = state.byKey[key];
    var ref = prior || ('e' + state.next++);
    state.byKey[key] = ref;
    state.byRef[ref] = key;
    try { el.setAttribute('data-brw-ref', ref); } catch (_) {}
    return ref;
  }
  var el = findByRef(ref);
  if (el) {
    el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
    var r = el.getBoundingClientRect();
    var o = offsetFor(el);
    return { ok: r.width > 0 && r.height > 0, ref: ref, recovered: false, x: r.left + o.ox + window.scrollX, y: r.top + o.oy + window.scrollY, width: r.width, height: r.height, viewport_x: r.left + o.ox + r.width / 2, viewport_y: r.top + o.oy + r.height / 2 };
  }
  var key = state.byRef[ref];
  if (!key) return { ok: false, ref: ref, recovered: false, reason: 'no_key' };
  var parts = key.split('|');
  var kTag = parts[0] || '';
  var kRole = parts[1] || '';
  var kName = parts[2] || '';
  var kType = parts[6] || '';
  var recovered = recoverByRoleAndName(kRole, kName, kTag, kType);
  if (!recovered) return { ok: false, ref: ref, recovered: false, reason: 'no_match' };
  var newKey = [kTag, kRole, kName, recovered.id || '', recovered.getAttribute('name') || '', recovered.getAttribute('href') || '', kType, recovered.getAttribute('aria-controls') || '', ''].join('|');
  var newRef = refFor(recovered, newKey);
  recovered.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  var r2 = recovered.getBoundingClientRect();
  var o2 = offsetFor(recovered);
  return { ok: r2.width > 0 && r2.height > 0, ref: newRef, recovered: true, old_ref: ref, x: r2.left + o2.ox + window.scrollX, y: r2.top + o2.oy + window.scrollY, width: r2.width, height: r2.height, viewport_x: r2.left + o2.ox + r2.width / 2, viewport_y: r2.top + o2.oy + r2.height / 2 };
})`

const FocusElementScript = `(function(ref) {` + FrameWalkHelpers + `
  function roots() {
    return __abRootList();
  }
  function findByRef(ref) {
    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  function deepActive(root) {
    let active = root && root.activeElement;
    while (active && active.shadowRoot && active.shadowRoot.activeElement) {
      active = active.shadowRoot.activeElement;
    }
    return active;
  }
  function focused(el) {
    const active = deepActive(document);
    const root = el.getRootNode && el.getRootNode();
    const rootActive = deepActive(root);
    return active === el || el.contains(active) || rootActive === el || el.contains(rootActive);
  }
  const el = findByRef(ref);
  if (!el) return false;
  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  if (typeof el.focus === 'function') el.focus({ preventScroll: true });
  return focused(el);
})`

const SelectElementScript = `(function(ref, value) {` + FrameWalkHelpers + `
  function clean(s) {
    return String(s || '').replace(/\s+/g, ' ').trim();
  }
  function roots() {
    return __abRootList();
  }
  function findByRef(ref) {
    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  const el = findByRef(ref);
  if (!el) return { ok: false, error: 'ref not found — the page likely changed; re-run brw_snapshot to get current refs' };
  if (el.tagName.toLowerCase() !== 'select') return { ok: false, error: 'ref is not a select element' };
  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  const requested = clean(value);
  const options = Array.from(el.options || []);
  const option = options.find(o => String(o.value) === String(value)) ||
    options.find(o => clean(o.textContent) === requested);
  if (!option) return { ok: false, error: 'select option not found: ' + requested };
  el.value = option.value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
  el.dispatchEvent(new Event('change', { bubbles: true }));
  return { ok: true, ref, value: el.value, text: clean(option.textContent) };
})`

const FillElementScript = `(function(ref, text, replace) {` + FrameWalkHelpers + `
  function roots() {
    return __abRootList();
  }
  function findByRef(ref) {
    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  function setNativeValue(el, value) {
    const own = Object.getOwnPropertyDescriptor(el, 'value');
    const proto = Object.getPrototypeOf(el);
    const protoDesc = proto && Object.getOwnPropertyDescriptor(proto, 'value');
    if (protoDesc && protoDesc.set && (!own || own.set !== protoDesc.set)) {
      protoDesc.set.call(el, value);
      return;
    }
    if (own && own.set) {
      own.set.call(el, value);
      return;
    }
    el.value = value;
  }
  function emitInputEvents(el, text, replace) {
    const inputType = replace ? 'insertReplacementText' : 'insertText';
    try {
      el.dispatchEvent(new InputEvent('beforeinput', { bubbles: true, cancelable: true, composed: true, inputType, data: String(text || '') }));
    } catch (_) {}
    try {
      el.dispatchEvent(new InputEvent('input', { bubbles: true, composed: true, inputType, data: String(text || '') }));
    } catch (_) {
      el.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
    }
    el.dispatchEvent(new Event('change', { bubbles: true, composed: true }));
  }
  const el = findByRef(ref);
  if (!el) return { ok: false, error: 'ref not found — the page likely changed; re-run brw_snapshot to get current refs' };
  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  if (typeof el.focus === 'function') el.focus({ preventScroll: true });
  const current = ('value' in el) ? String(el.value || '') : String(el.textContent || '');
  const next = replace ? String(text || '') : current + String(text || '');
  if ('value' in el) {
    setNativeValue(el, next);
  } else if (el.isContentEditable || el.getAttribute('contenteditable') === 'true') {
    el.textContent = next;
  } else {
    var desc = el.tagName.toLowerCase();
    if (el.getAttribute('role')) desc += '[role=' + el.getAttribute('role') + ']';
    else if (el.type) desc += '[type=' + el.type + ']';
    return { ok: false, error: 'ref ' + ref + ' is not fillable (' + desc + ' has no value property and is not contenteditable; use brw_type to type into it, or re-run brw_snapshot to find the correct input ref)' };
  }
  emitInputEvents(el, text, replace);
  return { ok: true, ref, value: next };
})`

const FileInputElementScript = `(function(ref) {` + FrameWalkHelpers + `
  function roots() {
    return __abRootList();
  }
  function findByRef(ref) {
    if (!ref) return null;
    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  function onlyFileInput() {
    const matches = [];
    for (const root of roots()) {
      if (root.querySelectorAll) matches.push(...Array.from(root.querySelectorAll('input[type="file"]')));
    }
    return matches.length === 1 ? matches[0] : null;
  }
  const el = findByRef(ref) || onlyFileInput();
  if (!el) throw new Error('ref not found — the page likely changed; re-run brw_snapshot to get current refs');
  if (el.tagName.toLowerCase() !== 'input' || String(el.type || '').toLowerCase() !== 'file') {
    throw new Error('ref is not a file input');
  }
  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  return el;
})`

const FileInputEventsScript = `(function(ref) {` + FrameWalkHelpers + `
  function roots() {
    return __abRootList();
  }
  function findByRef(ref) {
    if (!ref) return null;
    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  function onlyFileInput() {
    const matches = [];
    for (const root of roots()) {
      if (root.querySelectorAll) matches.push(...Array.from(root.querySelectorAll('input[type="file"]')));
    }
    return matches.length === 1 ? matches[0] : null;
  }
  const el = findByRef(ref) || onlyFileInput();
  if (!el) return { ok: false, error: 'ref not found — the page likely changed; re-run brw_snapshot to get current refs' };
  if (el.tagName.toLowerCase() !== 'input' || String(el.type || '').toLowerCase() !== 'file') {
    return { ok: false, error: 'ref is not a file input' };
  }
  if (typeof el.focus === 'function') el.focus({ preventScroll: true });
  try {
    el.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
    el.dispatchEvent(new Event('change', { bubbles: true, composed: true }));
  } catch (_) {}
  return { ok: true, ref, files: Array.from(el.files || []).map(f => f.name) };
})`

const HoverElementScript = `(function(ref) {` + FrameWalkHelpers + `
  function roots() {
    return __abRootList();
  }
  function findByRef(ref) {
    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  const el = findByRef(ref);
  if (!el) return { ok: false, error: 'ref not found — the page likely changed; re-run brw_snapshot to get current refs' };
  if (el.closest('[hidden],[aria-hidden="true"]')) return { ok: false, error: 'ref hidden' };
  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  const r = el.getBoundingClientRect();
  const cx = r.left + r.width / 2;
  const cy = r.top + r.height / 2;
  const opts = { bubbles: true, cancelable: true, view: window, clientX: cx, clientY: cy };
  el.dispatchEvent(new PointerEvent('pointermove', opts));
  el.dispatchEvent(new MouseEvent('mouseenter', opts));
  el.dispatchEvent(new MouseEvent('mouseover', opts));
  return { ok: true, ref: ref };
})`

const ScrollPageScript = `(function(direction) {
  direction = String(direction || 'down').toLowerCase().trim() || 'down';
  const amount = 700;
  const delta = {
    left: direction === 'left' ? -amount : direction === 'right' ? amount : 0,
    top: direction === 'up' ? -amount : direction === 'down' ? amount : 0
  };
  if (!delta.left && !delta.top) return { ok: false, error: 'unsupported scroll direction: ' + direction };

  function deepActive(root) {
    let active = root && root.activeElement;
    while (active && active.shadowRoot && active.shadowRoot.activeElement) active = active.shadowRoot.activeElement;
    return active;
  }

  function visible(el) {
    if (!el || el.nodeType !== 1) return false;
    if (el.closest('[hidden],[aria-hidden="true"]')) return false;
    const style = window.getComputedStyle(el);
    if (!style || style.display === 'none' || style.visibility === 'hidden' || Number(style.opacity) === 0) return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && r.bottom >= 0 && r.right >= 0 && r.top <= window.innerHeight && r.left <= window.innerWidth;
  }

  function canMove(el) {
    if (!el || el.nodeType !== 1) return false;
    const style = window.getComputedStyle(el);
    const overflowY = style.overflowY;
    const overflowX = style.overflowX;
    const canY = delta.top !== 0 &&
      !['visible', 'clip'].includes(overflowY) &&
      el.scrollHeight > el.clientHeight + 2 &&
      (delta.top > 0 ? el.scrollTop + el.clientHeight < el.scrollHeight - 2 : el.scrollTop > 1);
    const canX = delta.left !== 0 &&
      !['visible', 'clip'].includes(overflowX) &&
      el.scrollWidth > el.clientWidth + 2 &&
      (delta.left > 0 ? el.scrollLeft + el.clientWidth < el.scrollWidth - 2 : el.scrollLeft > 1);
    return canY || canX;
  }

  function nameFor(el) {
    return String(el.getAttribute('aria-label') || el.getAttribute('title') || el.id || el.className || el.tagName || '').replace(/\s+/g, ' ').trim();
  }

  function score(el, active) {
    if (!visible(el) || !canMove(el)) return -1;
    const style = window.getComputedStyle(el);
    const role = String(el.getAttribute('role') || '').toLowerCase();
    const r = el.getBoundingClientRect();
    const area = Math.max(0, Math.min(r.right, window.innerWidth) - Math.max(r.left, 0)) *
      Math.max(0, Math.min(r.bottom, window.innerHeight) - Math.max(r.top, 0));
    const activeInside = Boolean(active && (el === active || el.contains(active)));
    const semanticSmallScroll = ['listbox', 'menu', 'tree', 'grid'].includes(role);
    if (!activeInside && !semanticSmallScroll && area < Math.max(12000, window.innerWidth * window.innerHeight * 0.035)) return -1;
    let s = Math.min(area / 1000, 600);
    if (activeInside) s += 3000;
    if (el.matches('dialog[open],[aria-modal="true"]')) s += 1200;
    if (['dialog', 'alertdialog', 'menu', 'listbox', 'tree', 'grid'].includes(role)) s += 800;
    if (style.position === 'fixed' || style.position === 'sticky') s += 700;
    const z = Number.parseInt(style.zIndex, 10);
    if (Number.isFinite(z) && z > 0) s += Math.min(z, 10000) / 20;
    if (r.left > window.innerWidth * 0.35 || r.top > window.innerHeight * 0.25) s += 120;
    return s;
  }

  function visibleArea(el) {
    const r = el.getBoundingClientRect();
    return Math.max(0, Math.min(r.right, window.innerWidth) - Math.max(r.left, 0)) *
      Math.max(0, Math.min(r.bottom, window.innerHeight) - Math.max(r.top, 0));
  }

  function overlayScore(el, active) {
    if (!visible(el)) return -1;
    const style = window.getComputedStyle(el);
    const role = String(el.getAttribute('role') || '').toLowerCase();
    const area = visibleArea(el);
    const viewportArea = Math.max(1, window.innerWidth * window.innerHeight);
    const modal = el.matches('dialog[open],[aria-modal="true"]') || ['dialog', 'alertdialog'].includes(role);
    const fixed = style.position === 'fixed' || style.position === 'sticky';
    if (!modal && !fixed) return -1;
    if (!modal && area < viewportArea * 0.14) return -1;
    const r = el.getBoundingClientRect();
    let s = area / 1000;
    if (modal) s += 2000;
    if (fixed) s += 800;
    if (active && (el === active || el.contains(active))) s += 1600;
    if (r.left > window.innerWidth * 0.35) s += 350;
    const z = Number.parseInt(style.zIndex, 10);
    if (Number.isFinite(z) && z > 0) s += Math.min(z, 10000) / 10;
    return s;
  }

  function activeOverlayRoots(active) {
    const roots = [];
    let best = null;
    let bestScore = -1;
    for (const el of Array.from(document.querySelectorAll('body *'))) {
      const s = overlayScore(el, active);
      if (s < 0) continue;
      if (active && (el === active || el.contains(active))) roots.push(el);
      if (s > bestScore) {
        bestScore = s;
        best = el;
      }
    }
    if (!roots.length && best) roots.push(best);
    return roots;
  }

  function windowCanMove() {
    const doc = document.scrollingElement || document.documentElement;
    if (delta.top !== 0) {
      return delta.top > 0
        ? window.scrollY + window.innerHeight < doc.scrollHeight - 2
        : window.scrollY > 1;
    }
    return delta.left > 0
      ? window.scrollX + window.innerWidth < doc.scrollWidth - 2
      : window.scrollX > 1;
  }

  const active = deepActive(document);
  const candidates = [];
  const seen = new Set();
  const overlayRoots = activeOverlayRoots(active);
  function addCandidate(el) {
    if (el && el.nodeType === Node.ELEMENT_NODE && !seen.has(el)) {
      seen.add(el);
      candidates.push(el);
    }
  }
  function addSubtree(root) {
    addCandidate(root);
    if (root && root.querySelectorAll) {
      for (const el of Array.from(root.querySelectorAll('*'))) addCandidate(el);
    }
  }
  if (overlayRoots.length) {
    for (const root of overlayRoots) addSubtree(root);
  } else {
    let node = active;
    while (node && node.nodeType === Node.ELEMENT_NODE) {
      addCandidate(node);
      node = node.parentElement || (node.getRootNode && node.getRootNode().host) || null;
    }
    for (const el of Array.from(document.querySelectorAll('body *'))) {
      addCandidate(el);
    }
  }

  let best = null;
  let bestScore = -1;
  for (const el of candidates) {
    const s = score(el, active);
    if (s > bestScore) {
      bestScore = s;
      best = el;
    }
  }

  if (best) {
    const beforeTop = best.scrollTop;
    const beforeLeft = best.scrollLeft;
    best.scrollBy({ left: delta.left, top: delta.top, behavior: 'instant' });
    return {
      ok: true,
      target: 'element',
      tag: best.tagName.toLowerCase(),
      role: best.getAttribute('role') || '',
      name: nameFor(best),
      changed: beforeTop !== best.scrollTop || beforeLeft !== best.scrollLeft,
      scroll_top: best.scrollTop,
      scroll_left: best.scrollLeft
    };
  }

  if (windowCanMove()) {
    const beforeTop = window.scrollY;
    const beforeLeft = window.scrollX;
    window.scrollBy({ left: delta.left, top: delta.top, behavior: 'instant' });
    return {
      ok: true,
      target: 'window',
      changed: beforeTop !== window.scrollY || beforeLeft !== window.scrollX,
      scroll_top: window.scrollY,
      scroll_left: window.scrollX
    };
  }

  return { ok: true, target: 'none', changed: false };
})`

func Evaluate(ctx context.Context) (PageSnapshot, error) {
	return EvaluateWithOptions(ctx, SnapshotOptions{})
}

func EvaluateWithOptions(ctx context.Context, opts SnapshotOptions) (PageSnapshot, error) {
	var snap PageSnapshot
	args, _ := json.Marshal(opts)
	// Fast path: a prior call on this document already installed the walker under
	// the private per-process name, so ship only the tiny call expression instead
	// of the ~30KB source.
	hit := fmt.Sprintf("%s(%s)", snapshotInstallTarget, args)
	if err := chromedp.Run(ctx, chromedp.Evaluate(hit, &snap)); err != nil {
		// Cold document (first call, or a navigation replaced the JS context):
		// define the walker and call it in a single round-trip. The assignment is
		// UNCONDITIONAL so a page that predefined this name (collision) or set it
		// to a non-function cannot shadow/spoof or wedge snapshots — we always
		// install our own. Every later call on this document takes the fast path,
		// so the full source ships once per document, not once per call.
		snap = PageSnapshot{}
		cold := fmt.Sprintf("(function(){%s=%s;return %s(%s);})()", snapshotInstallTarget, SnapshotFunctionScript, snapshotInstallTarget, args)
		if err := chromedp.Run(ctx, chromedp.Evaluate(cold, &snap)); err != nil {
			return PageSnapshot{}, err
		}
	}
	for i := range snap.Elements {
		if len(snap.Elements[i].Source) == 0 {
			snap.Elements[i].Source = []string{"dom"}
		}
	}
	return snap, nil
}

func Find(ctx context.Context, opts FindOptions) (FindResult, error) {
	snap, err := EvaluateWithOptions(ctx, SnapshotOptions{
		Query:         opts.Query,
		Text:          opts.Text,
		Role:          opts.Role,
		Limit:         opts.Limit,
		ViewportOnly:  opts.ViewportOnly,
		IncludeHidden: opts.IncludeHidden,
		TextContent:   opts.TextContent,
	})
	if err != nil {
		return FindResult{}, err
	}
	return FindResult{
		URL:      snap.URL,
		Title:    snap.Title,
		Elements: snap.Elements,
		Metadata: snap.Metadata,
	}, nil
}

func Scroll(ctx context.Context, direction string) (ScrollResult, error) {
	var result ScrollResult
	directionJSON, _ := json.Marshal(direction)
	expr := fmt.Sprintf("%s(%s)", ScrollPageScript, directionJSON)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
		return ScrollResult{}, err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "scroll failed"
		}
		return result, errors.New(result.Error)
	}
	return result, nil
}

func ResolveBox(ctx context.Context, ref string) (ElementBox, error) {
	var box ElementBox
	args, _ := json.Marshal(ref)
	expr := fmt.Sprintf("%s(%s)", ResolveBoxScript, args)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &box)); err != nil {
		return ElementBox{}, err
	}
	if !box.OK {
		return box, fmt.Errorf("element ref %q not found or not visible — the page likely changed; re-run brw_snapshot to get current refs", ref)
	}
	return box, nil
}

type RecoveredBox struct {
	ElementBox
	Recovered bool   `json:"recovered"`
	OldRef    string `json:"old_ref,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func ResolveOrRecoverBox(ctx context.Context, ref string) (RecoveredBox, error) {
	var box RecoveredBox
	args, _ := json.Marshal(ref)
	expr := fmt.Sprintf("%s(%s)", ResolveOrRecoverBoxScript, args)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &box)); err != nil {
		return RecoveredBox{}, err
	}
	if !box.OK {
		reason := box.Reason
		if reason == "" {
			reason = "not_visible"
		}
		return box, fmt.Errorf("element ref %q not recoverable: %s", ref, reason)
	}
	return box, nil
}

func Focus(ctx context.Context, ref string) error {
	var ok bool
	args, _ := json.Marshal(ref)
	expr := fmt.Sprintf("%s(%s)", FocusElementScript, args)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ok)); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("element ref %q not found or could not be focused — the page likely changed; re-run brw_snapshot to get current refs", ref)
	}
	return nil
}

func Select(ctx context.Context, ref, value string) error {
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	refJSON, _ := json.Marshal(ref)
	valueJSON, _ := json.Marshal(value)
	expr := fmt.Sprintf("%s(%s,%s)", SelectElementScript, refJSON, valueJSON)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
		return err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "select failed"
		}
		return errors.New(result.Error)
	}
	return nil
}

func Fill(ctx context.Context, ref, text string, replace bool) error {
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	refJSON, _ := json.Marshal(ref)
	textJSON, _ := json.Marshal(text)
	replaceJSON, _ := json.Marshal(replace)
	expr := fmt.Sprintf("%s(%s,%s,%s)", FillElementScript, refJSON, textJSON, replaceJSON)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
		return err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "fill failed"
		}
		return errors.New(result.Error)
	}
	return nil
}

// DragHtml5Script simulates the native HTML5 drag-and-drop protocol between two
// refs. CDP mouse events (mousedown/move/up) do NOT drive HTML5 DnD
// (draggable=true elements that listen for dragstart/dragover/drop) — the browser
// only synthesises drag events for a real OS drag loop — so a coordinate drag
// silently no-ops on those widgets. This dispatches the real sequence
// (dragstart → drag → dragenter → dragover → drop → dragend) carrying ONE shared
// DataTransfer, which is exactly what a drop handler reads. Returns
// {ok, dropped} where dropped reports whether the target's drop handler ran
// (preventDefault) so the caller can fall back to a coordinate drag when false.
const DragHtml5Script = `(function(fromRef, toRef){` + FrameWalkHelpers + `
  function findByRef(ref){
    var sel='[data-brw-ref="'+CSS.escape(ref)+'"]';
    for (var root of __abRootList()){ var el=root.querySelector&&root.querySelector(sel); if(el) return el; }
    return null;
  }
  var source=findByRef(fromRef), target=findByRef(toRef);
  if(!source) return {ok:false, error:'drag source ref not found'};
  if(!target) return {ok:false, error:'drag target ref not found'};
  var dt; try { dt=new DataTransfer(); } catch(e){ dt=null; }
  function pt(el){ var r=el.getBoundingClientRect(); return {x:r.left+r.width/2, y:r.top+r.height/2}; }
  function fire(el, type, p){
    var ev;
    try { ev=new DragEvent(type, {bubbles:true, cancelable:true, composed:true, dataTransfer:dt, clientX:p.x, clientY:p.y}); }
    catch(e){
      ev=new MouseEvent(type, {bubbles:true, cancelable:true, composed:true, clientX:p.x, clientY:p.y});
      if(dt){ try { Object.defineProperty(ev,'dataTransfer',{value:dt}); } catch(_){} }
    }
    return el.dispatchEvent(ev);
  }
  var sp=pt(source), tp=pt(target);
  fire(source,'dragstart',sp);
  fire(source,'drag',sp);
  fire(target,'dragenter',tp);
  var overCancelled = !fire(target,'dragover',tp); // returns false when preventDefault'd
  var dropCancelled = !fire(target,'drop',tp);
  fire(source,'dragend',tp);
  return {ok:true, dropped: overCancelled || dropCancelled};
})`

// DragHtml5 runs the HTML5 drag-and-drop simulation between two refs. It returns
// (dropped, err): dropped is true when the target accepted the drop (its handler
// called preventDefault), letting the caller fall back to a coordinate drag.
func DragHtml5(ctx context.Context, fromRef, toRef string) (bool, error) {
	fr, _ := json.Marshal(fromRef)
	tr, _ := json.Marshal(toRef)
	expr := fmt.Sprintf("%s(%s,%s)", DragHtml5Script, fr, tr)
	var res struct {
		OK      bool   `json:"ok"`
		Dropped bool   `json:"dropped"`
		Error   string `json:"error"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &res)); err != nil {
		return false, err
	}
	if !res.OK {
		if res.Error == "" {
			res.Error = "html5 drag failed"
		}
		return false, errors.New(res.Error)
	}
	return res.Dropped, nil
}

// RefDraggable reports whether the element identified by ref has the native HTML5
// draggable affordance (draggable=true), so the caller can pick the HTML5 drag
// path over a coordinate drag.
func RefDraggable(ctx context.Context, ref string) bool {
	rj, _ := json.Marshal(ref)
	expr := fmt.Sprintf(`(function(ref){`+FrameWalkHelpers+`
  var sel='[data-brw-ref="'+CSS.escape(ref)+'"]';
  for (var root of __abRootList()){ var el=root.querySelector&&root.querySelector(sel); if(el) return !!el.draggable; }
  return false;
})(%s)`, rj)
	var draggable bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &draggable)); err != nil {
		return false
	}
	return draggable
}

// WaitConditionScript returns a Promise that resolves true as soon as the given
// condition holds, or false after timeoutMs. It checks immediately, then re-checks
// on DOM mutations (MutationObserver) and history events, with a 100ms safety
// interval for signals those miss (e.g. pushState URL changes) — replacing a
// fixed-interval CDP poll loop with a single awaited in-page promise.
const WaitConditionScript = `(function(condition, timeoutMs){` + FrameWalkHelpers + `
  if (!condition || condition === 'load' || condition === 'page_ready') condition = 'ready';
  function roots(){
    return __abRootList();
  }
  function hasRef(ref){
    const selector='[data-brw-ref="'+CSS.escape(ref)+'"]';
    return roots().some(root => root.querySelector && root.querySelector(selector));
  }
  function check(){
    if(condition==='ready') return document.readyState==='complete'||document.readyState==='interactive';
    if(condition==='committed') return (document.readyState==='complete'||document.readyState==='interactive') && location.href !== 'about:blank' && location.href !== '';
    if(condition.startsWith('url:')) return location.href.includes(condition.slice(4));
    if(condition.startsWith('not_url:')) return !location.href.includes(condition.slice(8));
    if(condition.startsWith('title:')) return document.title.includes(condition.slice(6));
    if(condition.startsWith('not_title:')) return !document.title.includes(condition.slice(10));
    if(condition.startsWith('text:')) return !!document.body && document.body.innerText.includes(condition.slice(5));
    if(condition.startsWith('not_text:')) return !document.body || !document.body.innerText.includes(condition.slice(9));
    if(condition.startsWith('ref:')) return hasRef(condition.slice(4));
    if(condition.startsWith('not_ref:')) return !hasRef(condition.slice(8));
    return !!document.body && document.body.innerText.includes(condition);
  }
  return new Promise(function(resolve){
    if(check()){ resolve(true); return; }
    var done=false, obs=null, iv=0, to=0;
    function recheck(){ try{ if(check()) finish(true); }catch(e){} }
    function finish(v){
      if(done) return; done=true;
      try{ if(obs) obs.disconnect(); }catch(e){}
      if(iv) clearInterval(iv);
      if(to) clearTimeout(to);
      try{ window.removeEventListener('popstate', recheck); window.removeEventListener('hashchange', recheck); }catch(e){}
      resolve(v);
    }
    try{ obs=new MutationObserver(recheck); obs.observe(document.documentElement||document, {subtree:true, childList:true, characterData:true, attributes:true}); }catch(e){}
    try{ window.addEventListener('popstate', recheck); window.addEventListener('hashchange', recheck); }catch(e){}
    iv=setInterval(recheck, 100);
    to=setTimeout(function(){ finish(check()); }, Math.max(0, timeoutMs|0));
  });
})`

// WaitForCondition evaluates WaitConditionScript and awaits its promise, returning
// whether the condition was met within timeoutMs.
func WaitForCondition(ctx context.Context, condition string, timeoutMs int64) (bool, error) {
	condJSON, _ := json.Marshal(condition)
	expr := fmt.Sprintf("%s(%s,%d)", WaitConditionScript, condJSON, timeoutMs)
	var matched bool
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
			return fmt.Errorf("wait condition failed: %s", details)
		}
		if obj == nil || len(obj.Value) == 0 {
			return nil
		}
		return json.Unmarshal(obj.Value, &matched)
	})); err != nil {
		return false, err
	}
	return matched, nil
}

// SettleScript returns a Promise that resolves the MOMENT the page settles after
// an action, bounded by capMs. It replaces the old unconditional fixed
// post-action chromedp.Sleep with an event-driven wait that returns early when the
// page has demonstrably reacted and gone quiet:
//
//   - DOM-mutation quiesce: a MutationObserver watches the whole tree; every
//     mutation arms a short quiet timer (quietMs, ~2 animation frames). When no
//     further mutation lands for quietMs after at least one mutation was seen, the
//     page has visibly reacted and stopped, so resolve.
//   - Navigation / history signals: popstate, hashchange, and pagehide all mean
//     the action triggered a navigation; resolve promptly (the post-action snapshot
//     reads the new state). beforeunload is treated the same way.
//   - Network signal: a PerformanceObserver for 'resource' entries resolves as soon
//     as a network response lands (XHR/fetch/img), which is the common "click ->
//     request -> render" case.
//
// The hard cap (capMs, via performance.now) bounds the worst case so a page that
// never quiesces (continuous animation, polling) degrades to exactly today's fixed
// delay — never slower. It always resolves an object reporting how long it actually
// waited (settledMs) and why (reason), so the caller can record the latency win.
//
// Reuses the same MutationObserver + nav-event + performance.now() primitives proven
// by WaitConditionScript; it is additive and carries no site-specific logic.
const SettleScript = `(function(capMs){
  var cap=Math.max(0, capMs|0);
  // quietMs is the DOM-mutation quiesce window: once a mutation is seen, the page
  // is considered settled if no further mutation arrives within this window. ~2
  // animation frames at 60fps; clamped well under the cap so the early path is a
  // genuine win even at small caps.
  var quietMs=Math.min(40, cap);
  var start=performance.now();
  return new Promise(function(resolve){
    var done=false, obs=null, po=null, capTo=0, quietTo=0, sawMutation=false;
    function elapsed(){ return Math.round(performance.now()-start); }
    function finish(reason){
      if(done) return; done=true;
      try{ if(obs) obs.disconnect(); }catch(e){}
      try{ if(po) po.disconnect(); }catch(e){}
      if(capTo) clearTimeout(capTo);
      if(quietTo) clearTimeout(quietTo);
      try{
        window.removeEventListener('popstate', onNav, true);
        window.removeEventListener('hashchange', onNav, true);
        window.removeEventListener('pagehide', onNav, true);
        window.removeEventListener('beforeunload', onNav, true);
      }catch(e){}
      resolve({settledMs:elapsed(), reason:reason, cap:cap});
    }
    function onNav(){ finish('navigation'); }
    function armQuiet(){
      if(quietTo) clearTimeout(quietTo);
      quietTo=setTimeout(function(){ finish('quiesce'); }, quietMs);
    }
    function onMutation(){
      sawMutation=true;
      armQuiet();
    }
    // Hard cap — never slower than today's fixed delay.
    capTo=setTimeout(function(){ finish(sawMutation?'quiesce_cap':'cap'); }, cap);
    if(cap===0){ finish('cap'); return; }
    try{
      obs=new MutationObserver(onMutation);
      obs.observe(document.documentElement||document, {subtree:true, childList:true, characterData:true, attributes:true});
    }catch(e){}
    try{
      po=new PerformanceObserver(function(){ finish('network'); });
      po.observe({type:'resource', buffered:false});
    }catch(e){}
    try{
      window.addEventListener('popstate', onNav, true);
      window.addEventListener('hashchange', onNav, true);
      window.addEventListener('pagehide', onNav, true);
      window.addEventListener('beforeunload', onNav, true);
    }catch(e){}
  });
})`

// SettleResult reports how an in-page settle wait resolved: how long it actually
// waited (SettledMS), why it stopped (Reason: quiesce | navigation | network |
// quiesce_cap | cap), and the cap that bounded it.
type SettleResult struct {
	SettledMS int64  `json:"settledMs"`
	Reason    string `json:"reason"`
	Cap       int64  `json:"cap"`
}

// Settle awaits SettleScript: it resolves the moment the page settles after an
// action (DOM mutations quiesce, OR a navigation/popstate/hashchange/pagehide
// fires, OR a network response lands) bounded by capMs so the worst case equals
// today's fixed delay. On any evaluation error it returns a zero result and the
// error; callers treat that as "no settle observed" without failing the action.
func Settle(ctx context.Context, capMs int64) (SettleResult, error) {
	expr := fmt.Sprintf("%s(%d)", SettleScript, capMs)
	var result SettleResult
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
			return fmt.Errorf("settle failed: %s", details)
		}
		if obj == nil || len(obj.Value) == 0 {
			return nil
		}
		return json.Unmarshal(obj.Value, &result)
	})); err != nil {
		return SettleResult{}, err
	}
	return result, nil
}

// WaitForActionableScript returns a Promise that resolves true when the element
// identified by ref is visible, stable (bounding box unchanged for two
// consecutive checks 100ms apart), and enabled. Resolves false on timeout.
const WaitForActionableScript = `(function(ref, timeoutMs){` + FrameWalkHelpers + `
  function roots(){
    return __abRootList();
  }
  function findByRef(ref){
    const selector='[data-brw-ref="'+CSS.escape(ref)+'"]';
    for(const root of roots()){
      const el=root.querySelector&&root.querySelector(selector);
      if(el) return el;
    }
    return recoverRef(ref);
  }
  function clean(s){ return String(s||'').replace(/\s+/g,' ').trim(); }
  function labelText(el){
    if(!el) return '';
    if(el.labels&&el.labels.length) return clean(Array.from(el.labels).map(function(l){return l.innerText||l.textContent;}).join(' '));
    if(el.id){
      var root=el.getRootNode&&el.getRootNode();
      var labelRoot=root&&root.querySelector?root:document;
      var label=labelRoot.querySelector('label[for="'+CSS.escape(el.id)+'"]');
      if(label) return clean(label.innerText||label.textContent);
    }
    var parent=el.closest('label');
    return parent?clean(parent.innerText||parent.textContent):'';
  }
  function nameFor(el){
    return clean(el.getAttribute('aria-label')||labelText(el)||el.getAttribute('alt')||el.getAttribute('title')||el.getAttribute('placeholder')||el.getAttribute('name')||(el.innerText||el.textContent||''));
  }
  function roleFor(el){
    var explicit=clean(el.getAttribute('role'));
    if(explicit) return explicit.split(/\s+/)[0];
    var tag=el.tagName.toLowerCase();
    var type=(el.getAttribute('type')||'').toLowerCase();
    if(tag==='a'&&el.href) return 'link';
    if(tag==='button'||type==='button'||type==='submit'||type==='reset'||type==='image') return 'button';
    if(tag==='textarea'||el.isContentEditable) return 'textbox';
    if(tag==='select') return el.multiple?'listbox':'combobox';
    if(tag==='input'){
      if(type==='checkbox') return 'checkbox';
      if(type==='radio') return 'radio';
      if(type==='range') return 'slider';
      if(type==='number') return 'spinbutton';
      if(type==='search') return 'searchbox';
      return 'textbox';
    }
    return 'generic';
  }
  function refFor(el,key){
    var state=window.__brw||(window.__brw={next:1,byKey:{},byRef:{}});
    var existing=el.getAttribute('data-brw-ref');
    if(existing){ state.byRef[existing]=key; state.byKey[key]=existing; return existing; }
    var prior=state.byKey[key];
    var newRef=prior||('e'+state.next++);
    state.byKey[key]=newRef;
    state.byRef[newRef]=key;
    try{ el.setAttribute('data-brw-ref',newRef); }catch(_){}
    return newRef;
  }
  function recoverByRoleAndName(role,name,tag,type){
    var best=null,bestScore=0;
    var allRoots=roots();
    for(var i=0;i<allRoots.length;i++){
      var root=allRoots[i];
      if(!root.querySelectorAll) continue;
      var candidates=root.querySelectorAll('*');
      for(var j=0;j<candidates.length;j++){
        var el=candidates[j];
        if(!el||el.nodeType!==1) continue;
        var elRole=roleFor(el);
        if(elRole!==role) continue;
        var elName=nameFor(el);
        var score=0;
        if(elName===name) score+=100;
        else if(name&&elName.toLowerCase().indexOf(name.toLowerCase())!==-1) score+=50;
        if(tag&&el.tagName.toLowerCase()===tag) score+=20;
        if(type&&(el.getAttribute('type')||'').toLowerCase()===type) score+=10;
        if(score>bestScore&&score>=50){ bestScore=score; best=el; }
      }
    }
    return best;
  }
  function recoverRef(ref){
    var state=window.__brw;
    var key=state&&state.byRef&&state.byRef[ref];
    if(!key) return null;
    var parts=key.split('|');
    var recovered=recoverByRoleAndName(parts[1]||'',parts[2]||'',parts[0]||'',parts[6]||'');
    if(!recovered) return null;
    var newKey=[parts[0]||'',parts[1]||'',parts[2]||'',recovered.id||'',recovered.getAttribute('name')||'',recovered.getAttribute('href')||'',parts[6]||'',recovered.getAttribute('aria-controls')||'',''].join('|');
    refFor(recovered,newKey);
    return recovered;
  }
  // visible() is the strict AX-style heuristic: the element is not display:none /
  // visibility:hidden / opacity:0, not inside [hidden]/[aria-hidden], and paints a
  // non-zero box. Custom web components frequently FAIL this (a host element with
  // a zero-size shadow host, an aria-hidden wrapper, opacity driven by a CSS var
  // the heuristic can't read) even though they are visibly painted and clickable,
  // which is exactly the false-negative that burned the full WaitForActionable
  // timeout on heavy-CSR pages. It stays the FAST PATH; geometryActionable() is the
  // fallback that proceeds when the AX heuristic disagrees but the pixel is real.
  function visible(el){
    if(!el||el.nodeType!==1) return false;
    if(el.closest('[hidden],[aria-hidden="true"]')) return false;
    var w=(el.ownerDocument&&el.ownerDocument.defaultView)||window;
    const s=w.getComputedStyle(el);
    if(!s||s.display==='none'||s.visibility==='hidden'||Number(s.opacity)===0) return false;
    const r=el.getClientRects();
    return r&&r.length>0&&Array.from(r).some(function(x){return x.width>0&&x.height>0;});
  }
  // cssRemoved() is the hard-disqualifier geometry actionability honours: a
  // display:none / visibility:hidden element (or descendant of one) genuinely
  // cannot receive a pointer event, so it is never geometry-actionable. opacity:0
  // is DELIBERATELY NOT disqualifying: an opacity:0 element still has layout and
  // still receives pointer events (the canonical CSS-styled checkbox/radio pattern
  // — e.g. TodoMVC's .toggle — hides the native control with opacity:0 and overlays
  // a styled label, yet the control is fully clickable). The elementFromPoint hit
  // test in geometryActionable() already rejects genuinely-occluded elements, so a
  // transparent-but-topmost control is correctly actionable. Unlike visible(), this
  // ignores aria-hidden and getClientRects heuristics — a custom component can be
  // aria-hidden yet fully painted and clickable.
  function cssRemoved(el){
    var n=el;
    while(n&&n.nodeType===1){
      var w=(n.ownerDocument&&n.ownerDocument.defaultView)||window;
      var s=w.getComputedStyle(n);
      if(s&&(s.display==='none'||s.visibility==='hidden')) return true;
      n=n.parentElement||(n.getRootNode&&n.getRootNode().host)||null;
    }
    return false;
  }
  function deepFromPoint(doc, x, y){
    var el=doc.elementFromPoint(x,y);
    // Pierce shadow roots so the hit-test resolves the real painted leaf inside a
    // custom component, not just its shadow host.
    while(el&&el.shadowRoot){
      var inner=el.shadowRoot.elementFromPoint(x,y);
      if(!inner||inner===el) break;
      el=inner;
    }
    return el;
  }
  // geometryActionable() is the hit-test path: the element occupies a non-zero box
  // inside the viewport, is not CSS-removed, and the pixel at its (viewport-clamped)
  // center actually belongs to it — elementFromPoint resolves to the element, a
  // descendant, or an ancestor that contains it (a wrapper that intercepts the
  // pointer on the element's behalf). This is what makes an AX-invisible custom
  // component clickable without burning the timeout, and is intentionally pointer-
  // event-honest (an element fully occluded by an overlay fails the contains test).
  function geometryActionable(el){
    if(!el||el.nodeType!==1) return false;
    if(cssRemoved(el)) return false;
    var w=(el.ownerDocument&&el.ownerDocument.defaultView)||window;
    var r=el.getBoundingClientRect();
    if(!(r.width>0&&r.height>0)) return false;
    var vw=w.innerWidth||0, vh=w.innerHeight||0;
    // Must intersect the viewport (a screenshot/click only reaches painted pixels).
    if(r.bottom<0||r.right<0||r.top>vh||r.left>vw) return false;
    var cx=Math.max(1,Math.min((vw||r.right)-1, r.left+r.width/2));
    var cy=Math.max(1,Math.min((vh||r.bottom)-1, r.top+r.height/2));
    var doc=el.ownerDocument||document;
    var hit=deepFromPoint(doc, cx, cy);
    if(!hit) return false;
    return hit===el||el.contains(hit)||(hit.contains&&hit.contains(el));
  }
  function boxKey(el){
    const r=el.getBoundingClientRect();
    return [r.x,r.y,r.width,r.height].join(',');
  }
  // check() reports actionability AND which path established it (mode): 'ax_visible'
  // when the strict heuristic passes, 'hit_test' when only geometry+hit-test does.
  // The mode is surfaced to the Go caller so it can decide whether the optimized
  // in-page click suffices or a coordinate fallback is warranted.
  function check(){
    var el=findByRef(ref);
    if(!el) return {ok:false,reason:'not_found'};
    if(el.disabled||el.getAttribute('aria-disabled')==='true') return {ok:false,reason:'disabled'};
    if(visible(el)) return {ok:true,mode:'ax_visible'};
    if(geometryActionable(el)) return {ok:true,mode:'hit_test'};
    return {ok:false,reason:'not_visible'};
  }
  return new Promise(function(resolve){
    var c=check();
    if(!c.ok&&c.reason!=='not_visible'){ resolve({ok:false,reason:c.reason}); return; }
    // FAIL-FAST: an element present in the DOM but neither AX-visible NOR
    // geometry-actionable rarely recovers within the full timeout (it is genuinely
    // off-screen / occluded / not yet laid out). Bound that case to a short window
    // (failFastMs) so a present-but-invisible element returns promptly instead of
    // burning the whole 5s, then degrades to the retry path. A geometry-actionable
    // element proceeds immediately via the normal stability loop.
    var deadline=Math.max(0, timeoutMs|0);
    var failFastMs=Math.min(deadline, 400);
    // Re-check stability with a short setTimeout chain, not setInterval(100) and
    // not requestAnimationFrame. Hidden/headless pages PAUSE rAF entirely and
    // (without --disable-background-timer-throttling) clamp timers to ~1Hz,
    // which silently turned the old 100ms poll into a ~700-900ms stall per
    // click. With throttling disabled at launch a 16ms timeout fires promptly,
    // so a statically-positioned element settles in ~2 samples (~16ms). The
    // in-page deadline (performance.now) bounds the wait regardless.
    var done=false,lastBox='',start=performance.now(),mode='';
    function finish(v,m){ if(done)return; done=true; resolve({ok:v,mode:m||'',reason:v?'':'not_visible'}); }
    function step(){
      if(done) return;
      var cc=check();
      if(cc.ok){
        mode=cc.mode;
        var bk=boxKey(findByRef(ref));
        if(bk===lastBox){ finish(true,mode); return; } // unchanged across two samples => stable
        lastBox=bk;
      } else {
        lastBox='';
      }
      var now=performance.now()-start;
      // While the element has NEVER become actionable, honour the short fail-fast
      // budget; once it has been seen actionable at least once, the full deadline
      // applies so a briefly-jittering box still gets time to settle.
      var budget=(mode==='')?failFastMs:deadline;
      if(now>=budget){ var fc=check(); finish(fc.ok, fc.mode); return; }
      setTimeout(step,16);
    }
    step();
  });
})`

// ActionableResult reports how WaitForActionableScript resolved: whether the
// element became actionable (OK), which path established it (Mode: "ax_visible"
// when the strict AX heuristic passed, "hit_test" when only geometry+elementFromPoint
// did), and the failure reason on timeout. Mode lets callers decide whether the
// optimized in-page click suffices ("ax_visible") or a coordinate/CDP fallback is
// the more reliable actuation ("hit_test" custom components).
type ActionableResult struct {
	OK     bool   `json:"ok"`
	Mode   string `json:"mode,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// WaitForActionableResult waits for the element identified by ref to become
// actionable (AX-visible OR geometry+hit-test actionable), stable, and enabled
// within timeoutMs, returning the resolution detail. A present-but-AX-invisible
// element that is also not geometry-actionable fails fast (short bounded wait)
// rather than burning the full timeout.
func WaitForActionableResult(ctx context.Context, ref string, timeoutMs int64) (ActionableResult, error) {
	refJSON, _ := json.Marshal(ref)
	expr := fmt.Sprintf("%s(%s,%d)", WaitForActionableScript, refJSON, timeoutMs)
	var res ActionableResult
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
			return fmt.Errorf("actionable wait failed: %s", details)
		}
		if obj == nil || len(obj.Value) == 0 {
			return nil
		}
		return json.Unmarshal(obj.Value, &res)
	})); err != nil {
		return ActionableResult{}, err
	}
	return res, nil
}

// WaitForActionable waits for the element identified by ref to become actionable,
// stable, and enabled within timeoutMs. Returns nil on success, error on timeout.
func WaitForActionable(ctx context.Context, ref string, timeoutMs int64) error {
	res, err := WaitForActionableResult(ctx, ref, timeoutMs)
	if err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("element ref %q not actionable within %dms — it may be hidden, disabled, or covered by an overlay; re-run brw_snapshot to refresh refs, or brw_screenshot with ref %q to inspect it", ref, timeoutMs, ref)
	}
	return nil
}

// AssertTextScript returns a Promise that resolves true when the element
// identified by ref contains the expected text (case-insensitive substring
// match). Resolves false on timeout.
const AssertTextScript = `(function(ref, expected, timeoutMs){` + FrameWalkHelpers + `
  function roots(){
    return __abRootList();
  }
  function findByRef(ref){
    const selector='[data-brw-ref="'+CSS.escape(ref)+'"]';
    for(const root of roots()){
      const el=root.querySelector&&root.querySelector(selector);
      if(el) return el;
    }
    return null;
  }
  function check(){
    var el=findByRef(ref);
    if(!el) return false;
    var text=(el.innerText||el.textContent||el.value||'').toLowerCase();
    return text.indexOf(expected.toLowerCase())!==-1;
  }
  return new Promise(function(resolve){
    if(check()){ resolve(true); return; }
    var done=false,iv=0,to=0;
    function finish(v){ if(done)return; done=true; if(iv)clearInterval(iv); if(to)clearTimeout(to); resolve(v); }
    var obs=null;
    try{ obs=new MutationObserver(function(){ if(check()) finish(true); });
         obs.observe(document.documentElement||document, {subtree:true, childList:true, characterData:true, attributes:true}); }catch(e){}
    iv=setInterval(function(){ if(check()) finish(true); }, 100);
    to=setTimeout(function(){ finish(check()); }, Math.max(0, timeoutMs|0));
  });
})`

// AssertValueScript returns a Promise that resolves true when the element
// identified by ref has a value matching expected (exact match).
const AssertValueScript = `(function(ref, expected, timeoutMs){` + FrameWalkHelpers + `
  function roots(){
    return __abRootList();
  }
  function findByRef(ref){
    const selector='[data-brw-ref="'+CSS.escape(ref)+'"]';
    for(const root of roots()){
      const el=root.querySelector&&root.querySelector(selector);
      if(el) return el;
    }
    return null;
  }
  function check(){
    var el=findByRef(ref);
    if(!el) return false;
    var val=('value' in el)?el.value:(el.innerText||el.textContent||'');
    return String(val)===expected;
  }
  return new Promise(function(resolve){
    if(check()){ resolve(true); return; }
    var done=false,iv=0,to=0;
    function finish(v){ if(done)return; done=true; if(iv)clearInterval(iv); if(to)clearTimeout(to); resolve(v); }
    try{ var obs=new MutationObserver(function(){ if(check()) finish(true); });
         obs.observe(document.documentElement||document, {subtree:true, childList:true, characterData:true, attributes:true}); }catch(e){}
    iv=setInterval(function(){ if(check()) finish(true); }, 100);
    to=setTimeout(function(){ finish(check()); }, Math.max(0, timeoutMs|0));
  });
})`

// AssertVisibleScript returns a Promise that resolves true when the element
// identified by ref is visible in the viewport.
const AssertVisibleScript = `(function(ref, timeoutMs){` + FrameWalkHelpers + `
  function roots(){
    return __abRootList();
  }
  function findByRef(ref){
    const selector='[data-brw-ref="'+CSS.escape(ref)+'"]';
    for(const root of roots()){
      const el=root.querySelector&&root.querySelector(selector);
      if(el) return el;
    }
    return null;
  }
  function visible(el){
    if(!el||el.nodeType!==1) return false;
    if(el.closest('[hidden],[aria-hidden="true"]')) return false;
    var w=(el.ownerDocument&&el.ownerDocument.defaultView)||window;
    var s=w.getComputedStyle(el);
    if(!s||s.display==='none'||s.visibility==='hidden'||Number(s.opacity)===0) return false;
    var r=el.getClientRects();
    return r&&r.length>0&&Array.from(r).some(function(x){return x.width>0&&x.height>0;});
  }
  function check(){ return visible(findByRef(ref)); }
  return new Promise(function(resolve){
    if(check()){ resolve(true); return; }
    var done=false,iv=0,to=0;
    function finish(v){ if(done)return; done=true; if(iv)clearInterval(iv); if(to)clearTimeout(to); resolve(v); }
    try{ var obs=new MutationObserver(function(){ if(check()) finish(true); });
         obs.observe(document.documentElement||document, {subtree:true, childList:true, characterData:true, attributes:true}); }catch(e){}
    iv=setInterval(function(){ if(check()) finish(true); }, 100);
    to=setTimeout(function(){ finish(check()); }, Math.max(0, timeoutMs|0));
  });
})`

// AssertHiddenScript returns a Promise that resolves true when the element
// identified by ref is hidden or absent from the DOM.
const AssertHiddenScript = `(function(ref, timeoutMs){` + FrameWalkHelpers + `
  function roots(){
    return __abRootList();
  }
  function findByRef(ref){
    const selector='[data-brw-ref="'+CSS.escape(ref)+'"]';
    for(const root of roots()){
      const el=root.querySelector&&root.querySelector(selector);
      if(el) return el;
    }
    return null;
  }
  function hidden(){
    var el=findByRef(ref);
    if(!el) return true;
    if(el.closest('[hidden],[aria-hidden="true"]')) return true;
    var w=(el.ownerDocument&&el.ownerDocument.defaultView)||window;
    var s=w.getComputedStyle(el);
    if(!s||s.display==='none'||s.visibility==='hidden'||Number(s.opacity)===0) return true;
    var r=el.getClientRects();
    return !r||r.length===0||!Array.from(r).some(function(x){return x.width>0&&x.height>0;});
  }
  return new Promise(function(resolve){
    if(hidden()){ resolve(true); return; }
    var done=false,iv=0,to=0;
    function finish(v){ if(done)return; done=true; if(iv)clearInterval(iv); if(to)clearTimeout(to); resolve(v); }
    try{ var obs=new MutationObserver(function(){ if(hidden()) finish(true); });
         obs.observe(document.documentElement||document, {subtree:true, childList:true, characterData:true, attributes:true}); }catch(e){}
    iv=setInterval(function(){ if(hidden()) finish(true); }, 100);
    to=setTimeout(function(){ finish(hidden()); }, Math.max(0, timeoutMs|0));
  });
})`

// EvalAssert evaluates an assertion script that returns a Promise<bool>.
func EvalAssert(ctx context.Context, script string, args ...any) error {
	// Marshal each arg and build the expression
	marshaled := make([]string, len(args))
	for i, a := range args {
		j, _ := json.Marshal(a)
		marshaled[i] = string(j)
	}
	expr := fmt.Sprintf("%s(%s)", script, strings.Join(marshaled, ","))
	var ok bool
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
			return fmt.Errorf("assertion failed: %s", details)
		}
		if obj == nil || len(obj.Value) == 0 {
			return nil
		}
		return json.Unmarshal(obj.Value, &ok)
	})); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("assertion did not pass within timeout")
	}
	return nil
}

func SetFileInputFiles(ctx context.Context, ref string, paths []string) error {
	if ref == "" {
		return errors.New("ref is required")
	}
	if len(paths) == 0 {
		return errors.New("at least one file path is required")
	}
	refJSON, _ := json.Marshal(ref)
	expr := fmt.Sprintf("%s(%s)", FileInputElementScript, refJSON)

	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		obj, exception, err := runtime.Evaluate(expr).
			WithObjectGroup("brw-upload").
			WithAwaitPromise(true).
			Do(ctx)
		if err != nil {
			return err
		}
		if exception != nil {
			details, _ := json.Marshal(exception)
			return fmt.Errorf("file input resolution failed: %s", details)
		}
		if obj == nil || obj.ObjectID == "" {
			return errors.New("file input resolution returned no object id")
		}
		defer func() { _ = runtime.ReleaseObject(obj.ObjectID).Do(ctx) }()
		return dom.SetFileInputFiles(paths).WithObjectID(obj.ObjectID).Do(ctx)
	})); err != nil {
		return err
	}
	return DispatchFileInputEvents(ctx, ref)
}

func DispatchFileInputEvents(ctx context.Context, ref string) error {
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	refJSON, _ := json.Marshal(ref)
	expr := fmt.Sprintf("%s(%s)", FileInputEventsScript, refJSON)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
		return err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "file input event dispatch failed"
		}
		return errors.New(result.Error)
	}
	return nil
}

const ClickXYScript = `(function(x, y) {
  var el = document.elementFromPoint(x, y);
  if (!el) return { ok: false, error: 'no element at coordinates' };
  var opts = { bubbles: true, cancelable: true, clientX: x, clientY: y, view: window };
  // Hover + focus first: many custom controls (hover-reveal menus, comboboxes,
  // disclosure widgets) only wire up their click handler after pointerover/focus.
  el.dispatchEvent(new PointerEvent('pointerover', opts));
  el.dispatchEvent(new MouseEvent('mouseover', opts));
  el.dispatchEvent(new PointerEvent('pointermove', opts));
  el.dispatchEvent(new MouseEvent('mousemove', opts));
  var focusTarget = (el.closest && el.closest('a[href],button,input,select,textarea,[tabindex],[contenteditable=""],[contenteditable="true"]')) || el;
  if (focusTarget && typeof focusTarget.focus === 'function') { try { focusTarget.focus({ preventScroll: true }); } catch (e) {} }
  el.dispatchEvent(new PointerEvent('pointerdown', opts));
  el.dispatchEvent(new MouseEvent('mousedown', opts));
  el.dispatchEvent(new PointerEvent('pointerup', opts));
  el.dispatchEvent(new MouseEvent('mouseup', opts));
  el.dispatchEvent(new MouseEvent('click', opts));
  var tag = el.tagName.toLowerCase();
  var role = el.getAttribute('role') || '';
  var name = (el.getAttribute('aria-label') || el.getAttribute('title') || el.innerText || '').slice(0, 100);
  return { ok: true, x: x, y: y, tag: tag, role: role, name: name.trim() };
})`

// MouseEventScript resolves a target by ref or x,y, then dispatches a full
// pointer + mouse event sequence with the requested button and click count.
// Used by the extension bridge (which has no direct CDP Input access) for
// right/double/triple/middle click. clickCount>1 fires the extra click events
// (dblclick for 2) the way browsers do for repeated clicks.
const MouseEventScript = `(function(opts) {
  opts = opts || {};
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
    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  function buttonConsts(name) {
    switch (String(name || 'left').toLowerCase()) {
      case 'right': return { button: 2, buttons: 2 };
      case 'middle': return { button: 1, buttons: 4 };
      case 'back': return { button: 3, buttons: 8 };
      case 'forward': return { button: 4, buttons: 16 };
      case 'none': return { button: 0, buttons: 0 };
      default: return { button: 0, buttons: 1 };
    }
  }
  let x = opts.x;
  let y = opts.y;
  let target = null;
  if (opts.ref) {
    target = findByRef(opts.ref);
    if (!target) return { ok: false, error: 'ref not found — the page likely changed; re-run brw_snapshot to get current refs' };
    if (target.closest('[hidden],[aria-hidden="true"]')) return { ok: false, error: 'ref hidden' };
    target.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
    const r = target.getBoundingClientRect();
    x = r.left + r.width / 2;
    y = r.top + r.height / 2;
  } else if (typeof x !== 'number' || typeof y !== 'number') {
    return { ok: false, error: 'mouse target requires either a ref or x and y coordinates' };
  } else {
    target = document.elementFromPoint(x, y);
    if (!target) return { ok: false, error: 'no element at coordinates' };
  }
  const bc = buttonConsts(opts.button);
  const clickCount = Math.max(1, Number(opts.click_count || 1));
  function pe(type, opt) { return new PointerEvent(type, opt); }
  function me(type, opt) { return new MouseEvent(type, opt); }
  const down = { bubbles: true, cancelable: true, composed: true, view: window, clientX: x, clientY: y, button: bc.button, buttons: bc.buttons };
  const up = { bubbles: true, cancelable: true, composed: true, view: window, clientX: x, clientY: y, button: bc.button, buttons: 0 };
  target.dispatchEvent(pe('pointermove', up));
  for (let i = 1; i <= clickCount; i++) {
    const clickOpts = Object.assign({}, up, { detail: i });
    target.dispatchEvent(pe('pointerdown', Object.assign({}, down, { detail: i })));
    target.dispatchEvent(me('mousedown', Object.assign({}, down, { detail: i })));
    target.dispatchEvent(pe('pointerup', clickOpts));
    target.dispatchEvent(me('mouseup', clickOpts));
    if (bc.button === 2) {
      target.dispatchEvent(me('contextmenu', clickOpts));
    } else if (bc.button === 0) {
      if (i === clickCount && typeof target.click === 'function') target.click();
      else target.dispatchEvent(me('click', clickOpts));
      if (i === 2) target.dispatchEvent(me('dblclick', Object.assign({}, clickOpts, { detail: 2 })));
    } else {
      target.dispatchEvent(me('auxclick', clickOpts));
    }
  }
  const tag = target.tagName.toLowerCase();
  const role = target.getAttribute('role') || '';
  const name = (target.getAttribute('aria-label') || target.getAttribute('title') || target.innerText || '').slice(0, 100);
  return { ok: true, x: x, y: y, tag: tag, role: role, name: name.trim(), href: target.href || target.getAttribute('href') || '' };
})`

// MouseHalfScript dispatches a single pointerdown+mousedown (down) or
// pointerup+mouseup (up) at a ref or x,y — the decomposed press-and-hold the
// extension bridge uses for mouse_down / mouse_up.
const MouseHalfScript = `(function(opts) {
  opts = opts || {};
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
    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  function buttonConsts(name) {
    switch (String(name || 'left').toLowerCase()) {
      case 'right': return { button: 2, buttons: 2 };
      case 'middle': return { button: 1, buttons: 4 };
      case 'back': return { button: 3, buttons: 8 };
      case 'forward': return { button: 4, buttons: 16 };
      case 'none': return { button: 0, buttons: 0 };
      default: return { button: 0, buttons: 1 };
    }
  }
  let x = opts.x;
  let y = opts.y;
  let target = null;
  if (opts.ref) {
    target = findByRef(opts.ref);
    if (!target) return { ok: false, error: 'ref not found — the page likely changed; re-run brw_snapshot to get current refs' };
    target.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
    const r = target.getBoundingClientRect();
    x = r.left + r.width / 2;
    y = r.top + r.height / 2;
  } else if (typeof x !== 'number' || typeof y !== 'number') {
    return { ok: false, error: 'mouse target requires either a ref or x and y coordinates' };
  } else {
    target = document.elementFromPoint(x, y);
    if (!target) return { ok: false, error: 'no element at coordinates' };
  }
  const bc = buttonConsts(opts.button);
  const isDown = String(opts.phase) === 'down';
  const opt = { bubbles: true, cancelable: true, composed: true, view: window, clientX: x, clientY: y, button: bc.button, buttons: isDown ? bc.buttons : 0 };
  if (isDown) {
    target.dispatchEvent(new PointerEvent('pointermove', Object.assign({}, opt, { buttons: 0 })));
    target.dispatchEvent(new PointerEvent('pointerdown', opt));
    target.dispatchEvent(new MouseEvent('mousedown', opt));
  } else {
    target.dispatchEvent(new PointerEvent('pointerup', opt));
    target.dispatchEvent(new MouseEvent('mouseup', opt));
  }
  const name = (target.getAttribute('aria-label') || target.getAttribute('title') || target.innerText || '').slice(0, 100);
  return { ok: true, x: x, y: y, tag: target.tagName.toLowerCase(), role: target.getAttribute('role') || '', name: name.trim(), href: target.href || target.getAttribute('href') || '' };
})`

// DragScript presses at a source (ref or x,y), emits interpolated pointermove +
// mousemove events with the button held, then releases at the target. Generic
// pointer-event drag the extension bridge uses for sliders/range inputs,
// drag-and-drop reorder, and canvas/map panning.
const DragScript = `(function(opts) {
  opts = opts || {};
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
    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  function buttonConsts(name) {
    switch (String(name || 'left').toLowerCase()) {
      case 'right': return { button: 2, buttons: 2 };
      case 'middle': return { button: 1, buttons: 4 };
      default: return { button: 0, buttons: 1 };
    }
  }
  function point(p) {
    if (p && p.ref) {
      const el = findByRef(p.ref);
      if (!el) return null;
      el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
      const r = el.getBoundingClientRect();
      return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
    }
    if (p && typeof p.x === 'number' && typeof p.y === 'number') return { x: p.x, y: p.y };
    return null;
  }
  const from = point(opts.from);
  if (!from) return { ok: false, error: 'drag source requires either a ref or x and y coordinates' };
  const to = point(opts.to);
  if (!to) return { ok: false, error: 'drag target requires either a ref or x and y coordinates' };
  const bc = buttonConsts(opts.button);
  let steps = Math.max(1, Number(opts.steps || 12));
  const src = document.elementFromPoint(from.x, from.y);
  if (!src) return { ok: false, error: 'no element at drag source' };
  function disp(el, type, x, y, buttons) {
    const opt = { bubbles: true, cancelable: true, composed: true, view: window, clientX: x, clientY: y, button: bc.button, buttons: buttons };
    if (type.startsWith('pointer')) el.dispatchEvent(new PointerEvent(type, opt));
    else el.dispatchEvent(new MouseEvent(type, opt));
  }
  disp(src, 'pointermove', from.x, from.y, 0);
  disp(src, 'pointerdown', from.x, from.y, bc.buttons);
  disp(src, 'mousedown', from.x, from.y, bc.buttons);
  let last = src;
  for (let i = 1; i <= steps; i++) {
    const t = i / steps;
    const mx = from.x + (to.x - from.x) * t;
    const my = from.y + (to.y - from.y) * t;
    const over = document.elementFromPoint(mx, my) || last;
    disp(over, 'pointermove', mx, my, bc.buttons);
    disp(over, 'mousemove', mx, my, bc.buttons);
    last = over;
  }
  const dst = document.elementFromPoint(to.x, to.y) || last;
  disp(dst, 'pointerup', to.x, to.y, 0);
  disp(dst, 'mouseup', to.x, to.y, 0);
  return { ok: true, from: from, to: to };
})`

const ClickTextScript = `(function(opts) {` + FrameWalkHelpers + `
  opts = opts || {};
  function clean(s) { return String(s || '').replace(/\s+/g, ' ').trim(); }
  function roots() {
    return __abRootList();
  }
  function all(selector) {
    const out = [];
    for (const root of roots()) {
      if (root.querySelectorAll) out.push(...Array.from(root.querySelectorAll(selector)));
    }
    return out;
  }
  function winFor(el) {
    var doc = el && el.ownerDocument;
    return (doc && doc.defaultView) || window;
  }
  function visible(el) {
    if (!el || el.nodeType !== 1) return false;
    if (el.closest('[hidden],[aria-hidden="true"]')) return false;
    const style = winFor(el).getComputedStyle(el);
    if (!style || style.display === 'none' || style.visibility === 'hidden' || Number(style.opacity) === 0) return false;
    const rects = el.getClientRects();
    return rects && rects.length > 0 && Array.from(rects).some(r => r.width > 0 && r.height > 0);
  }
  function disabled(el) {
    return Boolean(el.disabled || el.getAttribute('aria-disabled') === 'true');
  }
  function labelText(el) {
    if (!el) return '';
    if (el.labels && el.labels.length) return clean(Array.from(el.labels).map(l => l.innerText || l.textContent).join(' '));
    if (el.id) {
      const root = el.getRootNode && el.getRootNode();
      const labelRoot = root && root.querySelector ? root : document;
      const label = labelRoot.querySelector('label[for="' + CSS.escape(el.id) + '"]');
      if (label) return clean(label.innerText || label.textContent);
    }
    const parent = el.closest('label');
    return parent ? clean(parent.innerText || parent.textContent) : '';
  }
  function roleFor(el) {
    const explicit = clean(el.getAttribute('role'));
    if (explicit) return explicit.split(/\s+/)[0];
    const tag = el.tagName.toLowerCase();
    const type = (el.getAttribute('type') || '').toLowerCase();
    if (tag === 'a' && el.href) return 'link';
    if (tag === 'button' || type === 'button' || type === 'submit' || type === 'reset' || type === 'image') return 'button';
    if (tag === 'select') return el.multiple ? 'listbox' : 'combobox';
    if (tag === 'input') return type === 'search' ? 'searchbox' : 'textbox';
    if (tag === 'summary') return 'button';
    return 'generic';
  }
  function textFor(el) {
    const tag = el.tagName.toLowerCase();
    if (tag === 'input') return clean(el.value || el.getAttribute('value') || '');
    return clean(el.innerText || el.textContent || '');
  }
  function nameFor(el) {
    return clean(el.getAttribute('aria-label') || labelText(el) || el.getAttribute('alt') || el.getAttribute('title') || el.getAttribute('placeholder') || el.getAttribute('name') || textFor(el));
  }
  function clickableAncestor(el) {
    let n = el;
    while (n && n !== document.body && n.nodeType === Node.ELEMENT_NODE) {
      const role = roleFor(n);
      const tag = n.tagName.toLowerCase();
      if (tag === 'button' || tag === 'a' || role === 'button' || role === 'link' || role === 'option' || role === 'menuitem' || n.onclick || n.tabIndex >= 0) return n;
      n = n.parentElement || (n.getRootNode && n.getRootNode().host) || null;
    }
    return el;
  }
  const want = clean(opts.text).toLowerCase();
  if (!want) return { ok: false, error: 'text is required' };
  const roleFilter = clean(opts.role).toLowerCase();
  const exact = Boolean(opts.exact);
  const selector = [
    'button','a[href]','[role="button"]','[role="link"]','[role="option"]','[role="menuitem"]',
    'input[type="button"]','input[type="submit"]','summary','label','[tabindex]','[onclick]'
  ].join(',');
  const candidates = [];
  const seen = new Set();
  for (const base of all(selector)) {
    const el = clickableAncestor(base);
    if (!el || seen.has(el) || !visible(el) || disabled(el)) continue;
    seen.add(el);
    const role = roleFor(el);
    if (roleFilter && role !== roleFilter) continue;
    const label = nameFor(el);
    const text = textFor(el);
    const hay = clean([label, text].filter(Boolean).join(' '));
    const normalized = hay.toLowerCase();
    if (!normalized) continue;
    let score = -1;
    if (normalized === want || label.toLowerCase() === want || text.toLowerCase() === want) score = 120;
    else if (!exact && normalized.includes(want)) score = 80;
    else continue;
    if (['button','link','option','menuitem'].includes(role)) score += 20;
    const r = el.getBoundingClientRect();
    const inViewport = r.bottom >= 0 && r.right >= 0 && r.top <= window.innerHeight && r.left <= window.innerWidth;
    if (inViewport) score += 10;
    score -= Math.min(20, Math.round((r.width * r.height) / 50000));
    candidates.push({ el, role, label, text, score });
  }
  // Default-on auto-scroll: opts.auto_scroll === false opts out, undefined/true
  // keeps it. When on, a below-fold match is scrolled into view before clicking
  // so links beneath the fold work without a manual scrollIntoView. When off,
  // an off-screen best candidate is rejected so only in-viewport matches click.
  const autoScroll = opts.auto_scroll !== false;
  function inViewportNow(el) {
    const r = el.getBoundingClientRect();
    return r.bottom >= 0 && r.right >= 0 && r.top <= (window.innerHeight || document.documentElement.clientHeight) && r.left <= (window.innerWidth || document.documentElement.clientWidth);
  }
  candidates.sort((a, b) => b.score - a.score);
  let hit = candidates[0];
  if (!hit) return { ok: false, error: 'no visible element found for text ' + JSON.stringify(opts.text) };
  if (!autoScroll) {
    // Opt-out path: discard off-screen matches so only an in-viewport element is
    // clicked. Pick the best candidate that is currently within the viewport.
    hit = candidates.find(c => inViewportNow(c.el));
    if (!hit) return { ok: false, error: 'no in-viewport element found for text ' + JSON.stringify(opts.text) + ' (auto_scroll disabled)' };
  }
  const el = hit.el;
  if (autoScroll && !inViewportNow(el)) {
    el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  } else if (autoScroll) {
    // Already in view: a cheap centering scroll keeps the click point hit-testable
    // for elements partially clipped by sticky headers/footers.
    el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  }
  const r = el.getBoundingClientRect();
  // Clamp the click point into the viewport so elementFromPoint resolves the
  // element even when it is taller than the viewport after scrolling.
  const vw = window.innerWidth || document.documentElement.clientWidth;
  const vh = window.innerHeight || document.documentElement.clientHeight;
  const x = Math.max(1, Math.min(vw - 1, r.left + r.width / 2));
  const y = Math.max(1, Math.min(vh - 1, r.top + r.height / 2));
  let target = document.elementFromPoint(x, y) || el;
  target = clickableAncestor(target) || el;
  target.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, cancelable: true, clientX: x, clientY: y }));
  target.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true, clientX: x, clientY: y }));
  target.dispatchEvent(new PointerEvent('pointerup', { bubbles: true, cancelable: true, clientX: x, clientY: y }));
  target.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, cancelable: true, clientX: x, clientY: y }));
  if (typeof target.click === 'function') target.click();
  else target.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, clientX: x, clientY: y }));
  const clickedRole = roleFor(target);
  const clickedName = nameFor(target);
  return {
    ok: true,
    x,
    y,
    tag: target.tagName.toLowerCase(),
    role: clickedRole,
    name: clickedName,
    text: clean(target.innerText || target.textContent || hit.text || hit.label).slice(0, 200),
    href: target.href || target.getAttribute('href') || ''
  };
})`

const CommitFieldScript = `(function(ref) {` + FrameWalkHelpers + `
  function roots() {
    return __abRootList();
  }
  function findByRef(ref) {
    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  const el = findByRef(ref);
  if (!el) return { ok: false, error: 'ref not found — the page likely changed; re-run brw_snapshot to get current refs' };
  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  if (typeof el.focus === 'function') el.focus({ preventScroll: true });
  const form = el.closest('form');
  if (form) {
    const submitBtn = form.querySelector('button[type="submit"], input[type="submit"], button:not([type])');
    if (submitBtn) {
      submitBtn.click();
      return { ok: true, ref: ref, method: 'submit_button' };
    }
    form.requestSubmit();
    return { ok: true, ref: ref, method: 'form_submit' };
  }
  el.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', code: 'Enter', keyCode: 13, bubbles: true, cancelable: true }));
  el.dispatchEvent(new KeyboardEvent('keypress', { key: 'Enter', code: 'Enter', keyCode: 13, bubbles: true, cancelable: true }));
  el.dispatchEvent(new KeyboardEvent('keyup', { key: 'Enter', code: 'Enter', keyCode: 13, bubbles: true, cancelable: true }));
  return { ok: true, ref: ref, method: 'enter_key' };
})`

func CommitField(ctx context.Context, ref string) error {
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	refJSON, _ := json.Marshal(ref)
	expr := fmt.Sprintf("%s(%s)", CommitFieldScript, refJSON)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
		return err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "commit failed"
		}
		return errors.New(result.Error)
	}
	return nil
}

type ClickXYResult struct {
	OK    bool    `json:"ok"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Tag   string  `json:"tag,omitempty"`
	Role  string  `json:"role,omitempty"`
	Name  string  `json:"name,omitempty"`
	Text  string  `json:"text,omitempty"`
	Href  string  `json:"href,omitempty"`
	Error string  `json:"error,omitempty"`
}

func ClickXY(ctx context.Context, x, y float64) (ClickXYResult, error) {
	var result ClickXYResult
	xJSON, _ := json.Marshal(x)
	yJSON, _ := json.Marshal(y)
	expr := fmt.Sprintf("%s(%s,%s)", ClickXYScript, xJSON, yJSON)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
		return ClickXYResult{}, err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "click failed"
		}
		return result, errors.New(result.Error)
	}
	return result, nil
}

// MouseActionResult is the by-value result of an in-page mouse action
// (MouseEvent/MouseHalf/Drag) dispatched through the extension bridge.
type MouseActionResult struct {
	OK    bool    `json:"ok"`
	X     float64 `json:"x,omitempty"`
	Y     float64 `json:"y,omitempty"`
	Tag   string  `json:"tag,omitempty"`
	Role  string  `json:"role,omitempty"`
	Name  string  `json:"name,omitempty"`
	Href  string  `json:"href,omitempty"`
	Error string  `json:"error,omitempty"`
}

func ClickText(ctx context.Context, opts ClickTextOptions) (ClickXYResult, error) {
	var result ClickXYResult
	args, _ := json.Marshal(opts)
	expr := fmt.Sprintf("%s(%s)", ClickTextScript, args)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
		return ClickXYResult{}, err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "click text failed"
		}
		return result, errors.New(result.Error)
	}
	return result, nil
}
