package readability

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// readMinMainLen is the threshold below which .main is considered too short to be
// useful, triggering the document-text fallback and (on CSR/SPA shells) the brief
// content-settle wait before giving up.
const readMinMainLen = 50

// readSettleCapMS bounds how long ReadScript waits for client-side-rendered
// content to populate the DOM when the first synchronous extraction comes back
// empty/near-empty. It resolves the MOMENT real text appears (MutationObserver),
// so well-formed pages pay nothing and only blank SPA shells wait — and never
// longer than this cap.
const readSettleCapMS = 800

const ReadScript = `(function(minMainLen, settleCapMs) {
  minMainLen = Number(minMainLen) || 50;
  settleCapMs = Math.max(0, Number(settleCapMs) || 0);
  function clean(s) {
    return String(s || '').replace(/\s+/g, ' ').trim();
  }
  // deepBodyText gathers visible text from the document body AND any open shadow
  // roots, so a CSR page that renders its content inside web components (whose
  // innerText does NOT include shadow content) still yields a usable body-text
  // fallback. Bounded by the same 100k slice the primary path uses.
  function deepBodyText() {
    var fb = document.body || document.documentElement;
    var base = fb ? clean(fb.innerText || fb.textContent || '') : '';
    var parts = base ? [base] : [];
    try {
      var hosts = (fb || document).querySelectorAll('*');
      for (var i = 0; i < hosts.length; i++) {
        var sr = hosts[i].shadowRoot;
        if (sr) {
          var st = clean(sr.textContent || '');
          if (st) parts.push(st);
        }
      }
    } catch (_) {}
    return clean(parts.join(' '));
  }
  function visible(el) {
    if (!el || !(el instanceof Element)) return false;
    if (el.closest('[hidden],[aria-hidden="true"]')) return false;
    const style = getComputedStyle(el);
    if (style.display === 'none' || style.visibility === 'hidden') return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  }
  function text(el) {
    return clean(el ? (el.innerText || el.textContent || '') : '');
  }
  function bestMain() {
    const body = document.body || document.documentElement;
    if (!body) return document.documentElement;
    const direct = document.querySelector('article, main, [role="main"], .article, .post, .content');
    if (direct && text(direct).length > 120) return direct;
    let best = body;
    let bestScore = 0;
    for (const el of Array.from(body.querySelectorAll('article,main,section,div'))) {
      if (!visible(el)) continue;
      const t = text(el);
      if (t.length < 120) continue;
      const linkText = Array.from(el.querySelectorAll('a')).map(a => text(a)).join(' ');
      const score = t.length - linkText.length * 0.7 + el.querySelectorAll('p,li,h1,h2,h3').length * 30;
      if (score > bestScore) {
        best = el;
        bestScore = score;
      }
    }
    return best || body;
  }
  function roleFor(el) {
    const explicit = clean(el.getAttribute('role'));
    if (explicit) return explicit.split(/\s+/)[0];
    const tag = el.tagName.toLowerCase();
    const type = (el.getAttribute('type') || '').toLowerCase();
    if (tag === 'a' && el.href) return 'link';
    if (tag === 'button' || ['button', 'submit', 'reset', 'image'].includes(type)) return 'button';
    if (tag === 'textarea') return 'textbox';
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
  function labelText(el) {
    if (el.labels && el.labels.length) return clean(Array.from(el.labels).map(l => l.innerText || l.textContent).join(' '));
    if (el.id) {
      const label = document.querySelector('label[for="' + CSS.escape(el.id) + '"]');
      if (label) return text(label);
    }
    const parent = el.closest('label');
    return parent ? text(parent) : '';
  }
  function nameFor(el) {
    return clean(el.getAttribute('aria-label') || labelText(el) || el.getAttribute('placeholder') || el.getAttribute('name') || el.getAttribute('title') || (sensitive(el) ? '' : ((('value' in el) ? el.value : '') || text(el))));
  }
  function sensitive(el) {
    if (!el || !el.tagName) return false;
    const tag = el.tagName.toLowerCase();
    if (tag !== 'input' && tag !== 'textarea' && tag !== 'select') return false;
    const type = (el.getAttribute('type') || '').toLowerCase();
    if (type === 'password' || type === 'hidden') return true;
    const ac = (el.getAttribute('autocomplete') || '').toLowerCase();
    const hints = ['current-password', 'new-password', 'one-time-code', 'cc-number', 'cc-csc', 'cc-exp', 'cc-exp-month', 'cc-exp-year', 'cc-name', 'cc-type', 'cc-given-name', 'cc-family-name'];
    for (const hint of hints) { if (ac.includes(hint)) return true; }
    return false;
  }

  // extract() runs one full readability pass against the CURRENT DOM. It is
  // re-runnable so the CSR settle loop below can retry it as content streams in.
  function extract() {
  const mainEl = bestMain();
  const metadata = { open_graph: {} };
  const desc = document.querySelector('meta[name="description"]');
  if (desc) metadata.description = clean(desc.getAttribute('content'));
  const canonical = document.querySelector('link[rel="canonical"]');
  if (canonical) metadata.canonical = canonical.href || canonical.getAttribute('href') || '';
  metadata.lang = document.documentElement.lang || '';
  for (const m of Array.from(document.querySelectorAll('meta[property^="og:"]'))) {
    metadata.open_graph[m.getAttribute('property')] = clean(m.getAttribute('content'));
  }

  const headings = Array.from(document.querySelectorAll('h1,h2,h3,h4,h5,h6'))
    .filter(visible)
    .map(h => ({ level: Number(h.tagName.substring(1)), text: text(h), id: h.id || '' }))
    .filter(h => h.text);

  const links = Array.from(document.querySelectorAll('a[href]'))
    .filter(visible)
    .slice(0, 300)
    .map(a => ({ ref: a.getAttribute('data-agent-browser-ref') || '', text: text(a) || clean(a.href), href: a.href }))
    .filter(a => a.href);

  const forms = Array.from(document.querySelectorAll('form')).filter(visible).map(form => ({
    ref: form.getAttribute('data-agent-browser-ref') || '',
    name: clean(form.getAttribute('name') || form.getAttribute('aria-label') || ''),
    action: form.action || form.getAttribute('action') || '',
    method: clean(form.method || form.getAttribute('method') || 'get').toLowerCase(),
    controls: Array.from(form.querySelectorAll('input:not([type="hidden"]),textarea,select,button'))
      .filter(visible)
      .map(el => {
        const isSensitive = sensitive(el);
        return {
          ref: el.getAttribute('data-agent-browser-ref') || '',
          role: roleFor(el),
          name: nameFor(el),
          type: clean(el.getAttribute('type') || ''),
          value: isSensitive ? '' : (('value' in el) ? clean(el.value) : ''),
          sensitive: isSensitive || undefined,
          required: Boolean(el.required || el.getAttribute('aria-required') === 'true'),
          disabled: Boolean(el.disabled || el.getAttribute('aria-disabled') === 'true')
        };
      })
  }));

  const tables = Array.from(document.querySelectorAll('table')).filter(visible).slice(0, 20).map(table => {
    const caption = table.querySelector('caption') ? text(table.querySelector('caption')) : '';
    const headers = Array.from(table.querySelectorAll('thead th, tr:first-child th')).map(th => text(th)).filter(Boolean);
    const rows = Array.from(table.querySelectorAll('tbody tr, tr')).slice(0, 40).map(tr =>
      Array.from(tr.querySelectorAll('th,td')).map(cell => text(cell))
    ).filter(row => row.length);
    return { caption, headers, rows };
  });

  // Primary extraction is the scored semantic main element. On link-heavy pages
  // (Hacker News, Wikipedia category lists) and SPA shells the bestMain()
  // heuristic can fall back to <body> whose innerText is empty/near-empty, so
  // .main came back blank even though links extracted fine (they query a[href]
  // directly, bypassing the heuristic). When the primary text is too short to be
  // useful, fall back to the cleaned full-document text — now shadow-DOM aware so
  // content rendered inside web components is captured. This only activates on
  // failed/near-empty primary extraction, so well-formed article pages are
  // unaffected.
  let main = text(mainEl).slice(0, 100000);
  if (main.length < minMainLen) {
    const fallback = deepBodyText();
    if (fallback.length > main.length) main = fallback.slice(0, 100000);
  }

  return {
    url: location.href,
    title: document.title || '',
    main: main,
    headings,
    links,
    forms,
    tables,
    metadata
  };
  }

  // CSR/SPA settle: a heavy client-side-rendered page often serves a near-empty
  // shell on first paint and streams the real content in milliseconds later, so a
  // single synchronous extract() returns blank .main. If the first pass is
  // too short to be useful AND there is a settle budget, wait for the DOM to
  // produce real text (MutationObserver, with a short interval safety net) and
  // re-extract — resolving the MOMENT content appears, bounded by settleCapMs.
  // A page that already has content resolves immediately and pays nothing.
  return new Promise(function(resolve) {
    var first = extract();
    if (settleCapMs <= 0 || (first.main && first.main.length >= minMainLen)) {
      resolve(first);
      return;
    }
    var done = false, obs = null, iv = 0, to = 0;
    function finish() {
      if (done) return; done = true;
      try { if (obs) obs.disconnect(); } catch (e) {}
      if (iv) clearInterval(iv);
      if (to) clearTimeout(to);
      // Final re-extract so we return the freshest content seen.
      var out = extract();
      // Never regress below the first pass.
      if (!out.main || out.main.length < (first.main || '').length) out.main = first.main;
      resolve(out);
    }
    function recheck() {
      var r = extract();
      if (r.main && r.main.length >= minMainLen) { first = r; finish(); }
    }
    try {
      obs = new MutationObserver(recheck);
      obs.observe(document.documentElement || document, { subtree: true, childList: true, characterData: true });
    } catch (e) {}
    iv = setInterval(recheck, 60);
    to = setTimeout(finish, settleCapMs);
  });
})`

// ReadExpr returns the full ReadScript invocation expression (with the
// min-main-length and CSR settle-cap arguments applied) that resolves to a
// Promise<PageRead>. Both the direct-CDP path and the extension bridge use it so
// the script is invoked — not left as a bare function definition — and awaited.
func ReadExpr() string {
	return fmt.Sprintf("(%s)(%d,%d)", ReadScript, readMinMainLen, readSettleCapMS)
}

func Evaluate(ctx context.Context) (PageRead, error) {
	var read PageRead
	expr := ReadExpr()
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
			return fmt.Errorf("read failed: %s", details)
		}
		if obj == nil || len(obj.Value) == 0 {
			return nil
		}
		return json.Unmarshal(obj.Value, &read)
	})); err != nil {
		return PageRead{}, err
	}
	return read, nil
}
