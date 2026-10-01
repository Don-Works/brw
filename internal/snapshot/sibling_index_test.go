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

const legacySnapshotPath = `  function pathFor(el) {
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

`

func legacyPathSnapshot(t *testing.T) string {
	t.Helper()
	start := strings.Index(SnapshotFunctionScript, "  function pathFor(el) {")
	end := strings.Index(SnapshotFunctionScript, "  // keyFor builds")
	if start < 0 || end <= start {
		t.Fatal("snapshot path function boundaries missing")
	}
	return SnapshotFunctionScript[:start] + legacySnapshotPath + SnapshotFunctionScript[end:]
}

func TestSnapshotSiblingIndexParityAndLinearWork(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	setup := `(() => {
      document.body.innerHTML = '<main></main><div id="shadow"></div><iframe></iframe>';
      const html = Array.from({length: 300}, (_, i) => '<span>prose</span><button>Button ' + i + '</button><a href="#' + i + '">Link</a>').join('');
      document.querySelector('main').innerHTML = html;
      document.querySelector('#shadow').attachShadow({mode:'open'}).innerHTML = html;
      document.querySelector('iframe').contentDocument.body.innerHTML = html;
    })()`
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank"), chromedp.Evaluate(setup, nil)); err != nil {
		t.Fatal(err)
	}
	for phase := 0; phase < 2; phase++ {
		var result struct {
			Equal    bool `json:"equal"`
			Elements int  `json:"elements"`
			Previous int  `json:"previous"`
			Next     int  `json:"next"`
		}
		expr := `(() => {
          const opts = {mode:'all'};
          const legacy = (` + legacyPathSnapshot(t) + `)(opts);
          let previous = 0, next = 0;
          const proto = Element.prototype;
          const pd = Object.getOwnPropertyDescriptor(proto, 'previousElementSibling');
          const nd = Object.getOwnPropertyDescriptor(proto, 'nextElementSibling');
          Object.defineProperty(proto, 'previousElementSibling', {...pd, get() { previous++; return pd.get.call(this); }});
          Object.defineProperty(proto, 'nextElementSibling', {...nd, get() { next++; return nd.get.call(this); }});
          try {
            const current = (` + SnapshotFunctionScript + `)(opts);
            return {equal:JSON.stringify(legacy.elements) === JSON.stringify(current.elements), elements:current.elements.length, previous, next};
          } finally {
            Object.defineProperty(proto, 'previousElementSibling', pd);
            Object.defineProperty(proto, 'nextElementSibling', nd);
          }
        })()`
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
			t.Fatal(err)
		}
		if !result.Equal || result.Elements < 1800 || result.Previous != 0 || result.Next > 3000 {
			t.Fatalf("phase %d: %+v", phase, result)
		}
		mutate := `(() => {
          const roots = [document.querySelector('main'), document.querySelector('#shadow').shadowRoot, document.querySelector('iframe').contentDocument.body];
          for (const root of roots) {
            root.prepend(root.lastElementChild);
            root.insertBefore(document.createElement('button'), root.children[12]);
            root.children[25].remove();
          }
        })()`
		if err := chromedp.Run(ctx, chromedp.Evaluate(mutate, nil)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSnapshotWideDOMMeasurement(t *testing.T) {
	if os.Getenv("BRW_MEASURE_SNAPSHOT") != "1" {
		t.Skip("set BRW_MEASURE_SNAPSHOT=1 for paired browser measurements")
	}
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank")); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{100, 1000, 5000} {
		setup := fmt.Sprintf(`document.body.innerHTML = '<main>' + Array.from({length:%d}, (_,i) => '<button>Item '+i+'</button>').join('') + '</main>'`, size)
		if err := chromedp.Run(ctx, chromedp.Evaluate(setup, nil)); err != nil {
			t.Fatal(err)
		}
		install := `window.__perfWalkers = [` + legacyPathSnapshot(t) + `,` + SnapshotFunctionScript + `]`
		if err := chromedp.Run(ctx, chromedp.Evaluate(install, nil)); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"frontier", "all"} {
			samples := [2][]float64{}
			for round := 0; round < 11; round++ {
				for turn := 0; turn < 2; turn++ {
					arm := (round + turn) % 2
					var ms float64
					expr := fmt.Sprintf(`(() => { const t=performance.now(); window.__perfWalkers[%d]({mode:%q,viewport_only:%t,limit:%d,__brw_version:(window.__perfVersion=(window.__perfVersion||0)+1),__brw_epoch:'paired-measurement'}); return performance.now()-t; })()`, arm, mode, mode == "frontier", map[bool]int{true: 40}[mode == "frontier"])
					if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ms)); err != nil {
						t.Fatal(err)
					}
					if round > 1 {
						samples[arm] = append(samples[arm], ms)
					}
				}
			}
			for _, values := range samples {
				slices.Sort(values)
			}
			data, _ := json.Marshal(map[string]any{"size": size, "mode": mode, "samples": samples, "legacy_median_ms": samples[0][4], "indexed_median_ms": samples[1][4]})
			t.Log(string(data))
		}
	}
}

func TestSnapshotSiblingIndexCustomElementReaction(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	setup := `(() => {
      customElements.define('path-control', class extends HTMLElement {
        static get observedAttributes() { return ['data-brw-ref']; }
        attributeChangedCallback() {
          const button = document.createElement('button');
          button.textContent = 'Inserted';
          this.after(button);
        }
      });
      document.body.innerHTML = '<path-control role="button">Custom</path-control><button>Target</button>';
    })()`
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank"), chromedp.Evaluate(setup, nil)); err != nil {
		t.Fatal(err)
	}
	var path string
	expr := `(() => {
      const snap = (` + SnapshotFunctionScript + `)({mode:'all'});
      return snap.elements.find(el => el.name === 'Target').key.split('|').pop();
    })()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &path)); err != nil {
		t.Fatal(err)
	}
	if path != "body:nth-of-type(1)>button:nth-of-type(2)" {
		t.Fatalf("stale path after synchronous custom-element reaction: %s", path)
	}
}
