package browser

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// reactScript is the in-page fiber walker. It returns an object shaped like
// ReactResult so chromedp.Evaluate can unmarshal it directly. It reads the fiber
// tree React attaches to the DOM (the __reactFiber$ / __reactContainer$ keys),
// which needs no page-side hook and no DevTools backend.
const reactScript = `function (action, target, depth, limit) {` + snapshot.FrameWalkHelpers + `
  function fiberKey(el) {
    var ks = Object.keys(el);
    for (var i = 0; i < ks.length; i++) { if (ks[i].indexOf('__reactFiber$') === 0) return ks[i]; }
    return null;
  }
  function containerKey(el) {
    var ks = Object.keys(el);
    for (var i = 0; i < ks.length; i++) { if (ks[i].indexOf('__reactContainer$') === 0) return ks[i]; }
    return null;
  }
  function typeName(t) {
    if (t == null) return null;
    if (typeof t === 'string') return t;
    if (typeof t === 'function') return t.displayName || t.name || 'Anonymous';
    if (typeof t === 'object') {
      if (typeof t.displayName === 'string' && t.displayName) return t.displayName;
      var inner = t.render || t.type;
      if (typeof inner === 'function') return inner.displayName || inner.name || 'Anonymous';
      if (typeof inner === 'string') return inner;
      if (t.$$typeof) { try { return String(t.$$typeof).replace(/^Symbol\(react\./, '').replace(/\)$/, ''); } catch (e) {} }
    }
    return 'Unknown';
  }
  function kindOf(t) {
    if (t == null) return 'root';
    if (typeof t === 'string') return 'host';
    if (typeof t === 'function') return 'component';
    if (typeof t === 'object' && t.$$typeof) return 'component';
    return 'other';
  }
  function findRoot() {
    var candidates = [document.body].concat(Array.prototype.slice.call(document.querySelectorAll('body *')));
    for (var i = 0; i < candidates.length; i++) {
      var el = candidates[i];
      if (!el) continue;
      var ck = containerKey(el);
      if (ck) return el[ck];
    }
    return null;
  }
  if (action === 'tree') {
    var root = findRoot();
    if (!root) return { ok: true, action: 'tree', present: false, count: 0, note: 'no React root found on this page' };
    var nodes = [];
    var over = false;
    function visit(f, d) {
      if (!f) return;
      if (nodes.length >= limit || d > depth) { if (nodes.length >= limit) over = true; return; }
      var t = f.type;
      var name = typeName(t);
      var kind = kindOf(t);
      if (kind === 'root') name = 'HostRoot';
      if (name) {
        nodes.push({ depth: d, name: name, kind: kind, tag: (typeof t === 'string' ? t : undefined), key: (f.key != null ? String(f.key) : undefined) });
      }
      var c = f.child;
      while (c) { visit(c, d + 1); c = c.sibling; }
    }
    visit(root, 0);
    var res = { ok: true, action: 'tree', present: true, nodes: nodes, count: nodes.length };
    if (over) res.note = 'tree truncated at ' + limit + ' nodes or depth ' + depth;
    return res;
  }
  if (action === 'inspect') {
    var el = null;
    var hit = __abFindDeep(target);
    if (hit) el = hit.el;
    if (!el) { try { el = document.querySelector(target); } catch (e) { el = null; } }
    if (!el) return { ok: false, action: 'inspect', present: true, count: 0, note: 'target not found; take a brw_snapshot first if target is a ref' };
    var fk = fiberKey(el);
    if (!fk) return { ok: true, action: 'inspect', present: true, count: 0, note: 'the element is not part of a React tree' };
    var f = el[fk];
    var ancestors = [];
    var comp = null;
    var cur = f;
    while (cur) {
      var k = kindOf(cur.type);
      var n = typeName(cur.type);
      if (k === 'component') { if (!comp) comp = cur; if (n) ancestors.push(n); }
      cur = cur.return;
    }
    if (!comp) return { ok: true, action: 'inspect', present: true, count: 0, note: 'no React component owns this element' };
    var props = {};
    var mp = comp.memoizedProps || {};
    var names = Object.keys(mp).slice(0, 40);
    for (var i = 0; i < names.length; i++) {
      var k2 = names[i]; var v = mp[k2];
      if (v === null) props[k2] = 'null';
      else if (typeof v === 'string') props[k2] = v.length > 200 ? v.slice(0, 200) + '…' : v;
      else if (typeof v === 'number' || typeof v === 'boolean') props[k2] = String(v);
      else if (typeof v === 'function') props[k2] = '[function]';
      else if (Array.isArray(v)) props[k2] = '[array ' + v.length + ']';
      else if (typeof v === 'object') props[k2] = '[object ' + ((v.constructor && v.constructor.name) || 'Object') + ']';
      else props[k2] = String(v);
    }
    var hooks = [];
    var h = comp.memoizedState;
    var guard = 0;
    while (h && guard < 50) { hooks.push(typeof h.memoizedState); h = h.next; guard++; }
    return { ok: true, action: 'inspect', present: true, count: 1, inspect: {
      component: typeName(comp.type) || 'Anonymous', kind: 'component',
      key: (comp.key != null ? String(comp.key) : undefined),
      props: props, hook_kinds: hooks, ancestors: ancestors.slice(0, 20)
    } };
  }
  var hook = window.__REACT_DEVTOOLS_GLOBAL_HOOK__;
  return { ok: true, action: action, present: !!findRoot(), count: 0,
    note: hook ? 'the page has a React DevTools hook, but brw does not attach to it for render/suspense data'
               : 'action=' + action + ' needs the React DevTools hook, which brw does not inject; use action=tree or action=inspect' };
}`

// ReactExpression builds the in-page expression for a validated React request.
// It is exported so the extension-bridge transport can run the same walker
// through its own Evaluate rather than keeping a second copy.
func ReactExpression(req ReactOptions) string {
	action, _ := json.Marshal(req.Action)
	target, _ := json.Marshal(req.Target)
	return fmt.Sprintf("(%s)(%s, %s, %d, %d)", reactScript, action, target, req.Depth, req.Limit)
}

// React reads a page's React component tree through the fiber tree React
// attaches to the DOM.
func (m *Manager) React(ctx context.Context, opts ReactOptions) (ReactResult, error) {
	req, err := NormalizeReact(opts)
	if err != nil {
		return ReactResult{}, err
	}
	if err := GuardCrossOriginRefs("react introspect", GenericCrossOriginRemedy, req.Target); err != nil {
		return ReactResult{}, err
	}
	tabID, tabCtx, cancel, err := m.contextForTab(ctx, opts.TabID)
	if err != nil {
		return ReactResult{}, err
	}
	defer cancel()

	var out ReactResult
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(ReactExpression(req), &out)); err != nil {
		return ReactResult{}, fmt.Errorf("read the React tree: %w", err)
	}
	out.Action = req.Action
	out.TabID = tabID
	return out, nil
}
