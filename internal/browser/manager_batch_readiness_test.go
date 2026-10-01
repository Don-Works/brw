package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestBatchReadinessUsesExplicitPostconditions(t *testing.T) {
	m := newHeadlessManager(t)
	m.SetPacing(PacingOff)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		if r.URL.Path == "/arrived" {
			_, _ = fmt.Fprint(w, `<title>Arrived</title><output id="state">done</output>`)
			return
		}
		_, _ = fmt.Fprint(w, `<input id="name" aria-label="Name"><button id="trigger">Trigger</button><output id="state">wait</output><div id="overlay"></div>`)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name, setup, action, condition string
	}{
		{"value", `window.ready=false;name.oninput=()=>window.ready=name.value==='Ada'`, "fill", `fn:window.ready === true`},
		{"delayed_enable", `trigger.disabled=true;name.oninput=()=>setTimeout(()=>trigger.disabled=false,80)`, "fill", `fn:!document.getElementById('trigger').disabled`},
		{"equal_length_text", `trigger.onclick=()=>setTimeout(()=>state.textContent='done',80)`, "click", `fn:document.getElementById('state').textContent === 'done'`},
		{"overlay", `overlay.style.cssText='position:fixed;top:0;right:0;width:20px;height:20px';trigger.onclick=()=>setTimeout(()=>overlay.remove(),80)`, "click", `fn:!document.getElementById('overlay')`},
		{"redirect", `trigger.onclick=()=>setTimeout(()=>location.href='/arrived',80)`, "click", `fn:location.pathname === '/arrived' && document.readyState === 'complete'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			opened, err := m.Open(ctx, server.URL+"/")
			if err != nil {
				t.Fatal(err)
			}
			ctx = WithTabID(ctx, opened.Tab.ID)
			if _, err := m.Evaluate(ctx, "(()=>{const name=document.getElementById('name');const trigger=document.getElementById('trigger');const state=document.getElementById('state');const overlay=document.getElementById('overlay');"+tc.setup+";return true})()"); err != nil {
				t.Fatal(err)
			}
			snap, err := m.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all"})
			if err != nil {
				t.Fatal(err)
			}
			ref := ""
			for _, el := range snap.Elements {
				if tc.action == "fill" && el.Name == "Name" || tc.action == "click" && el.Name == "Trigger" {
					ref = el.Ref
				}
			}
			if ref == "" {
				t.Fatal("missing action ref")
			}
			tabCtx, err := m.tabContext(opened.Tab.ID)
			if err != nil {
				t.Fatal(err)
			}
			step := BatchStep{Action: tc.action, Ref: ref, Text: "Ada"}
			wait := BatchStep{Action: "wait", Condition: tc.condition, TimeoutMS: 2000}
			if _, ok := batchReadinessContext(ctx, tabCtx, step, wait).Value(batchReadinessKey{}).(batchReadiness); !ok {
				t.Fatal("false explicit predicate did not activate readiness path")
			}
			started := time.Now()
			result, err := m.ExecuteBatch(ctx, []BatchStep{step, wait})
			if err != nil || !result.OK || result.StepsCompleted != 2 {
				t.Fatalf("batch=%+v err=%v", result, err)
			}
			value, err := m.Evaluate(ctx, "Boolean("+strings.TrimPrefix(tc.condition, "fn:")+")")
			if err != nil || value != true {
				t.Fatalf("returned before explicit predicate: value=%v err=%v", value, err)
			}
			t.Logf("%s verified in %s", tc.name, time.Since(started))
			if err := m.CloseTab(ctx, opened.Tab.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBatchReadinessRejectsAlreadyTrueAndUnboundedPredicates(t *testing.T) {
	m := newHeadlessManager(t)
	ctx := context.Background()
	opened, err := m.Open(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	tabCtx, err := m.tabContext(opened.Tab.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []BatchStep{
		{Action: "wait", Condition: "fn:true", TimeoutMS: 100},
		{Action: "wait", Condition: "fn:false"},
		{Action: "wait", Condition: "ready", TimeoutMS: 100},
		{Action: "wait", Condition: "fn:Promise.resolve(false)", TimeoutMS: 100},
		{Action: "wait", Condition: "fn:document.missing.value", TimeoutMS: 100},
	} {
		if _, ok := batchReadinessContext(ctx, tabCtx, BatchStep{Action: "fill"}, next).Value(batchReadinessKey{}).(batchReadiness); ok {
			t.Fatalf("unsafe readiness predicate accepted: %+v", next)
		}
	}
}

func TestBatchReadinessFailureStopsBeforeFollowingAction(t *testing.T) {
	m := newHeadlessManager(t)
	m.SetPacing(PacingOff)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithTabID(ctx, opened.Tab.ID)
	if _, err := m.Evaluate(ctx, `document.body.innerHTML='<input aria-label="Name"><button>Wrong next action</button>';window.ready=false;window.clicked=false;document.querySelector('button').onclick=()=>window.clicked=true`); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(ctx, snapshot.SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inputRef, buttonRef := "", ""
	for _, el := range snap.Elements {
		if el.Role == "textbox" {
			inputRef = el.Ref
		} else if el.Role == "button" {
			buttonRef = el.Ref
		}
	}
	result, err := m.ExecuteBatch(ctx, []BatchStep{
		{Action: "fill", Ref: inputRef, Text: "Ada"},
		{Action: "wait", Condition: "fn:window.ready === true", TimeoutMS: 80},
		{Action: "click", Ref: buttonRef},
	})
	if err != nil || result.OK || !strings.Contains(result.Error, "timed out") {
		t.Fatalf("missing postcondition accepted: %+v err=%v", result, err)
	}
	clicked, err := m.Evaluate(ctx, "window.clicked")
	if err != nil || clicked != false {
		t.Fatalf("following action ran: %v err=%v", clicked, err)
	}
}

func TestBatchReadinessCancellationCleansPageObserver(t *testing.T) {
	m := newHeadlessManager(t)
	opened, err := m.Open(context.Background(), "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTabID(context.Background(), opened.Tab.ID)
	if _, err := m.Evaluate(ctx, `window.ready=false;window.waitDisconnects=0;window.MutationObserver=class extends MutationObserver{disconnect(){window.waitDisconnects++;super.disconnect()}}`); err != nil {
		t.Fatal(err)
	}
	tabCtx, err := m.tabContext(opened.Tab.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner, cancel := context.WithCancel(context.Background())
	defer cancel()
	acted := false
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	err = m.runWithBatchReadiness(tabCtx, batchReadiness{owner: owner, condition: "fn:window.ready === true", timeout: 5 * time.Second}, func() error {
		acted = true
		return nil
	})
	if err == nil || !acted || time.Since(started) > time.Second {
		t.Fatalf("cancellation did not stop readiness wait: acted=%t err=%v elapsed=%s", acted, err, time.Since(started))
	}
	state, err := m.Evaluate(ctx, `({keys:Object.keys(window).filter(k=>k.startsWith('__brw_wait_')).length,disconnects:window.waitDisconnects})`)
	if err != nil {
		t.Fatal(err)
	}
	values := state.(map[string]any)
	if values["keys"] != float64(0) || values["disconnects"].(float64) < 1 {
		t.Fatalf("page observer/registry leaked: %v", state)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	acted = false
	err = m.runWithBatchReadiness(tabCtx, batchReadiness{owner: cancelled, condition: "fn:false", timeout: time.Second}, func() error {
		acted = true
		return nil
	})
	if err == nil || acted {
		t.Fatalf("cancelled action was dispatched: acted=%t err=%v", acted, err)
	}
}

func TestBatchReadinessMeasurement(t *testing.T) {
	if os.Getenv("BRW_MEASURE_BATCH_READINESS") != "1" {
		t.Skip("set BRW_MEASURE_BATCH_READINESS=1 for paired explicit-postcondition measurements")
	}
	m := newHeadlessManager(t)
	m.SetPacing(PacingOff)
	ctx := context.Background()
	opened, err := m.Open(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithTabID(ctx, opened.Tab.ID)
	if _, err := m.Evaluate(ctx, `document.body.innerHTML='<input aria-label="Name">'`); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(ctx, snapshot.SnapshotOptions{})
	if err != nil || len(snap.Elements) != 1 {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	tabCtx, err := m.tabContext(opened.Tab.ID)
	if err != nil {
		t.Fatal(err)
	}
	samples := [2][]float64{}
	for round := 0; round < 12; round++ {
		for turn := 0; turn < 2; turn++ {
			arm := (round + turn) % 2
			text := fmt.Sprintf("value-%d-%d", round, arm)
			condition := fmt.Sprintf("fn:document.querySelector('input').value === %q", text)
			started := time.Now()
			stepCtx := tabCtx
			if arm == 1 {
				stepCtx = batchReadinessContext(ctx, tabCtx, BatchStep{Action: "fill", Ref: snap.Elements[0].Ref, Text: text}, BatchStep{Action: "wait", Condition: condition, TimeoutMS: 1000})
			}
			if err := m.fillRef(stepCtx, snap.Elements[0].Ref, text, true); err != nil {
				t.Fatal(err)
			}
			if round > 1 {
				samples[arm] = append(samples[arm], float64(time.Since(started).Microseconds())/1000)
			}
			value, err := m.Evaluate(ctx, "document.querySelector('input').value")
			if err != nil || value != text {
				t.Fatalf("unverified field: %v err=%v", value, err)
			}
		}
	}
	for _, values := range samples {
		slices.Sort(values)
	}
	encoded, _ := json.Marshal(map[string]any{"samples_ms": samples, "medians_ms": []float64{samples[0][5], samples[1][5]}})
	t.Log(string(encoded))
}

func TestBatchReadinessRejectsReplacedDocument(t *testing.T) {
	m := newHeadlessManager(t)
	ctx := context.Background()
	opened, err := m.Open(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithTabID(ctx, opened.Tab.ID)
	if _, err := m.Evaluate(ctx, "window.ready=false"); err != nil {
		t.Fatal(err)
	}
	tabCtx, err := m.tabContext(opened.Tab.ID)
	if err != nil {
		t.Fatal(err)
	}
	stepCtx := batchReadinessContext(ctx, tabCtx, BatchStep{Action: "fill"}, BatchStep{Action: "wait", Condition: "fn:window.ready === true", TimeoutMS: 100})
	readiness, ok := stepCtx.Value(batchReadinessKey{}).(batchReadiness)
	if !ok {
		t.Fatal("missing baseline readiness context")
	}
	if _, err := m.NavigateTo(ctx, "about:blank"); err != nil {
		t.Fatal(err)
	}
	if readiness.document == batchDocumentIdentity(tabCtx) {
		t.Fatal("fixture did not replace the document")
	}
	if _, err := m.Evaluate(ctx, `window.ready=true;window.settleObservers=0;window.MutationObserver=class extends MutationObserver{constructor(fn){super(fn);window.settleObservers++}}`); err != nil {
		t.Fatal(err)
	}
	if err := m.runWithPrearmedSettle(stepCtx, 20*time.Millisecond, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	value, err := m.Evaluate(ctx, "window.settleObservers")
	if err != nil || value.(float64) < 1 {
		t.Fatalf("stale document postcondition bypassed ordinary settle: %v err=%v", value, err)
	}
}
