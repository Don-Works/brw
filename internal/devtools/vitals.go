package devtools

import (
	"encoding/json"
	"fmt"
)

// Vitals settle bounds. A PerformanceObserver created after load replays the
// buffered timeline asynchronously, so a read that resolved synchronously would
// report zero for every metric. The default is the shortest wait that reliably
// drains the buffer; the ceiling stops a caller turning a read into a sleep.
const (
	DefaultVitalsSettleMS = 250
	MaxVitalsSettleMS     = 5000
)

type VitalsOptions struct {
	// SettleMS is how long to let the buffered performance timeline drain
	// before answering. Zero means DefaultVitalsSettleMS.
	SettleMS int    `json:"settle_ms,omitempty"`
	TabID    string `json:"tab_id,omitempty"`
}

// Normalize clamps the settle window into the supported range.
func (o VitalsOptions) Normalize() VitalsOptions {
	if o.SettleMS <= 0 {
		o.SettleMS = DefaultVitalsSettleMS
	}
	if o.SettleMS > MaxVitalsSettleMS {
		o.SettleMS = MaxVitalsSettleMS
	}
	return o
}

// Vitals is one reading of the Core Web Vitals for the document currently
// loaded in the target tab. Every duration is milliseconds from the navigation
// start; a metric the page has not produced yet is null rather than zero,
// because "no largest paint yet" and "painted instantly" are different facts.
type Vitals struct {
	URL   string `json:"url"`
	Title string `json:"title"`

	LCPMS      *float64 `json:"lcp_ms"`
	LCPElement string   `json:"lcp_element,omitempty"`
	// CLS is null, not zero, when this browser cannot observe layout-shift at
	// all: "nothing moved" and "nobody was watching" are different facts, and
	// only one of them deserves a "good" rating.
	CLS       *float64 `json:"cls"`
	CLSShifts int      `json:"cls_shifts"`
	INPMS     *float64 `json:"inp_ms"`
	// Interactions is how many distinct interactions the timeline retained, not
	// how many event-timing entries: one tap emits pointerdown, pointerup and
	// click sharing a single interactionId and counts once.
	Interactions int      `json:"interactions"`
	TTFBMS       *float64 `json:"ttfb_ms"`
	FCPMS        *float64 `json:"fcp_ms"`

	DOMContentLoadedMS *float64 `json:"dom_content_loaded_ms"`
	LoadMS             *float64 `json:"load_ms"`
	NavigationType     string   `json:"navigation_type,omitempty"`
	TransferBytes      int64    `json:"transfer_bytes,omitempty"`

	// Ratings label each metric against the published Core Web Vitals
	// thresholds, so a caller does not have to carry the numbers. A metric this
	// browser could not observe is rated "unknown".
	Ratings map[string]string `json:"ratings,omitempty"`
	// Unavailable names the entry types this browser does not support, taken
	// from PerformanceObserver.supportedEntryTypes. Every metric derived from
	// one of them is null.
	Unavailable []string `json:"unavailable,omitempty"`
	SettledMS   int      `json:"settled_ms"`
	Note        string   `json:"note,omitempty"`
}

// BuildVitalsExpression renders the read for one settle window. Only the
// fields the in-page script reads are marshalled into the argument: tab_id is
// daemon-side routing and has no business crossing into the document.
func BuildVitalsExpression(opts VitalsOptions) string {
	args, _ := json.Marshal(map[string]any{"settle_ms": opts.Normalize().SettleMS})
	return fmt.Sprintf("%s(%s)", VitalsScript, args)
}

// VitalsScript reads the Core Web Vitals out of the page's own performance
// timeline. It is a pure read: it registers observers, waits for the buffered
// replay, disconnects them and resolves. Nothing is left installed in the page.
//
// Each observer is created with buffered:true, which replays entries recorded
// before brw attached. That is what makes this work on a page brw did not open,
// and it is also the honest limit on INP — the event-timing buffer only retains
// interactions at or above the browser's own 104 ms threshold, so a page whose
// every interaction was fast reports no INP rather than a small one.
const VitalsScript = `(function(opts) {
  return new Promise(function(resolve) {
    var settleMs = (opts && opts.settle_ms) || 250;
    var observers = [];
    var unavailable = [];
    var watching = {};
    var lcpEntry = null;
    var sessions = [];
    var shiftCount = 0;
    var interactionMax = {};
    var fcp = null;

    // observe() reports whether the metric is being watched at all. The
    // Performance Timeline spec says observe() with one unsupported type warns
    // and returns rather than throwing, so a try/catch alone would leave
    // unavailable permanently empty and an unobservable metric reading as zero.
    function observe(type, init, onEntry) {
      var supported = false;
      try {
        var types = PerformanceObserver.supportedEntryTypes || [];
        supported = Array.prototype.indexOf.call(types, type) >= 0;
      } catch (_) { supported = false; }
      if (!supported) {
        unavailable.push(type);
        return false;
      }
      try {
        var po = new PerformanceObserver(function(list) {
          var entries = list.getEntries();
          for (var i = 0; i < entries.length; i++) onEntry(entries[i]);
        });
        var config = { type: type, buffered: true };
        for (var key in (init || {})) config[key] = init[key];
        po.observe(config);
        observers.push(po);
        return true;
      } catch (e) {
        unavailable.push(type);
        return false;
      }
    }

    watching.lcp = observe('largest-contentful-paint', null, function(e) {
      if (!lcpEntry || e.startTime >= lcpEntry.startTime) lcpEntry = e;
    });
    // The session-window algorithm the Core Web Vitals definition uses: shifts
    // group into a window that ends after a 1s gap or 5s of elapsed time, and
    // CLS is the worst window, not the sum of everything the page ever did.
    watching.cls = observe('layout-shift', null, function(e) {
      if (e.hadRecentInput) return;
      shiftCount++;
      var last = sessions.length ? sessions[sessions.length - 1] : null;
      if (last && e.startTime - last.last < 1000 && e.startTime - last.first < 5000) {
        last.value += e.value;
        last.last = e.startTime;
      } else {
        sessions.push({ value: e.value, first: e.startTime, last: e.startTime });
      }
    });
    // One interaction emits several timed events — pointerdown, pointerup and
    // click all carry the same interactionId — and the spec's INP is the worst
    // duration per interaction, not per event. Keying by interactionId is what
    // stops one tap being counted three times and dragging the percentile down
    // onto a faster event than the slowest one.
    watching.inp = observe('event', { durationThreshold: 16 }, function(e) {
      if (!e.interactionId) return;
      var id = String(e.interactionId);
      if (interactionMax[id] === undefined || e.duration > interactionMax[id]) {
        interactionMax[id] = e.duration;
      }
    });
    watching.fcp = observe('paint', null, function(e) {
      if (e.name === 'first-contentful-paint' && fcp === null) fcp = e.startTime;
    });

    function describe(el) {
      if (!el || !el.tagName) return '';
      var out = String(el.tagName).toLowerCase();
      try {
        if (el.id) out += '#' + el.id;
        else if (typeof el.className === 'string' && el.className.trim()) {
          out += '.' + el.className.trim().split(/\s+/)[0];
        }
        var ref = el.getAttribute && el.getAttribute('data-brw-ref');
        if (ref) out += ' @' + ref;
      } catch (_) {}
      return out;
    }
    function ms(value) {
      if (value === null || value === undefined || !isFinite(value)) return null;
      return Math.round(value * 10) / 10;
    }
    function rate(value, good, poor) {
      if (value === null || value === undefined) return 'unknown';
      if (value <= good) return 'good';
      if (value <= poor) return 'needs-improvement';
      return 'poor';
    }

    function finish() {
      for (var i = 0; i < observers.length; i++) {
        try { observers[i].disconnect(); } catch (_) {}
      }
      var cls = null;
      if (watching.cls) {
        cls = 0;
        for (var s = 0; s < sessions.length; s++) if (sessions[s].value > cls) cls = sessions[s].value;
        cls = Math.round(cls * 10000) / 10000;
      }

      var nav = (performance.getEntriesByType('navigation') || [])[0] || null;
      var activation = nav && nav.activationStart ? nav.activationStart : 0;
      var ttfb = nav ? Math.max(0, nav.responseStart - activation) : null;
      // domContentLoadedEventEnd/loadEventEnd read 0 until the event fires, and
      // 0 would be indistinguishable from "instant". Report null instead.
      var dcl = nav && nav.domContentLoadedEventEnd ? nav.domContentLoadedEventEnd - activation : null;
      var load = nav && nav.loadEventEnd ? nav.loadEventEnd - activation : null;

      var durations = [];
      for (var id in interactionMax) durations.push(interactionMax[id]);
      durations.sort(function(a, b) { return b - a; });
      // INP is the 98th percentile of interaction latency. With few
      // interactions that is the slowest one, which is what the spec's index
      // formula degenerates to below fifty.
      var inp = durations.length ? durations[Math.floor(durations.length / 50)] : null;
      var lcp = lcpEntry ? lcpEntry.startTime : null;

      resolve({
        url: location.href,
        title: document.title || '',
        lcp_ms: ms(lcp),
        lcp_element: describe(lcpEntry && lcpEntry.element),
        cls: cls,
        cls_shifts: shiftCount,
        inp_ms: ms(inp),
        interactions: durations.length,
        ttfb_ms: ms(ttfb),
        fcp_ms: ms(fcp),
        dom_content_loaded_ms: ms(dcl),
        load_ms: ms(load),
        navigation_type: nav ? String(nav.type || '') : '',
        transfer_bytes: nav && nav.transferSize ? nav.transferSize : 0,
        ratings: {
          lcp: rate(ms(lcp), 2500, 4000),
          cls: rate(cls, 0.1, 0.25),
          inp: rate(ms(inp), 200, 500),
          ttfb: rate(ms(ttfb), 800, 1800)
        },
        unavailable: unavailable,
        settled_ms: settleMs,
        note: document.prerendering ? 'page is still prerendering; timings are relative to activation' : ''
      });
    }
    setTimeout(finish, settleMs);
  });
})`
