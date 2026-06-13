package readability

import (
	"context"

	"github.com/chromedp/chromedp"
)

const ReadScript = `(function() {
  function clean(s) {
    return String(s || '').replace(/\s+/g, ' ').trim();
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
    const direct = document.querySelector('article, main, [role="main"], .article, .post, .content');
    if (direct && text(direct).length > 120) return direct;
    let best = document.body;
    let bestScore = 0;
    for (const el of Array.from(document.body.querySelectorAll('article,main,section,div'))) {
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
    return best || document.body;
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
    return clean(el.getAttribute('aria-label') || labelText(el) || el.getAttribute('placeholder') || el.getAttribute('name') || el.getAttribute('title') || text(el));
  }

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
      .map(el => ({
        ref: el.getAttribute('data-agent-browser-ref') || '',
        role: roleFor(el),
        name: nameFor(el),
        type: clean(el.getAttribute('type') || ''),
        value: ('value' in el) ? clean(el.value) : '',
        required: Boolean(el.required || el.getAttribute('aria-required') === 'true'),
        disabled: Boolean(el.disabled || el.getAttribute('aria-disabled') === 'true')
      }))
  }));

  const tables = Array.from(document.querySelectorAll('table')).filter(visible).slice(0, 20).map(table => {
    const caption = table.querySelector('caption') ? text(table.querySelector('caption')) : '';
    const headers = Array.from(table.querySelectorAll('thead th, tr:first-child th')).map(th => text(th)).filter(Boolean);
    const rows = Array.from(table.querySelectorAll('tbody tr, tr')).slice(0, 40).map(tr =>
      Array.from(tr.querySelectorAll('th,td')).map(cell => text(cell))
    ).filter(row => row.length);
    return { caption, headers, rows };
  });

  return {
    url: location.href,
    title: document.title || '',
    main: text(mainEl).slice(0, 100000),
    headings,
    links,
    forms,
    tables,
    metadata
  };
})()`

func Evaluate(ctx context.Context) (PageRead, error) {
	var read PageRead
	if err := chromedp.Run(ctx, chromedp.Evaluate(ReadScript, &read)); err != nil {
		return PageRead{}, err
	}
	return read, nil
}
