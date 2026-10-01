package extensionbridge

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

func TestBridgeRefClickTrustedTargetingAndFocusIsolation(t *testing.T) {
	if os.Getenv("BRW_TEST_BRIDGE_TRUSTED_CLICK") != "1" {
		t.Skip("set BRW_TEST_BRIDGE_TRUSTED_CLICK=1 for disposable trusted click conformance")
	}
	t.Run("headless", func(t *testing.T) { runBridgeTrustedClickFixture(t, true) })
	t.Run("headed", func(t *testing.T) { runBridgeTrustedClickFixture(t, false) })
}

func runBridgeTrustedClickFixture(t *testing.T, headless bool) {
	b, ctx, endpoint := deadlineLiveBridgeMode(t, false, headless)
	focusDeadline := time.Now().Add(4 * time.Second)
	for {
		if err := b.FocusTab(ctx, browser.TabIDFromContext(ctx)); err != nil {
			t.Fatal(err)
		}
		err := b.requireTrustedClickTarget(ctx)
		if err == nil {
			break
		}
		if time.Now().After(focusDeadline) {
			t.Fatalf("owned fixture focus did not settle: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	focusState, stateErr := b.call(ctx, "get_tab_input_state", map[string]any{"tabId": parseTabID(browser.TabIDFromContext(ctx))})
	t.Logf("actual Chrome focus state=%s err=%v headless=%t", focusState, stateErr, headless)
	setup := func(markup string) {
		t.Helper()
		raw, _ := json.Marshal(markup)
		_, err := b.Evaluate(ctx, `document.body.replaceWith(document.createElement("body"));document.body.innerHTML=`+string(raw)+`;window.effects=0;window.events=[];window.listen=function(el){el.addEventListener('click',e=>{events.push({trusted:e.isTrusted,type:e.type});if(e.isTrusted)effects++})};`)
		if err != nil {
			t.Fatal(err)
		}
	}
	check := func(expression string) {
		t.Helper()
		result, err := b.Evaluate(ctx, expression)
		if err != nil || result != true {
			t.Fatalf("postcondition %s result=%v err=%v", expression, result, err)
		}
	}
	setup(`<dialog open><button data-brw-ref="e1"><span>Confirm</span></button></dialog><output id="completion">Pending</output>`)
	if _, err := b.Evaluate(ctx, `listen(document.querySelector('button'));document.querySelector('button').addEventListener('click',e=>{if(e.isTrusted){document.querySelector('#completion').textContent='1 member added';document.querySelector('dialog').remove()}})`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := b.Click(ctx, "e1")
	if err != nil {
		t.Fatal(err)
	}
	probe, probeErr := b.Evaluate(ctx, `({effects:effects,event_count:events.length,trusted:events.map(e=>e.trusted),completion:document.querySelector("#completion").textContent,dialog:!!document.querySelector("dialog")})`)
	t.Logf("bridge business probe=%v err=%v", probe, probeErr)
	check(`effects===1 && events.length===1 && events[0].trusted===true && document.querySelector("#completion").textContent==="1 member added" && !document.querySelector("dialog")`)
	t.Logf("trusted public click elapsed=%s action=%s", time.Since(started), result.Message)
	for _, action := range []string{"text", "xy"} {
		setup(`<button id="confirm">Confirm</button>`)
		if _, err := b.Evaluate(ctx, `listen(document.querySelector('button'))`); err != nil {
			t.Fatal(err)
		}
		if action == "text" {
			if _, err := b.ClickText(ctx, snapshot.ClickTextOptions{Text: "Confirm"}); err != nil {
				t.Fatal(err)
			}
		} else {
			p, err := b.Evaluate(ctx, `(()=>{var r=document.querySelector('button').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2}})()`)
			if err != nil {
				t.Fatal(err)
			}
			point := p.(map[string]any)
			if _, err := b.ClickXY(ctx, point["x"].(float64), point["y"].(float64)); err != nil {
				t.Fatal(err)
			}
		}
		check(`effects===1 && events.length===1 && events[0].trusted===true`)
	}
	setup(`<button>HTTP-compatible text</button>`)
	if _, err := b.Evaluate(ctx, `Object.defineProperty(crypto,'randomUUID',{value:undefined,configurable:true});listen(document.querySelector('button'))`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.clickTextRaw(ctx, snapshot.ClickTextOptions{Text: "HTTP-compatible text"}); err != nil {
		t.Fatal(err)
	}
	check(`effects===1 && events.length===1 && events[0].trusted===true`)
	setup(`<button style="position:fixed;top:0;left:20px;width:150px;height:3000px">Tall target</button>`)
	if _, err := b.Evaluate(ctx, `listen(document.querySelector('button'))`); err != nil {
		t.Fatal(err)
	}
	noScroll := false
	if _, err := b.clickTextRaw(ctx, snapshot.ClickTextOptions{Text: "Tall target", AutoScroll: &noScroll}); err != nil {
		t.Fatal(err)
	}
	check(`effects===1 && events.length===1 && events[0].trusted===true && scrollY===0`)
	for _, markup := range []string{`<button disabled><span style="display:block;width:150px;height:40px">Disabled child</span></button>`, `<label for="disabled-input"><span style="display:block;width:150px;height:40px">Disabled label child</span></label><input id="disabled-input" type="checkbox" disabled>`} {
		setup(markup)
		if _, err := b.Evaluate(ctx, `document.body.addEventListener('click',()=>effects++)`); err != nil {
			t.Fatal(err)
		}
		p, err := b.Evaluate(ctx, `(()=>{var r=document.querySelector('span').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2}})()`)
		if err != nil {
			t.Fatal(err)
		}
		point := p.(map[string]any)
		if _, err := b.ClickXY(ctx, point["x"].(float64), point["y"].(float64)); err == nil {
			t.Fatal("disabled nested point reported dispatched")
		}
		check(`effects===0`)
	}
	setup(`<button style="position:absolute;left:20px;top:20px;width:100px;height:40px">Covered text</button><div style="position:absolute;inset:0;background:white"></div>`)
	if _, err := b.Evaluate(ctx, `listen(document.querySelector('button'));document.body.addEventListener('click',()=>effects+=100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ClickText(ctx, snapshot.ClickTextOptions{Text: "Covered text"}); err == nil {
		t.Fatal("text locator clicked overlay")
	}
	check(`effects===0 && events.length===0`)
	setup(`<div id="host"></div>`)
	if _, err := b.Evaluate(ctx, `let root=document.querySelector('#host').attachShadow({mode:'open'});root.innerHTML='<button data-brw-ref="e2"><span>Shadow</span></button>';listen(root.querySelector('button'))`); err != nil {
		t.Fatal(err)
	}
	if err := b.clickRef(ctx, "e2"); err != nil {
		t.Fatal(err)
	}
	check(`effects===1 && events.length===1 && events[0].trusted===true`)
	setup(`<div id="closed-host"></div>`)
	if _, err := b.Evaluate(ctx, `var closedRoot=document.querySelector('#closed-host').attachShadow({mode:'closed'});closedRoot.innerHTML='<button data-brw-ref="eClosed"><span>Closed</span></button>';listen(closedRoot.querySelector('button'))`); err != nil {
		t.Fatal(err)
	}
	if err := b.clickRef(ctx, "eClosed"); err != nil {
		t.Fatal(err)
	}
	check(`document.querySelector('#closed-host').shadowRoot===null && effects===1 && events[0].trusted===true`)
	setup(`<fieldset disabled><legend><button data-brw-ref="eLegend">Enabled legend</button></legend></fieldset>`)
	if _, err := b.Evaluate(ctx, `listen(document.querySelector('button'))`); err != nil {
		t.Fatal(err)
	}
	if err := b.clickRef(ctx, "eLegend"); err != nil {
		t.Fatal(err)
	}
	check(`effects===1 && events[0].trusted===true`)
	setup(`<label for="check" style="display:block;position:relative;width:200px;height:40px">External<input id="check" type="checkbox" data-brw-ref="e3" style="opacity:0;position:absolute;inset:0;width:100%;height:100%;margin:0;pointer-events:none"></label>`)
	if err := b.clickRef(ctx, "e3"); err != nil {
		t.Fatal(err)
	}
	check(`document.querySelector('#check').checked===true`)
	for _, tc := range []struct{ name, markup string }{
		{"disabled", `<button data-brw-ref="e4" disabled>Disabled</button>`},
		{"fieldset", `<fieldset disabled><button data-brw-ref="e4">Disabled</button></fieldset>`},
		{"aria", `<div aria-disabled="true"><button data-brw-ref="e4">Disabled</button></div>`},
		{"inert", `<div inert><button data-brw-ref="e4">Inert</button></div>`},
		{"occluded", `<button data-brw-ref="e4" style="position:absolute;left:20px;top:20px;width:100px;height:40px">Covered</button><div style="position:absolute;left:0;top:0;width:300px;height:300px;background:white"></div>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(tc.markup)
			if _, err := b.Evaluate(ctx, `listen(document.querySelector('button'));document.body.addEventListener('click',()=>effects+=100)`); err != nil {
				t.Fatal(err)
			}
			if err := b.clickRef(ctx, "e4"); err == nil {
				t.Fatal("invalid target reported clicked")
			}
			check(`effects===0 && events.length===0`)
		})
	}
	setup(`<button data-brw-ref="eHover" style="position:absolute;left:200px;top:200px;width:100px;height:40px">Hover target</button><div id="cover" hidden style="position:absolute;inset:0;background:white"></div>`)
	if _, err := b.Evaluate(ctx, `listen(document.querySelector('button'));document.querySelector('button').addEventListener('pointerover',()=>document.querySelector('#cover').hidden=false)`); err != nil {
		t.Fatal(err)
	}
	if err := b.clickRef(ctx, "eHover"); err == nil {
		t.Fatal("hover overlay reported clicked")
	}
	check(`effects===0 && events.length===0`)

	touch := true
	if _, err := b.EmulateDevice(ctx, browser.DeviceEmulationOptions{Width: 1280, Height: 1000, DeviceScaleFactor: 1, Mobile: &touch, Touch: &touch}); err != nil {
		t.Fatal(err)
	}
	for _, entrypoint := range []string{"ref", "text", "xy"} {
		setup(`<button data-brw-ref="eTap" style="position:absolute;left:20px;top:20px;width:100px;height:40px">Trusted tap</button>`)
		if _, err := b.Evaluate(ctx, `listen(document.querySelector('button'))`); err != nil {
			t.Fatal(err)
		}
		switch entrypoint {
		case "ref":
			_, err = b.Click(ctx, "eTap")
		case "text":
			_, err = b.ClickText(ctx, snapshot.ClickTextOptions{Text: "Trusted tap"})
		case "xy":
			_, err = b.ClickXY(ctx, 70, 40)
		}
		if err != nil {
			t.Fatalf("mobile %s: %v", entrypoint, err)
		}
		check(`effects===1 && events.length===1 && events[0].trusted`)
	}
	if _, err := b.EmulateDevice(ctx, browser.DeviceEmulationOptions{Clear: true}); err != nil {
		t.Fatal(err)
	}
	setup(`<button data-brw-ref="e5">Background target</button>`)
	if _, err := b.Evaluate(ctx, `listen(document.querySelector('button'))`); err != nil {
		t.Fatal(err)
	}
	url, err := b.Evaluate(ctx, `location.href`)
	if err != nil {
		t.Fatal(err)
	}
	sentinel, err := b.Open(ctx, url.(string))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.CloseTab(context.Background(), sentinel.Tab.ID) })
	if err := b.FocusTab(ctx, sentinel.Tab.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.clickRef(ctx, "e5"); err == nil || !strings.Contains(err.Error(), "brw_focus_tab") {
		t.Fatalf("inactive click err=%v", err)
	}
	check(`effects===0 && events.length===0`)
	if _, err := b.ClickText(ctx, snapshot.ClickTextOptions{Text: "Background target"}); err == nil || !strings.Contains(err.Error(), "brw_focus_tab") {
		t.Fatalf("inactive text err=%v", err)
	}
	if _, err := b.ClickXY(ctx, 20, 20); err == nil || !strings.Contains(err.Error(), "brw_focus_tab") {
		t.Fatalf("inactive xy err=%v", err)
	}
	check(`effects===0 && events.length===0`)
	tabs, err := b.ListTabs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	active := ""
	for _, tab := range tabs {
		if tab.Active {
			active = tab.ID
		}
	}
	if active != sentinel.Tab.ID {
		t.Fatalf("click stole focus: active=%s want=%s", active, sentinel.Tab.ID)
	}
	original := browser.TabIDFromContext(ctx)
	t.Logf("explicit target=%s stayed distinct from active sentinel=%s", original, active)
	m, err := browser.New(ctx, browser.Config{RemoteURL: endpoint, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetPacing(browser.PacingOff)
	opened, err := m.Open(ctx, url.(string))
	if err != nil {
		t.Fatal(err)
	}
	directCtx := browser.WithTabID(ctx, opened.Tab.ID)
	defer m.CloseTab(context.Background(), opened.Tab.ID)
	if err := m.FocusTab(directCtx, opened.Tab.ID); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		_, err := m.Evaluate(directCtx, `document.body.innerHTML='<dialog open><button data-brw-ref="eDirect">Confirm</button></dialog><output id="completion">Pending</output>';window.effects=0;window.trusted=[];document.querySelector('button').addEventListener('click',e=>{trusted.push(e.isTrusted);if(e.isTrusted){effects++;document.querySelector('#completion').textContent='1 member added';document.querySelector('dialog').remove()}})`)
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		result, err := m.Click(directCtx, "eDirect")
		if err != nil {
			t.Fatal(err)
		}
		state, err := m.Evaluate(directCtx, `effects===1 && trusted.length===1 && trusted[0]===true && document.querySelector('#completion').textContent==='1 member added' && !document.querySelector('dialog')`)
		if err != nil || state != true {
			t.Fatalf("direct business postcondition=%v err=%v", state, err)
		}
		t.Logf("direct verified effect round=%d elapsed=%s message=%s", round, time.Since(started), result.Message)
	}
	for _, markup := range []string{`<button data-brw-ref="eBad" disabled>Disabled</button>`, `<button data-brw-ref="eBad" style="position:absolute;left:20px;top:20px;width:100px;height:40px">Covered</button><div style="position:absolute;inset:0;background:white"></div>`} {
		raw, _ := json.Marshal(markup)
		if _, err := m.Evaluate(directCtx, `document.body.innerHTML=`+string(raw)+`;window.effects=0;document.body.addEventListener('click',()=>effects++)`); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Click(directCtx, "eBad"); err == nil {
			t.Fatal("direct invalid target reported dispatched")
		}
		state, err := m.Evaluate(directCtx, `effects===0`)
		if err != nil || state != true {
			t.Fatalf("direct refused input effect=%v err=%v", state, err)
		}
	}

}
