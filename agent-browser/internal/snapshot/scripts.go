package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const SnapshotFunctionScript = `(function(opts) {
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

  function all(selector) {
    const out = [];
    for (const root of roots()) {
      if (root.querySelectorAll) out.push(...Array.from(root.querySelectorAll(selector)));
    }
    return out;
  }

  function visible(el) {
    if (!el || !(el instanceof Element)) return false;
    if (el.closest('[hidden],[aria-hidden="true"]')) return false;
    const style = window.getComputedStyle(el);
    if (!style || style.display === 'none' || style.visibility === 'hidden' || Number(style.opacity) === 0) return false;
    const rects = el.getClientRects();
    return rects && rects.length > 0 && Array.from(rects).some(r => r.width > 0 && r.height > 0);
  }

  function inViewport(el) {
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && r.bottom >= 0 && r.right >= 0 && r.top <= window.innerHeight && r.left <= window.innerWidth;
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
      textFor(el)
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
  const viewportOnly = Boolean(opts.viewport_only);
  const limit = Math.max(0, Number(opts.limit || 0));
  const active = deepActive(document);

  const seen = new Set();
  const elements = [];
  for (const el of all(selector)) {
    if (!(el instanceof HTMLElement) && !(el instanceof SVGElement)) continue;
    if (seen.has(el)) continue;
    seen.add(el);
    const role = roleFor(el);
    const name = nameFor(el);
    const isFocusable = el.tabIndex >= 0;
    const isUseful = role !== 'generic' || isFocusable || typeof el.onclick === 'function';
    if (!isUseful) continue;
    const key = keyFor(el, role, name);
    const ref = refFor(el, key);
    const checked = ('checked' in el) ? Boolean(el.checked) : null;
    const selected = ('selected' in el) ? Boolean(el.selected) : (el.getAttribute('aria-selected') === 'true' ? true : (el.getAttribute('aria-selected') === 'false' ? false : null));
    const expanded = el.getAttribute('aria-expanded') === 'true' ? true : (el.getAttribute('aria-expanded') === 'false' ? false : null);
    const signals = structuralSignals(el, role, active);
    const item = {
      ref,
      role,
      name,
      tag: el.tagName.toLowerCase(),
      type: (el.getAttribute('type') || '').toLowerCase(),
      href: el.href || el.getAttribute('href') || '',
      value: ('value' in el) ? clean(el.value) : '',
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
    if (checked !== null) item.checked = checked;
    if (selected !== null) item.selected = selected;
    if (expanded !== null) item.expanded = expanded;
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

const ResolveBoxScript = `(function(ref) {
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
    const selector = '[data-agent-browser-ref="' + CSS.escape(ref) + '"]';
    for (const root of roots()) {
      const el = root.querySelector && root.querySelector(selector);
      if (el) return el;
    }
    return null;
  }
  const el = findByRef(ref);
  if (!el) return { ok: false, ref };
  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  const r = el.getBoundingClientRect();
  return {
    ok: r.width > 0 && r.height > 0,
    ref,
    x: r.left + window.scrollX,
    y: r.top + window.scrollY,
    width: r.width,
    height: r.height,
    viewport_x: r.left + r.width / 2,
    viewport_y: r.top + r.height / 2
  };
})`

const FocusElementScript = `(function(ref) {
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

const SelectElementScript = `(function(ref, value) {
  function clean(s) {
    return String(s || '').replace(/\s+/g, ' ').trim();
  }
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

const FillElementScript = `(function(ref, text, replace) {
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

const FileInputElementScript = `(function(ref) {
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

const FileInputEventsScript = `(function(ref) {
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
    if (!el || !(el instanceof Element)) return false;
    if (el.closest('[hidden],[aria-hidden="true"]')) return false;
    const style = window.getComputedStyle(el);
    if (!style || style.display === 'none' || style.visibility === 'hidden' || Number(style.opacity) === 0) return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && r.bottom >= 0 && r.right >= 0 && r.top <= window.innerHeight && r.left <= window.innerWidth;
  }

  function canMove(el) {
    if (!el || !(el instanceof Element)) return false;
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
const WaitConditionScript = `(function(condition, timeoutMs){
  if (!condition || condition === 'load') condition = 'ready';
  function roots(){
    const out=[document];
    for(let i=0;i<out.length;i++){
      const root=out[i];
      if(!root.querySelectorAll) continue;
      for(const el of Array.from(root.querySelectorAll('*'))){ if(el.shadowRoot) out.push(el.shadowRoot); }
    }
    return out;
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
