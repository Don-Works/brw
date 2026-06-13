package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/chromedp/chromedp"
)

const SnapshotScript = `(function() {
  const state = window.__agentBrowser || (window.__agentBrowser = { next: 1, byKey: {}, byRef: {} });
  const selector = [
    'a[href]',
    'button',
    'input:not([type="hidden"])',
    'textarea',
    'select',
    '[role]',
    '[contenteditable="true"]',
    '[tabindex]',
    'summary',
    'label'
  ].join(',');

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
      source: ['dom'],
      key
    };
    if (checked !== null) item.checked = checked;
    elements.push(item);
  }

  return {
    url: location.href,
    title: document.title || '',
    elements,
    metadata: {
      generated_at: new Date().toISOString(),
      element_count: elements.length
    }
  };
})()`

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
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
  el.dispatchEvent(new Event('change', { bubbles: true }));
  return { ok: true, ref, value: el.value };
})`

func Evaluate(ctx context.Context) (PageSnapshot, error) {
	var snap PageSnapshot
	if err := chromedp.Run(ctx, chromedp.Evaluate(SnapshotScript, &snap)); err != nil {
		return PageSnapshot{}, err
	}
	for i := range snap.Elements {
		if len(snap.Elements[i].Source) == 0 {
			snap.Elements[i].Source = []string{"dom"}
		}
	}
	return snap, nil
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
