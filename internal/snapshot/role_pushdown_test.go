package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

func lateRoleSnapshot(t *testing.T) string {
	t.Helper()
	s := strings.Replace(SnapshotFunctionScript, "    if (roleFilter && role !== roleFilter) continue;\n", "", 1)
	if s == SnapshotFunctionScript {
		t.Fatal("early role filter missing")
	}
	return strings.Replace(s, "    if (textFilter && !haystack.includes(textFilter)) continue;", "    if (roleFilter && item.role !== roleFilter) continue;\n    if (textFilter && !haystack.includes(textFilter)) continue;", 1)
}

func TestRolePushdownRandomizedDOM(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank")); err != nil {
		t.Fatal(err)
	}
	expr := `(() => {
      const oldWalk = ` + lateRoleSnapshot(t) + `;
      const newWalk = ` + SnapshotFunctionScript + `;
      let seed = 20261001;
      const rand = n => {seed=(Math.imul(seed,1664525)+1013904223)>>>0; return seed%n;};
      const roles = ['button','textbox','combobox','link','checkbox'];
      document.body.innerHTML = '<main></main><section></section><iframe></iframe>';
      const roots = [document.querySelector('main'), document.querySelector('section').attachShadow({mode:'open'}), document.querySelector('iframe').contentDocument.body];
      const create = () => {
        const el = document.createElement('input');
        el.setAttribute('role', roles[rand(roles.length)]);
        el.setAttribute('aria-label', 'Control '+rand(12));
        el.value = 'value '+rand(10);
        return el;
      };
      for (const root of roots) for(let i=0;i<60;i++) root.append(create());
      oldWalk({mode:'all',include_hidden:true});
      let checked = 0;
      for(let round=0;round<120;round++) {
        const root = roots[rand(roots.length)];
        const el = root.children[rand(root.children.length)];
        switch(rand(6)) {
          case 0: root.prepend(el); break;
          case 1: el.replaceWith(create()); break;
          case 2: el.setAttribute('role', roles[rand(roles.length)]); break;
          case 3: el.disabled = !el.disabled; break;
          case 4: el.value = 'value '+rand(10); break;
          case 5: root.append(create()); root.firstElementChild.remove(); break;
        }
        for (const role of roles) {
          const opts = {mode:round%2?'all':'frontier', role, limit:round%3?0:4, query:round%4?'':'Control 1', include_hidden:true};
          const stateBefore = JSON.stringify(window.__brw);
          const stamps = roots.flatMap(root => Array.from(root.children).map(el => [el, el.getAttribute('data-brw-ref')]));
          const before = oldWalk(opts);
          window.__brw = JSON.parse(stateBefore);
          for(const [el,ref] of stamps) { if(ref===null) el.removeAttribute('data-brw-ref'); else el.setAttribute('data-brw-ref',ref); }
          const after = newWalk(opts);
          if(JSON.stringify(before.elements)!==JSON.stringify(after.elements)) return {ok:false,round,role,details:JSON.stringify({before:before.elements.filter((e,i)=>JSON.stringify(e)!==JSON.stringify(after.elements[i])).slice(0,1),after:after.elements.filter((e,i)=>JSON.stringify(e)!==JSON.stringify(before.elements[i])).slice(0,1)})};
          checked++;
        }
      }
      return {ok:true,checked};
    })()`
	var result struct {
		OK      bool   `json:"ok"`
		Checked int    `json:"checked"`
		Round   int    `json:"round"`
		Role    string `json:"role"`
		Details string `json:"details"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Checked != 600 {
		t.Fatalf("differential failure: %+v", result)
	}
	t.Logf("%d role-filtered views matched across randomized DOM mutations", result.Checked)
}

func TestRolePushdownColdRefsAndSkippedWork(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	var result struct {
		Count        int  `json:"count"`
		SkippedReads int  `json:"skipped_reads"`
		Bound        bool `json:"bound"`
		Stable       bool `json:"stable"`
	}
	expr := `(() => {
      document.body.innerHTML = '<main>' + Array.from({length:1000},(_,i)=>'<a href="#'+i+'">Noise '+i+'</a>').join('') + '<input aria-label="Search"><input aria-label="Search"></main>';
      let skippedReads = 0;
      for(const el of document.querySelectorAll('a')) {
        el.getBoundingClientRect = () => {skippedReads++; return new DOMRect(0,0,10,10);};
      }
      const walk = ` + SnapshotFunctionScript + `;
      const first = walk({mode:'all',role:'textbox'});
      const skipped = skippedReads;
      const bound = first.elements.every(e => document.querySelectorAll('[data-brw-ref="'+e.ref+'"]').length===1);
      walk({mode:'all'});
      const second = walk({mode:'all',role:'textbox'});
      return {count:first.elements.length,skipped_reads:skipped,bound,stable:JSON.stringify(first.elements)===JSON.stringify(second.elements)};
    })()`
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank"), chromedp.Evaluate(expr, &result)); err != nil {
		t.Fatal(err)
	}
	if result.Count != 2 || result.SkippedReads != 0 || !result.Bound || !result.Stable {
		t.Fatalf("filtered cold walk: %+v", result)
	}
}

func TestRolePushdownLiveMeasurement(t *testing.T) {
	if os.Getenv("BRW_MEASURE_LIVE_ROLE") != "1" {
		t.Skip("set BRW_MEASURE_LIVE_ROLE=1 for public-site paired measurements")
	}
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	for _, site := range []struct{ url, role string }{
		{"https://en.wikipedia.org/wiki/Bloom_filter", "searchbox"},
		{"https://developer.mozilla.org/en-US/docs/Web/API/MutationObserver", "button"},
		{"https://news.ycombinator.com/", "textbox"},
	} {
		if err := chromedp.Run(ctx, chromedp.Navigate(site.url), chromedp.WaitReady("body")); err != nil {
			t.Fatal(err)
		}
		install := `window.__roleWalkers = [` + lateRoleSnapshot(t) + `,` + SnapshotFunctionScript + `]`
		if err := chromedp.Run(ctx, chromedp.Evaluate(install, nil)); err != nil {
			t.Fatal(err)
		}
		samples := [2][]float64{}
		counts := [2][]int{}
		for round := 0; round < 9; round++ {
			for turn := 0; turn < 2; turn++ {
				arm := (round + turn) % 2
				var result struct {
					MS    float64 `json:"ms"`
					Count int     `json:"count"`
					Chars int     `json:"chars"`
				}
				expr := fmt.Sprintf(`(() => {const t=performance.now();const r=window.__roleWalkers[%d]({mode:'all',role:%q,__brw_version:(window.__v=(window.__v||0)+1),__brw_epoch:'role-measurement'}); return {ms:performance.now()-t,count:r.elements.length,chars:JSON.stringify(r).length};})()`, arm, site.role)
				if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
					t.Fatal(err)
				}
				if round > 1 {
					samples[arm] = append(samples[arm], result.MS)
					counts[arm] = append(counts[arm], result.Count)
				}
			}
		}
		for _, arm := range samples {
			slices.Sort(arm)
		}
		out, _ := json.Marshal(map[string]any{"url": site.url, "role": site.role, "samples_ms": samples, "counts": counts, "before_median_ms": samples[0][3], "after_median_ms": samples[1][3]})
		t.Log(string(out))
	}
}
