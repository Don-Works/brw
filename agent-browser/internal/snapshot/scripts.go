package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const SnapshotFunctionScript = `(function(opts) {` + FrameWalkHelpers + `
  opts = opts || {};
  const state = window.__agentBrowser || (window.__agentBrowser = { next: 1, byKey: {}, byRef: {} });
  const includeHidden = Boolean(opts.include_hidden);
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
    'label'
  ];
  if (includeHidden) selectorParts.push('input[type="hidden"]');
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

  function refFor(el, key) {
    const existing = el.getAttribute('data-agent-browser-ref');
    if (existing) {
      state.byRef[existing] = key;
      state.byKey[key] = existing;
      return existing;
    }
    const prior = state.byKey[key];
    const ref = prior || ('e' + state.next++);
    state.byKey[key] = ref;
    state.byRef[ref] = key;
    try { el.setAttribute('data-agent-browser-ref', ref); } catch (_) {}
    return ref;
  }

  function disabled(el) {
    return Boolean(el.disabled || el.getAttribute('aria-disabled') === 'true');
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
  const viewportOnly = Boolean(opts.viewport_only) || frontierMode;
  const limit = Math.max(0, Number(opts.limit || 0));
  const active = deepActive(document);

  const formRoles = new Set(['textbox', 'searchbox', 'combobox', 'listbox', 'checkbox', 'radio', 'slider', 'spinbutton', 'button', 'switch', 'option', 'menuitem']);
  const seen = new Set();
  const elements = [];
  for (const el of all(selector)) {
    if (!isElementLike(el)) continue;
    if (seen.has(el)) continue;
    seen.add(el);
    const role = roleFor(el);
    const name = nameFor(el);
    const isFocusable = el.tabIndex >= 0;
    const isUseful = role !== 'generic' || isFocusable || typeof el.onclick === 'function';
    if (!isUseful) continue;
    if (formLensMode && !formRoles.has(role)) continue;
    const key = keyFor(el, role, name);
    const ref = refFor(el, key);
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
      item.value
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
      item.match_reasons = reasons;
    }
    elements.push(item);
    if (!frontierMode && limit > 0 && elements.length >= limit) break;
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
  const returned = limit > 0 ? elements.slice(0, limit) : elements;
  for (const item of returned) delete item._frontier_score;

  state.version = (state.version || 0) + 1;
  const focusedRef = active && active.getAttribute ? (active.getAttribute('data-agent-browser-ref') || '') : '';
  return {
    url: location.href,
    title: document.title || '',
    elements: returned,
    metadata: {
      generated_at: new Date().toISOString(),
      element_count: returned.length,
      total_candidates: totalCandidates,
      truncated: limit > 0 && totalCandidates > returned.length,
      mode: frontierMode ? 'frontier' : 'all',
      include_hidden: includeHidden,
      version: state.version,
      focused_ref: focusedRef
    }
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
  const state = window.__agentBrowser || (window.__agentBrowser = { next: 1, byKey: {}, byRef: {} });
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
    var existing = el.getAttribute('data-agent-browser-ref');
    if (existing) {
      state.byRef[existing] = key;
      state.byKey[key] = existing;
      return existing;
    }
    var prior = state.byKey[key];
    var ref = prior || ('e' + state.next++);
    state.byKey[key] = ref;
    state.byRef[ref] = key;
    try { el.setAttribute('data-agent-browser-ref', ref); } catch (_) {}
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
    const selector = '[data-agent-browser-ref="' + CSS.escape(ref) + '"]';
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
    const selector = '[data-agent-browser-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  const el = findByRef(ref);
  if (!el) return { ok: false, error: 'ref not found' };
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
    const selector = '[data-agent-browser-ref="' + CSS.escape(ref) + '"]';
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
  if (!el) return { ok: false, error: 'ref not found' };
  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  if (typeof el.focus === 'function') el.focus({ preventScroll: true });
  const current = ('value' in el) ? String(el.value || '') : String(el.textContent || '');
  const next = replace ? String(text || '') : current + String(text || '');
  if ('value' in el) {
    setNativeValue(el, next);
  } else if (el.isContentEditable || el.getAttribute('contenteditable') === 'true') {
    el.textContent = next;
  } else {
    return { ok: false, error: 'ref is not fillable' };
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
    const selector = '[data-agent-browser-ref="' + CSS.escape(ref) + '"]';
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
  if (!el) throw new Error('ref not found');
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
    const selector = '[data-agent-browser-ref="' + CSS.escape(ref) + '"]';
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
  if (!el) return { ok: false, error: 'ref not found' };
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
    const selector = '[data-agent-browser-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  const el = findByRef(ref);
  if (!el) return { ok: false, error: 'ref not found' };
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
	expr := fmt.Sprintf("%s(%s)", SnapshotFunctionScript, args)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &snap)); err != nil {
		return PageSnapshot{}, err
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
		return box, fmt.Errorf("element ref %q not found or not visible", ref)
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
		return fmt.Errorf("element ref %q not found or could not be focused", ref)
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

// WaitConditionScript returns a Promise that resolves true as soon as the given
// condition holds, or false after timeoutMs. It checks immediately, then re-checks
// on DOM mutations (MutationObserver) and history events, with a 100ms safety
// interval for signals those miss (e.g. pushState URL changes) — replacing a
// fixed-interval CDP poll loop with a single awaited in-page promise.
const WaitConditionScript = `(function(condition, timeoutMs){` + FrameWalkHelpers + `
  if (!condition || condition === 'load') condition = 'ready';
  function roots(){
    return __abRootList();
  }
  function hasRef(ref){
    const selector='[data-agent-browser-ref="'+CSS.escape(ref)+'"]';
    return roots().some(root => root.querySelector && root.querySelector(selector));
  }
  function check(){
    if(condition==='ready') return document.readyState==='complete'||document.readyState==='interactive';
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

// WaitForActionableScript returns a Promise that resolves true when the element
// identified by ref is visible, stable (bounding box unchanged for two
// consecutive checks 100ms apart), and enabled. Resolves false on timeout.
const WaitForActionableScript = `(function(ref, timeoutMs){` + FrameWalkHelpers + `
  function roots(){
    return __abRootList();
  }
  function findByRef(ref){
    const selector='[data-agent-browser-ref="'+CSS.escape(ref)+'"]';
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
    var state=window.__agentBrowser||(window.__agentBrowser={next:1,byKey:{},byRef:{}});
    var existing=el.getAttribute('data-agent-browser-ref');
    if(existing){ state.byRef[existing]=key; state.byKey[key]=existing; return existing; }
    var prior=state.byKey[key];
    var newRef=prior||('e'+state.next++);
    state.byKey[key]=newRef;
    state.byRef[newRef]=key;
    try{ el.setAttribute('data-agent-browser-ref',newRef); }catch(_){}
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
    var state=window.__agentBrowser;
    var key=state&&state.byRef&&state.byRef[ref];
    if(!key) return null;
    var parts=key.split('|');
    var recovered=recoverByRoleAndName(parts[1]||'',parts[2]||'',parts[0]||'',parts[6]||'');
    if(!recovered) return null;
    var newKey=[parts[0]||'',parts[1]||'',parts[2]||'',recovered.id||'',recovered.getAttribute('name')||'',recovered.getAttribute('href')||'',parts[6]||'',recovered.getAttribute('aria-controls')||'',''].join('|');
    refFor(recovered,newKey);
    return recovered;
  }
  function visible(el){
    if(!el||el.nodeType!==1) return false;
    if(el.closest('[hidden],[aria-hidden="true"]')) return false;
    var w=(el.ownerDocument&&el.ownerDocument.defaultView)||window;
    const s=w.getComputedStyle(el);
    if(!s||s.display==='none'||s.visibility==='hidden'||Number(s.opacity)===0) return false;
    const r=el.getClientRects();
    return r&&r.length>0&&Array.from(r).some(function(x){return x.width>0&&x.height>0;});
  }
  function boxKey(el){
    const r=el.getBoundingClientRect();
    return [r.x,r.y,r.width,r.height].join(',');
  }
  function check(){
    var el=findByRef(ref);
    if(!el) return {ok:false,reason:'not_found'};
    if(!visible(el)) return {ok:false,reason:'not_visible'};
    if(el.disabled||el.getAttribute('aria-disabled')==='true') return {ok:false,reason:'disabled'};
    return {ok:true};
  }
  return new Promise(function(resolve){
    var c=check();
    if(!c.ok&&c.reason!=='not_visible'){ resolve(false); return; }
    var done=false,stable=false,lastBox='',iv=0,to=0;
    function tick(){
      c=check();
      if(!c.ok){ stable=false; return; }
      var bk=boxKey(findByRef(ref));
      if(bk===lastBox){ stable=true; } else { stable=false; lastBox=bk; }
    }
    function finish(v){ if(done)return; done=true; if(iv)clearInterval(iv); if(to)clearTimeout(to); resolve(v); }
    tick();
    iv=setInterval(function(){ tick(); if(stable) finish(true); }, 100);
    to=setTimeout(function(){ tick(); finish(stable); }, Math.max(0, timeoutMs|0));
  });
})`

// WaitForActionable waits for the element identified by ref to become visible,
// stable, and enabled within timeoutMs. Returns nil on success, error on timeout.
func WaitForActionable(ctx context.Context, ref string, timeoutMs int64) error {
	refJSON, _ := json.Marshal(ref)
	expr := fmt.Sprintf("%s(%s,%d)", WaitForActionableScript, refJSON, timeoutMs)
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
			return fmt.Errorf("actionable wait failed: %s", details)
		}
		if obj == nil || len(obj.Value) == 0 {
			return nil
		}
		return json.Unmarshal(obj.Value, &ok)
	})); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("element ref %q not actionable within %dms", ref, timeoutMs)
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
    const selector='[data-agent-browser-ref="'+CSS.escape(ref)+'"]';
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
    const selector='[data-agent-browser-ref="'+CSS.escape(ref)+'"]';
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
    const selector='[data-agent-browser-ref="'+CSS.escape(ref)+'"]';
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
    const selector='[data-agent-browser-ref="'+CSS.escape(ref)+'"]';
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
			WithObjectGroup("agent-browser-upload").
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
  el.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, cancelable: true, clientX: x, clientY: y }));
  el.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true, clientX: x, clientY: y }));
  el.dispatchEvent(new PointerEvent('pointerup', { bubbles: true, cancelable: true, clientX: x, clientY: y }));
  el.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, cancelable: true, clientX: x, clientY: y }));
  el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, clientX: x, clientY: y }));
  var tag = el.tagName.toLowerCase();
  var role = el.getAttribute('role') || '';
  var name = (el.getAttribute('aria-label') || el.getAttribute('title') || el.innerText || '').slice(0, 100);
  return { ok: true, x: x, y: y, tag: tag, role: role, name: name.trim() };
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
  candidates.sort((a, b) => b.score - a.score);
  const hit = candidates[0];
  if (!hit) return { ok: false, error: 'no visible element found for text ' + JSON.stringify(opts.text) };
  const el = hit.el;
  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  const r = el.getBoundingClientRect();
  const x = r.left + r.width / 2;
  const y = r.top + r.height / 2;
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
    const selector = '[data-agent-browser-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  const el = findByRef(ref);
  if (!el) return { ok: false, error: 'ref not found' };
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
