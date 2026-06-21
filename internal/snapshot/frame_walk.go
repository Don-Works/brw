package snapshot

// FrameWalkHelpers is a shared block of in-page JavaScript helper functions that
// teach the snapshot/find/resolve walkers to descend into BOTH open shadow roots
// and same-origin <iframe> documents.
//
// The browser exposes an iframe's document only when it is same-origin
// (accessible contentDocument); cross-origin frames throw on access, so every
// frame descent is wrapped in try/catch and silently skipped. Recursion depth is
// bounded by MAX_FRAME_DEPTH so a pathological deeply-nested document cannot hang
// the walker.
//
// Refs remain stable and unambiguous across frames because the ref attribute
// (data-brw-ref) is written onto the element in its OWN document, and
// lookups (__abFindDeep) walk the same frame tree to locate it again. Coordinates
// are returned in TOP-LEVEL viewport space: each root carries the cumulative
// (ox, oy) pixel offset of the iframe element(s) that contain it, so a
// frame-local getBoundingClientRect() is translated back to the top document's
// coordinate system that the CDP MouseClickXY path expects.
//
// All public walker scripts (SnapshotFunctionScript, ResolveBoxScript, etc.)
// prepend this block and call __abRoots()/__abFindDeep() instead of redefining a
// shadow-only roots()/findByRef() locally.
const FrameWalkHelpers = `
  var MAX_FRAME_DEPTH = 8;
  // __abRoots memoizes its frame/shadow walk, but ONLY while armed. The
  // synchronous snapshot walk (SnapshotFunctionScript) sets __abRootsCacheArmed so
  // its many all()/__abFindDeep()/detectVisualIslands() calls share one
  // querySelectorAll('*') frame walk instead of repeating it. The async
  // wait/assert poll scripts never arm it, so every poll recomputes the root list
  // — a shadow root or same-origin iframe attached DURING a wait is always
  // discovered (a stale cache there would cause spurious wait/assert timeouts).
  var __abRootsCache = null;
  var __abRootsCacheArmed = false;
  // __abInaccessibleFrames accumulates cross-origin / sandboxed <iframe> boxes
  // seen during the last __abRootsCompute() (the browser isolates their DOM, so
  // they cannot be read as semantic refs). The snapshot surfaces them in metadata
  // so an agent can fall back to brw_screenshot + brw_click_xy. Reset every
  // compute; only the snapshot reads it, so async pollers harmlessly clobber it.
  var __abInaccessibleFrames = [];
  // __abEnsureShadowPierce monkeypatches Element.prototype.attachShadow so CLOSED
  // shadow roots keep a side reference (__brwShadow) on their host. The page's own
  // el.shadowRoot stays null — encapsulation is intact for page scripts; only the
  // brw walker reads __brwShadow. Idempotent and page-tamper-safe via a window
  // guard, so it is cheap to call on every walk. It only captures roots attached
  // AFTER it runs; direct-CDP additionally installs the same patch at
  // document-start (ShadowPierceInstallScript) to catch earlier-mounted roots.
  function __abEnsureShadowPierce() {
    if (window.__brwShadowPierce) return;
    window.__brwShadowPierce = true;
    try {
      var o = Element.prototype.attachShadow;
      if (typeof o !== 'function') return;
      Element.prototype.attachShadow = function(init) {
        var r = o.call(this, init);
        try {
          if (init && init.mode === 'closed') {
            Object.defineProperty(this, '__brwShadow', { value: r, configurable: true });
          }
        } catch (_) {}
        return r;
      };
    } catch (_) {}
  }
  function __abRoots() {
    if (__abRootsCacheArmed && __abRootsCache) return __abRootsCache;
    var computed = __abRootsCompute();
    if (__abRootsCacheArmed) __abRootsCache = computed;
    return computed;
  }
  // __abRootsCompute returns an array of { root, ox, oy, depth } descriptors
  // covering the top document, every open shadowRoot, and every reachable
  // same-origin iframe document. ox/oy are the cumulative top-level viewport
  // offsets of the frame chain containing that root (0,0 for the top document and
  // its shadow roots).
  function __abRootsCompute() {
    __abEnsureShadowPierce();
    __abInaccessibleFrames = [];
    var out = [{ root: document, ox: 0, oy: 0, depth: 0 }];
    for (var i = 0; i < out.length; i++) {
      var entry = out[i];
      var root = entry.root;
      if (!root || !root.querySelectorAll) continue;
      var all;
      try { all = Array.from(root.querySelectorAll('*')); } catch (_) { continue; }
      for (var j = 0; j < all.length; j++) {
        var el = all[j];
        var sr = el.shadowRoot || el.__brwShadow;
        if (sr) {
          out.push({ root: sr, ox: entry.ox, oy: entry.oy, depth: entry.depth });
        }
        if (el.tagName && el.tagName.toLowerCase() === 'iframe' && entry.depth < MAX_FRAME_DEPTH) {
          var doc = null;
          try { doc = el.contentDocument; } catch (_) { doc = null; }
          if (doc && doc.querySelectorAll) {
            var fr;
            try { fr = el.getBoundingClientRect(); } catch (_) { fr = null; }
            if (fr) {
              // The iframe's content box is inset from its border box by border +
              // padding; approximate with clientLeft/clientTop which cover the
              // border, the dominant offset for typical embeds.
              var bx = entry.ox + fr.left + (el.clientLeft || 0);
              var by = entry.oy + fr.top + (el.clientTop || 0);
              out.push({ root: doc, ox: bx, oy: by, depth: entry.depth + 1 });
            }
          } else {
            // Cross-origin (or sandboxed) iframe with a real src: the browser
            // isolates its document, so it cannot be walked for semantic refs.
            // Record its top-level box + origin so the agent can fall back to
            // brw_screenshot + brw_click_xy instead of going silently blind.
            var xsrc = '';
            try { xsrc = el.getAttribute('src') || ''; } catch (_) { xsrc = ''; }
            if (xsrc && xsrc.indexOf('about:') !== 0 && xsrc.indexOf('javascript:') !== 0) {
              var xfr = null;
              try { xfr = el.getBoundingClientRect(); } catch (_) { xfr = null; }
              if (xfr && xfr.width > 0 && xfr.height > 0) {
                var xorigin = '';
                try { xorigin = new URL(el.src, location.href).origin; } catch (_) { xorigin = ''; }
                __abInaccessibleFrames.push({
                  x: Math.round(entry.ox + xfr.left),
                  y: Math.round(entry.oy + xfr.top),
                  width: Math.round(xfr.width),
                  height: Math.round(xfr.height),
                  origin: xorigin
                });
              }
            }
          }
        }
      }
    }
    return out;
  }
  // __abRootList returns just the document/shadowRoot nodes (offset-agnostic) for
  // callers that only need to query elements, not translate coordinates.
  function __abRootList() {
    return __abRoots().map(function(e) { return e.root; });
  }
  // __abFindDeep locates an element by ref across the whole frame tree and returns
  // { el, ox, oy } where ox/oy are the top-level viewport offsets to add to the
  // element's frame-local getBoundingClientRect(). Returns null when not found.
  function __abFindDeep(ref) {
    if (!ref) return null;
    var selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
    var entries = __abRoots();
    for (var i = 0; i < entries.length; i++) {
      var entry = entries[i];
      var el = null;
      try { el = entry.root.querySelector && entry.root.querySelector(selector); } catch (_) { el = null; }
      if (el) return { el: el, ox: entry.ox, oy: entry.oy };
    }
    return null;
  }
`
