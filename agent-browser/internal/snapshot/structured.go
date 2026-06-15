package snapshot

import (
	"context"

	"github.com/chromedp/chromedp"
)

// StructuredData is a compact normalized view of embedded page structured data.
type StructuredData struct {
	URL          string   `json:"url,omitempty"`
	Source       string   `json:"source"`
	Title        string   `json:"title,omitempty"`
	Type         string   `json:"type,omitempty"`
	Name         string   `json:"name,omitempty"`
	Price        string   `json:"price,omitempty"`
	Currency     string   `json:"currency,omitempty"`
	Availability string   `json:"availability,omitempty"`
	Rating       string   `json:"rating,omitempty"`
	ReviewCount  string   `json:"reviewCount,omitempty"`
	Brand        string   `json:"brand,omitempty"`
	Images       []string `json:"images,omitempty"`
	Raw          any      `json:"raw,omitempty"`
}

const StructuredDataScript = `(function() {
  var MAX_RAW = 4000;
  function clean(s) { return String(s || '').replace(/\s+/g, ' ').trim(); }
  function str(v) {
    if (v == null) return '';
    if (typeof v === 'string' || typeof v === 'number' || typeof v === 'boolean') return clean(v);
    if (typeof v === 'object' && v.name) return clean(v.name);
    return '';
  }
  function arr(v) {
    if (!v) return [];
    if (Array.isArray(v)) return v.map(function(x) { return clean(x); }).filter(Boolean);
    var one = clean(v);
    return one ? [one] : [];
  }
  function trunc(obj) {
    try {
      var s = JSON.stringify(obj);
      if (s.length <= MAX_RAW) return obj;
      return { _truncated: true, preview: s.slice(0, MAX_RAW) };
    } catch (e) { return null; }
  }
  function offerOf(o) {
    if (!o || typeof o !== 'object') return null;
    var offers = o.offers;
    if (Array.isArray(offers)) return offers[0] || null;
    if (offers && typeof offers === 'object') return offers;
    return null;
  }
  function ratingOf(o) {
    if (!o || typeof o !== 'object') return null;
    return o.aggregateRating || o.reviewRating || null;
  }
  function normalize(source, raw) {
    var o = raw && typeof raw === 'object' ? raw : {};
    var offer = offerOf(o);
    var rating = ratingOf(o);
    var type = str(o['@type'] || o.type || o['og:type']);
    var avail = str((offer && offer.availability) || o.availability);
    if (avail.indexOf('/') !== -1) avail = avail.split('/').pop();
    return {
      url: location.href,
      source: source,
      title: clean(document.title || o.title || o.headline || o.name),
      type: type,
      name: str(o.name || o.headline || o.title),
      price: str((offer && (offer.price || offer.lowPrice)) || o.price),
      currency: str((offer && offer.priceCurrency) || o.priceCurrency || o.currency),
      availability: avail,
      rating: str((rating && rating.ratingValue) || o.ratingValue || o.rating),
      reviewCount: str((rating && rating.reviewCount) || o.reviewCount),
      brand: str(o.brand),
      images: arr(o.image || o.images || o.photo),
      raw: trunc(raw)
    };
  }
  function parseJSON(text) {
    try { return JSON.parse(text); } catch (e) { return null; }
  }
  function fromNextData() {
    var el = document.getElementById('__NEXT_DATA__');
    if (!el || !el.textContent) return null;
    var data = parseJSON(el.textContent);
    if (!data) return null;
    var props = data.props || {};
    var page = props.pageProps || props.initialProps || props;
    if (page && page.product) return page.product;
    if (page && (page.name || page.title || page.price)) return page;
    if (page && page.item) return page.item;
    return page && typeof page === 'object' ? page : data;
  }
  function flattenJsonLd(blocks) {
    var out = [];
    blocks.forEach(function(b) {
      if (!b) return;
      if (Array.isArray(b)) { out = out.concat(flattenJsonLd(b)); return; }
      if (b['@graph']) { out = out.concat(flattenJsonLd(b['@graph'])); return; }
      out.push(b);
    });
    return out;
  }
  function pickEntity(items) {
    var order = ['Product', 'ProductGroup', 'Offer', 'Article', 'NewsArticle', 'WebPage', 'Organization'];
    for (var i = 0; i < order.length; i++) {
      var hit = items.find(function(it) {
        var t = it && it['@type'];
        if (Array.isArray(t)) return t.indexOf(order[i]) !== -1;
        return t === order[i];
      });
      if (hit) return hit;
    }
    return items[0] || null;
  }
  function fromJsonLd() {
    var nodes = document.querySelectorAll('script[type="application/ld+json"]');
    var blocks = [];
    nodes.forEach(function(n) {
      var parsed = parseJSON(n.textContent || '');
      if (parsed) blocks.push(parsed);
    });
    if (!blocks.length) return null;
    var items = flattenJsonLd(blocks);
    var entity = pickEntity(items);
    // Backfill aggregateRating from a sibling node (e.g. the ProductGroup parent
    // of a variant Product) when the picked entity lacks one. Catalogs commonly
    // put the rating on the group and price/offer on the specific variant, so a
    // first-source-wins pick would otherwise drop the rating entirely.
    if (entity && typeof entity === 'object' && !entity.aggregateRating) {
      for (var i = 0; i < items.length; i++) {
        if (items[i] && items[i].aggregateRating) {
          try { entity = Object.assign({}, entity, { aggregateRating: items[i].aggregateRating }); }
          catch (e) { entity.aggregateRating = items[i].aggregateRating; }
          break;
        }
      }
    }
    return entity;
  }
  function microProp(scope, name) {
    var el = scope.querySelector('[itemprop="' + name + '"]');
    if (!el) return '';
    if (el.getAttribute('content')) return clean(el.getAttribute('content'));
    if (el.getAttribute('href')) return clean(el.getAttribute('href'));
    return clean(el.textContent);
  }
  function fromMicrodata() {
    var scopes = document.querySelectorAll('[itemscope]');
    if (!scopes.length) return null;
    var scope = scopes[0];
    var type = clean(scope.getAttribute('itemtype') || '');
    if (type.indexOf('/') !== -1) type = type.split('/').pop();
    var offerScope = scope.querySelector('[itemprop="offers"][itemscope]') || scope;
    return {
      '@type': type,
      name: microProp(scope, 'name'),
      brand: microProp(scope, 'brand'),
      image: microProp(scope, 'image'),
      offers: {
        price: microProp(offerScope, 'price'),
        priceCurrency: microProp(offerScope, 'priceCurrency'),
        availability: microProp(offerScope, 'availability')
      },
      aggregateRating: {
        ratingValue: microProp(scope, 'ratingValue'),
        reviewCount: microProp(scope, 'reviewCount')
      }
    };
  }
  function meta(name) {
    var el = document.querySelector('meta[property="' + name + '"], meta[name="' + name + '"]');
    return el ? clean(el.getAttribute('content')) : '';
  }
  function fromMeta() {
    var title = meta('og:title') || meta('twitter:title');
    var type = meta('og:type');
    var name = title || meta('product:name');
    var price = meta('product:price:amount') || meta('og:price:amount');
    var currency = meta('product:price:currency') || meta('og:price:currency');
    if (!title && !type && !name && !price) return null;
    return {
      title: title,
      type: type,
      name: name,
      price: price,
      priceCurrency: currency,
      image: meta('og:image'),
      brand: meta('product:brand') || meta('og:site_name')
    };
  }
  function fromInlineScripts() {
    var scripts = document.querySelectorAll('script:not([src]):not([type="application/ld+json"])');
    for (var i = 0; i < scripts.length; i++) {
      var text = scripts[i].textContent || '';
      var start = text.indexOf('{');
      if (start === -1) continue;
      var end = text.lastIndexOf('}');
      if (end <= start) continue;
      var parsed = parseJSON(text.slice(start, end + 1));
      if (!parsed || typeof parsed !== 'object') continue;
      if (parsed.name || parsed.title || parsed.price || parsed.product || parsed['@type']) {
        return parsed.product || parsed;
      }
    }
    return null;
  }
  var chain = [
    ['next_data', fromNextData],
    ['json_ld', fromJsonLd],
    ['microdata', fromMicrodata],
    ['meta', fromMeta],
    ['inline_script', fromInlineScripts]
  ];
  for (var i = 0; i < chain.length; i++) {
    var raw = chain[i][1]();
    if (raw) return normalize(chain[i][0], raw);
  }
  return { url: location.href, source: 'none', title: clean(document.title || '') };
})()`

func EvaluateStructured(ctx context.Context) (StructuredData, error) {
	var data StructuredData
	if err := chromedp.Run(ctx, chromedp.Evaluate(StructuredDataScript, &data)); err != nil {
		return StructuredData{}, err
	}
	return data, nil
}