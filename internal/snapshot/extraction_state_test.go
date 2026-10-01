package snapshot

import (
	"encoding/json"
	"fmt"
	"github.com/chromedp/chromedp"
	"os"
	"slices"
	"strings"
	"testing"
)

func legacyExtractionSnapshot(t *testing.T) string {
	t.Helper()
	s := SnapshotFunctionScript
	s = strings.Replace(s, "    const cachedVisible = visible(el);\n    const cachedViewport = inViewport(el);\n    const cachedDisabled = disabled(el);\n", "", 1)
	s = strings.Replace(s, "visible: cachedVisible,", "visible: visible(el),", 1)
	s = strings.Replace(s, "in_viewport: cachedViewport,", "in_viewport: inViewport(el),", 1)
	s = strings.Replace(s, "disabled: cachedDisabled,", "disabled: disabled(el),", 1)
	s = strings.Replace(s, "_frontier_score: frontierScore(role, name, signals, cachedVisible, cachedViewport, cachedDisabled)", "_frontier_score: frontierScore(role, name, signals, visible(el), inViewport(el), disabled(el))", 1)
	s = strings.Replace(s, "    if (formLensMode && !formRoles.has(role)) continue;\n", "", 1)
	s = strings.Replace(s, "    const key = keyFor(el, role, name);", "    if (formLensMode && !formRoles.has(role)) continue;\n    const key = keyFor(el, role, name);", 1)
	if s == SnapshotFunctionScript || strings.Contains(s, "cachedVisible") {
		t.Fatal("legacy extraction reconstruction failed")
	}
	return s
}
func TestExtractionPushdownSeededParity(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank")); err != nil {
		t.Fatal(err)
	}
	expr := `(() => {
      const oldWalk = ` + legacyExtractionSnapshot(t) + `;
      const newWalk = ` + SnapshotFunctionScript + `;
      let seed = 20261001;
      const rand = n => {seed=(Math.imul(seed,1664525)+1013904223)>>>0; return seed%n;};
      const roles = ['button','textbox','combobox','link','checkbox'];
      document.body.innerHTML = '<main></main><section></section><iframe></iframe>';
      const roots = [document.querySelector('main'), document.querySelector('section').attachShadow({mode:'open'}), document.querySelector('iframe').contentDocument.body];
      const create = () => {
        const el = document.createElement(['input','button','select','textarea','a'][rand(5)]);
        if(el.tagName==='SELECT') el.innerHTML='<option value="value 0">Zero</option><option value="value 1">One</option>';
        if(el.tagName==='A') el.href='#'+rand(10);
        if(el.tagName!=='SELECT') el.textContent='Control '+rand(12);
        el.setAttribute('role', roles[rand(roles.length)]);
        el.setAttribute('aria-label', 'Control '+rand(12));
        el.value = 'value '+rand(10);
        return el;
      };
      for (const root of roots) for(let i=0;i<60;i++) root.append(create());
      oldWalk({mode:'all',include_hidden:true});
      let checked = 0;
      for(let round=0;round<360;round++) {
        if(round%120===0) seed=[20261001,1,12648430][Math.floor(round/120)];
        const root = roots[rand(roots.length)];
        const el = root.children[rand(root.children.length)];
        switch(rand(12)) {
          case 0: root.prepend(el); break;
          case 1: el.replaceWith(create()); break;
          case 2: el.setAttribute('role', roles[rand(roles.length)]); break;
          case 3: el.disabled = !el.disabled; break;
          case 4: el.value = 'value '+rand(10); break;
          case 5: root.append(create()); root.firstElementChild.remove(); break;
          case 6: el.style.display = el.style.display ? '' : 'none'; break;
          case 7: el.style.opacity = el.style.opacity === '0' ? '1' : '0'; break;
          case 8: el.setAttribute('aria-hidden',el.getAttribute('aria-hidden')==='true'?'false':'true'); break;
          case 9: el.style.transform = 'translateY('+rand(1500)+'px)'; break;
          case 10: el.focus(); break;
          case 11: el.style.width = rand(400)+'px'; break;
        }
        for (const role of roles) {
          const opts = {mode:['all','frontier','form_lens'][round%3], role, limit:round%3?0:4, query:round%4?'':'Control 1', include_hidden:round%2===0,include_boxes:round%3===0};
          const stateBefore = JSON.stringify(window.__brw);
          const stamps = roots.flatMap(root => Array.from(root.children).map(el => [el, el.getAttribute('data-brw-ref')]));
          const before = newWalk(opts);
          window.__brw = JSON.parse(stateBefore);
          for(const [el,ref] of stamps) { if(ref===null) el.removeAttribute('data-brw-ref'); else el.setAttribute('data-brw-ref',ref); }
          const after = oldWalk(opts);
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
	if !result.OK || result.Checked != 1800 {
		t.Fatalf("differential failure: %+v", result)
	}
	t.Logf("%d role-filtered views matched across randomized DOM mutations", result.Checked)
}

func TestExtractionReadsEachElementStateOnce(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	var result struct {
		Count  int `json:"count"`
		Bounds int `json:"bounds"`
		Styles int `json:"styles"`
		Rects  int `json:"rects"`
	}
	expr := `(()=>{document.body.innerHTML=Array.from({length:100},(_,i)=>'<button>Button '+i+'</button>').join(''); let bounds=0,styles=0,rects=0; const p=Element.prototype,gb=p.getBoundingClientRect,gc=p.getClientRects,gs=window.getComputedStyle;p.getBoundingClientRect=function(){bounds++;return gb.call(this)};p.getClientRects=function(){rects++;return gc.call(this)};window.getComputedStyle=function(...args){styles++;return gs.apply(this,args)};try{const r=(` + SnapshotFunctionScript + `)({mode:'all'});return {count:r.elements.length,bounds,styles,rects};}finally{p.getBoundingClientRect=gb;p.getClientRects=gc;window.getComputedStyle=gs}})()`
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank"), chromedp.Evaluate(expr, &result)); err != nil {
		t.Fatal(err)
	}
	if result.Count != 100 || result.Bounds != 100 || result.Styles != 100 || result.Rects != 100 {
		t.Fatalf("redundant geometry work: %+v", result)
	}
}

func TestExtractionCustomElementStateAfterRefWrite(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	var result struct {
		Visible  bool `json:"visible"`
		Viewport bool `json:"viewport"`
		Disabled bool `json:"disabled"`
		Count    int  `json:"count"`
		Stable   bool `json:"stable"`
	}
	expr := `(()=>{customElements.define('state-control',class extends HTMLElement {static get observedAttributes(){return ['data-brw-ref']} attributeChangedCallback(){this.style.display='none';this.disabled=true;}});document.body.innerHTML='<state-control role="button" aria-label="Custom">Custom</state-control><input aria-label="Search"><input aria-label="Search">';const walk=` + SnapshotFunctionScript + `;const first=walk({mode:'all'});const el=first.elements.find(e=>e.name==='Custom');const refs=first.elements.map(e=>e.ref);const stable=refs.every(r=>document.querySelectorAll('[data-brw-ref="'+r+'"]').length===1)&&JSON.stringify(first.elements)===JSON.stringify(walk({mode:'all'}).elements);return {visible:el.visible,viewport:el.in_viewport,disabled:el.disabled,count:first.elements.length,stable};})()`
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank"), chromedp.Evaluate(expr, &result)); err != nil {
		t.Fatal(err)
	}
	if result.Visible || result.Viewport || !result.Disabled || result.Count != 3 || !result.Stable {
		t.Fatalf("stale custom-element state or refs: %+v", result)
	}
}

func TestFormPushdownSkipsAccessibleNameWork(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	var result struct {
		Count  int  `json:"count"`
		Reads  int  `json:"reads"`
		Stable bool `json:"stable"`
	}
	expr := `(()=>{document.body.innerHTML=Array.from({length:1000},(_,i)=>'<a href="#'+i+'">Noise '+i+'</a>').join('')+'<input aria-label="Search">';let reads=0;for(const el of document.querySelectorAll('a')) Object.defineProperty(el,'innerText',{get(){reads++;return 'Noise'}});const walk=` + SnapshotFunctionScript + `;const first=walk({mode:'form_lens'});const skipped=reads;walk({mode:'all'});const second=walk({mode:'form_lens'});return {count:first.elements.length,reads:skipped,stable:JSON.stringify(first.elements)===JSON.stringify(second.elements)};})()`
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank"), chromedp.Evaluate(expr, &result)); err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || result.Reads != 0 || !result.Stable {
		t.Fatalf("form role predicate did not skip names: %+v", result)
	}
}

func TestSnapshotExtractionMeasurement(t *testing.T) {
	if os.Getenv("BRW_MEASURE_SNAPSHOT_EXTRACTION") != "1" {
		t.Skip("set BRW_MEASURE_SNAPSHOT_EXTRACTION=1 for paired local extraction measurements")
	}
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank")); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		size  int
		mode  string
		noise bool
	}{{1000, "all", false}, {5000, "all", false}, {5000, "frontier", false}, {5000, "form_lens", true}} {
		setup := fmt.Sprintf(`document.body.innerHTML='<main>'+Array.from({length:%d},(_,i)=>%s).join('')+'<input aria-label="Search"><input aria-label="Other"></main>';window.__extractionWalkers=[%s,%s];navigator.userAgent`, fixture.size, map[bool]string{false: `'<button>Item '+i+'</button>'`, true: `'<a href="#'+i+'">Noise '+i+'</a>'`}[fixture.noise], legacyExtractionSnapshot(t), SnapshotFunctionScript)
		var agent string
		if err := chromedp.Run(ctx, chromedp.Evaluate(setup, &agent)); err != nil {
			t.Fatal(err)
		}
		samples := [2][]float64{}
		counts := [2]int{}
		for round := 0; round < 33; round++ {
			for turn := 0; turn < 2; turn++ {
				arm := (round + turn) % 2
				var r struct {
					MS    float64 `json:"ms"`
					Count int     `json:"count"`
				}
				expr := fmt.Sprintf(`(()=>{const t=performance.now();const r=window.__extractionWalkers[%d]({mode:%q,limit:%d,__brw_version:(window.__v=(window.__v||0)+1),__brw_epoch:'extraction'});return {ms:performance.now()-t,count:r.elements.length};})()`, arm, fixture.mode, map[bool]int{true: 40}[fixture.mode == "frontier"])
				if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &r)); err != nil {
					t.Fatal(err)
				}
				counts[arm] = r.Count
				if round > 2 {
					samples[arm] = append(samples[arm], r.MS)
				}
			}
		}
		for _, values := range samples {
			slices.Sort(values)
		}
		if counts[0] != counts[1] {
			t.Fatalf("candidate count mismatch: %v", counts)
		}
		encoded, _ := json.Marshal(map[string]any{"size": fixture.size, "mode": fixture.mode, "user_agent": agent, "counts": counts, "samples_ms": samples, "medians_ms": []float64{samples[0][15], samples[1][15]}})
		t.Log(string(encoded))
	}
}
