package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// The button is pinned at a known position so a forwarded event can name real
// viewport coordinates without first resolving a ref — which is the whole point
// of takeover: the human aims with their eyes, not with the ref graph.
//
// The count is mirrored into the DOM as well as onto window, because the test
// has to read it DURING a hold and a hand-written expression is refused then.
// Reading it the way brw_get reads is the point: that path stays open.
const takeoverFixture = `<!doctype html><html><head><title>Takeover fixture</title></head><body style="margin:0">
<button id="go" style="position:fixed;left:0;top:0;width:240px;height:80px">Go</button>
<input id="field" style="position:fixed;left:0;top:100px;width:240px;height:40px" aria-label="Field">
<output id="count" style="position:fixed;left:0;top:160px">0</output>
<script>
window.__clicks = 0;
document.getElementById('go').addEventListener('click', function(){
  window.__clicks++;
  document.getElementById('count').textContent = String(window.__clicks);
  if (window.__slowClick) { var end = Date.now() + 250; while (Date.now() < end) {} }
});
</script>
</body></html>`

// slowClickFixture makes a click occupy the renderer for a quarter second, so a
// multi-step batch is still running when a human takes over partway through it,
// and so a forwarded press is still in the renderer when its grant expires. The
// mousedown listener is what stalls a raw forwarded event, which fires no click.
const slowClickFixture = `<!doctype html><html><head><title>Takeover fixture</title></head><body style="margin:0">
<script>
window.__slowClick = true;
document.addEventListener('mousedown', function(){
  if (window.__slowClick) { var end = Date.now() + 250; while (Date.now() < end) {} }
}, true);
</script>` + takeoverFixture

func takeoverFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, takeoverFixture)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, slowClickFixture)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// clickCount reads the counter the way brw_get reads it: through the generated
// script, which stays allowed during a hold. A hand-written expression would be
// refused, which is the property the tests below assert rather than rely on.
func clickCount(t *testing.T, ctx context.Context, m *Manager) float64 {
	t.Helper()
	return clickCountOnTab(t, ctx, m, "")
}

func clickCountOnTab(t *testing.T, ctx context.Context, m *Manager, tabID string) float64 {
	t.Helper()
	if tabID != "" {
		ctx = WithTabID(ctx, tabID)
	}
	text, err := generatedGet(ctx, m, "text", "#count")
	if err != nil {
		t.Fatalf("read the click counter: %v", err)
	}
	count, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		t.Fatalf("click counter %q is not a number: %v", text, err)
	}
	return count
}

// generatedGet answers one typed question the way brw_get does. The reply is
// the script's {value: ...} envelope, not a bare value.
func generatedGet(ctx context.Context, m *Manager, what, target string) (string, error) {
	result, err := m.Evaluate(WithTraceLabel(ctx, TraceActionGet, what+" "+target),
		snapshot.BuildGetExpression(what, target, ""))
	if err != nil {
		return "", err
	}
	envelope, ok := result.(map[string]any)
	if !ok {
		return "", fmt.Errorf("get returned %T (%v), want the script envelope", result, result)
	}
	switch value := envelope["value"].(type) {
	case string:
		return value, nil
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64), nil
	case nil:
		return "", fmt.Errorf("get %s %s returned no value (%v)", what, target, envelope)
	default:
		return fmt.Sprint(value), nil
	}
}

// Input forwarded without an explicit enable must not reach the page. The
// assertion is on the PAGE, not on the returned error: a refusal that still
// dispatched the event would satisfy an error-only test and leave a dashboard
// able to drive a signed-in browser nobody switched on.
func TestTakeoverInputReachesThePageOnlyAfterExplicitEnable(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := takeoverFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/page"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}

	press := TakeoverInput{Kind: "mouse", Type: "mousePressed", X: 120, Y: 40, Button: "left", Buttons: 1, ClickCount: 1}
	release := TakeoverInput{Kind: "mouse", Type: "mouseReleased", X: 120, Y: 40, Button: "left", ClickCount: 1}

	for _, event := range []TakeoverInput{press, release} {
		if err := manager.DispatchTakeoverInput(ctx, "", event); !errors.Is(err, ErrTakeoverNotHeld) {
			t.Fatalf("input without a grant = %v, want ErrTakeoverNotHeld", err)
		}
	}
	if got := clickCount(t, ctx, manager); got != 0 {
		t.Fatalf("the page saw %v clicks with takeover off; input must not reach it before an explicit enable", got)
	}
	if err := manager.DispatchTakeoverInput(ctx, "a-token-nobody-granted", press); !errors.Is(err, ErrTakeoverNotHeld) {
		t.Fatalf("input with an invented token = %v, want ErrTakeoverNotHeld", err)
	}
	if got := clickCount(t, ctx, manager); got != 0 {
		t.Fatalf("the page saw %v clicks from an invented token", got)
	}

	grant, err := manager.AcquireTakeover("operator", time.Minute)
	if err != nil {
		t.Fatalf("acquire takeover: %v", err)
	}
	for _, event := range []TakeoverInput{press, release} {
		if err := manager.DispatchTakeoverInput(ctx, grant.Token, event); err != nil {
			t.Fatalf("forward %s: %v", event.Type, err)
		}
	}
	if got := clickCount(t, ctx, manager); got != 1 {
		t.Fatalf("the page saw %v clicks after an explicit enable, want 1", got)
	}

	// The forwarded event belongs in the trace, attributed to the human.
	found := false
	for _, entry := range manager.GetTrace().Entries {
		if entry.Action == TraceActionHumanInput {
			found = true
		}
	}
	if !found {
		t.Error("a human's forwarded input left no trace entry; the feed would show the page changing with nothing that did it")
	}
}

// While a human holds the browser, an agent action is refused by name. It is
// neither queued (the counter does not move when the hold ends) nor silently
// dropped (the caller gets a typed error, not a success).
func TestAgentActionIsRefusedWhileAHumanHoldsTakeover(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := takeoverFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/page"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	snap, err := manager.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all", Limit: 50})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	ref := ""
	for _, element := range snap.Elements {
		if element.Name == "Go" {
			ref = element.Ref
		}
	}
	if ref == "" {
		t.Fatalf("fixture button has no ref in %+v", snap.Elements)
	}

	grant, err := manager.AcquireTakeover("operator", time.Minute)
	if err != nil {
		t.Fatalf("acquire takeover: %v", err)
	}
	result, err := manager.Click(ctx, ref)
	var refused *TakeoverRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("click during takeover = (%+v, %v), want a *TakeoverRefusedError", result, err)
	}
	if refused.Action != "click" {
		t.Errorf("refusal names %q, want \"click\"", refused.Action)
	}
	if got := clickCount(t, ctx, manager); got != 0 {
		t.Fatalf("a refused click still reached the page (%v clicks)", got)
	}

	// Reading is deliberately still allowed: it is how the agent finds out what
	// the human did before it resumes.
	if _, err := manager.Read(ctx); err != nil {
		t.Fatalf("read during takeover: %v", err)
	}

	if err := manager.ReleaseTakeover(grant.Token); err != nil {
		t.Fatalf("release takeover: %v", err)
	}
	if _, err := manager.Click(ctx, ref); err != nil {
		t.Fatalf("click after release: %v", err)
	}
	if got := clickCount(t, ctx, manager); got != 1 {
		t.Fatalf("the page saw %v clicks after one click; a refused action must not be replayed on release", got)
	}
}

// The activity feed is the trace stream, so an action has to reach a live
// subscriber promptly enough to explain the frame the operator is watching.
func TestTraceStreamDeliversAnActionToAWatcherPromptly(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := takeoverFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/page"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	snap, err := manager.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all", Limit: 50})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	ref := ""
	for _, element := range snap.Elements {
		if element.Name == "Go" {
			ref = element.Ref
		}
	}
	if ref == "" {
		t.Fatalf("fixture button has no ref in %+v", snap.Elements)
	}

	entries, unsubscribe := manager.SubscribeTrace()
	defer unsubscribe()

	// The budget runs from the moment the action starts, not from the moment it
	// returns: what the operator is waiting for is an explanation of the frame
	// that just changed under them.
	started := time.Now()
	if _, err := manager.Click(ctx, ref); err != nil {
		t.Fatalf("click: %v", err)
	}
	deadline := time.After(time.Until(started.Add(500 * time.Millisecond)))
	for {
		select {
		case entry, open := <-entries:
			if !open {
				t.Fatal("the stream closed before the click arrived")
			}
			if entry.Action != "click" {
				continue
			}
			// The feed line has to say what was acted on and how it went, or an
			// operator watching a page change still cannot tell which step did it.
			if entry.Ref != ref {
				t.Errorf("feed entry ref = %q, want %q", entry.Ref, ref)
			}
			if entry.Name == "" {
				t.Error("feed entry carries no accessible name")
			}
			if !entry.OK {
				t.Errorf("feed entry reports failure: %s", entry.Error)
			}
			if entry.DurationMS <= 0 {
				t.Errorf("feed entry latency = %dms, want the measured duration", entry.DurationMS)
			}
			t.Logf("click reached the feed %v after the action began", time.Since(started))
			return
		case <-deadline:
			t.Fatal("no click entry reached the watcher within 500ms of the action starting")
		}
	}
}

// brw_evaluate is the highest-leverage page-acting route in the product: one string
// can click, type and navigate. A hold that refuses brw_click and lets this
// through is not a handshake. Asserted on the PAGE, not on the returned error.
func TestEvaluateIsRefusedWhileAHumanHoldsTakeover(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := takeoverFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/page"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := manager.AcquireTakeover("operator", time.Minute); err != nil {
		t.Fatalf("acquire takeover: %v", err)
	}

	tests := []struct {
		name       string
		ctx        context.Context
		expression string
	}{
		{"a click through eval", ctx, "document.getElementById('go').click()"},
		{
			"a field write through eval",
			ctx,
			"document.getElementById('field').value = 'agent typed this'",
		},
		{
			// The label is a request field on /api/page/evaluate, so anything
			// that can call the route can claim its script is a typed read.
			"a click wearing a read's label",
			WithTraceLabel(ctx, TraceActionGet, "text #count"),
			"document.getElementById('go').click()",
		},
		{
			// The generated read script is a caller-supplied string like any
			// other, so a check that only constrains its start leaves everything
			// after the call's closing paren free: a whole evaluate riding in on
			// the exemption that keeps brw_get answering during a hold.
			"a click appended to a generated read script",
			WithTraceLabel(ctx, TraceActionGet, "text #count"),
			snapshot.BuildGetExpression("text", "#count", "") +
				";document.getElementById('go').click();document.getElementById('field').value='agent typed this';1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, err := manager.Evaluate(tt.ctx, tt.expression)
			var refused *TakeoverRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("evaluate during takeover = (%v, %v), want a *TakeoverRefusedError", value, err)
			}
			if refused.Action != "evaluate_script" {
				t.Errorf("refusal names %q, want \"evaluate_script\"", refused.Action)
			}
		})
	}

	if got := clickCount(t, ctx, manager); got != 0 {
		t.Fatalf("the page saw %v clicks from a refused evaluate", got)
	}
	field, err := generatedGet(ctx, manager, "value", "#field")
	if err != nil {
		t.Fatalf("read the field during a hold: %v", err)
	}
	if field != "" {
		t.Fatalf("a refused evaluate still wrote %q into the form field", field)
	}
}

// A batch already in flight when the human takes over must stop. ExecuteBatch
// guards once at entry and its steps reach the low-level helpers directly, so
// without a per-step guard the whole remaining batch keeps driving the page.
func TestABatchInFlightStopsDrivingThePageOnTakeover(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := takeoverFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/slow"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}

	const steps = 6
	batch := make([]BatchStep, 0, steps)
	for i := 0; i < steps; i++ {
		batch = append(batch, BatchStep{Action: "click_text", Text: "Go"})
	}

	type outcome struct {
		result BatchResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := manager.ExecuteBatch(ctx, batch)
		done <- outcome{result, err}
	}()

	// Long enough that the batch is genuinely mid-flight, short enough that it
	// cannot have finished: each click holds the renderer for 250ms.
	time.Sleep(150 * time.Millisecond)
	if _, err := manager.AcquireTakeover("operator", time.Minute); err != nil {
		t.Fatalf("acquire takeover mid-batch: %v", err)
	}

	var got outcome
	select {
	case got = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the batch never returned after the hold was taken")
	}
	if got.err != nil {
		t.Fatalf("batch returned an error rather than a refused step: %v", got.err)
	}
	if got.result.OK {
		t.Fatal("the batch reported success; every step ran through the hold")
	}
	if !strings.Contains(got.result.Error, "holds takeover of this browser") {
		t.Fatalf("batch failed with %q, want the named takeover refusal", got.result.Error)
	}
	if got.result.StepsCompleted >= steps {
		t.Fatalf("%d of %d steps completed; the batch ran to the end during a hold", got.result.StepsCompleted, steps)
	}
	if count := clickCount(t, ctx, manager); count >= steps {
		t.Fatalf("the page saw %v clicks of %d; the batch kept driving it after the hold", count, steps)
	} else {
		t.Logf("the batch stopped after %v clicks and %d steps", count, got.result.StepsCompleted)
	}
}

// The hold is bound to a tab. Resolving the target at dispatch time would send
// the human's click — aimed at the pixels of the tab on their screen — into
// whichever tab the browser had drifted to, which is the same two-actor race
// running in the other direction.
func TestTakeoverInputStaysOnTheTabTheHoldWasTakenOn(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := takeoverFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	held, err := manager.Open(ctx, srv.URL+"/page")
	if err != nil {
		t.Fatalf("open the held tab: %v", err)
	}
	other, err := manager.Open(ctx, srv.URL+"/page")
	if err != nil {
		t.Fatalf("open the second tab: %v", err)
	}
	// Open leaves the newest tab active, so put the human back on the first one.
	if err := manager.FocusTab(ctx, held.Tab.ID); err != nil {
		t.Fatalf("focus the held tab: %v", err)
	}

	grant, err := manager.AcquireTakeover("operator", time.Minute)
	if err != nil {
		t.Fatalf("acquire takeover: %v", err)
	}
	if grant.TabID != held.Tab.ID {
		t.Fatalf("grant bound to tab %q, want the active tab %q", grant.TabID, held.Tab.ID)
	}

	// An agent cannot move the active tab during a hold — the verbs are refused
	// below — so move it the way a path that escaped the guard would.
	manager.refs.SetActive(other.Tab.ID)

	press := TakeoverInput{Kind: "mouse", Type: "mousePressed", X: 120, Y: 40, Button: "left", Buttons: 1, ClickCount: 1}
	release := TakeoverInput{Kind: "mouse", Type: "mouseReleased", X: 120, Y: 40, Button: "left", ClickCount: 1}
	for _, event := range []TakeoverInput{press, release} {
		if err := manager.DispatchTakeoverInput(ctx, grant.Token, event); err != nil {
			t.Fatalf("forward %s: %v", event.Type, err)
		}
	}

	if got := clickCountOnTab(t, ctx, manager, held.Tab.ID); got != 1 {
		t.Fatalf("the held tab saw %v clicks, want 1: the human's input went somewhere else", got)
	}
	if got := clickCountOnTab(t, ctx, manager, other.Tab.ID); got != 0 {
		t.Fatalf("the tab the human was NOT driving saw %v clicks", got)
	}

	// And the verbs that move the target are refused for as long as the hold
	// lasts, so nothing has to be recovered from afterwards.
	for _, tc := range []struct {
		action string
		call   func() error
	}{
		{"open", func() error { _, err := manager.Open(ctx, srv.URL+"/page"); return err }},
		{"focus_tab", func() error { return manager.FocusTab(ctx, other.Tab.ID) }},
		{"close_tab", func() error { return manager.CloseTab(ctx, held.Tab.ID) }},
	} {
		var refused *TakeoverRefusedError
		if err := tc.call(); !errors.As(err, &refused) {
			t.Errorf("%s during a hold = %v, want a *TakeoverRefusedError", tc.action, err)
		} else if refused.Action != tc.action {
			t.Errorf("refusal names %q, want %q", refused.Action, tc.action)
		}
	}
	if tabs, err := manager.ListTabs(ctx); err != nil {
		t.Fatalf("list tabs: %v", err)
	} else {
		found := false
		for _, tab := range tabs {
			if tab.ID == held.Tab.ID {
				found = true
			}
		}
		if !found {
			t.Fatal("the tab the human was driving was closed during the hold")
		}
	}
}

// A grant that ends while the human's event is between the token check and the
// renderer is the two-actor race narrowed to a few milliseconds rather than
// closed. A release waits for the event; an expiry cannot be waited for, so the
// event is reported as refused instead of as delivered under a hold that had
// already gone.
func TestAHoldThatEndsMidDispatchDoesNotReportTheEventAsDelivered(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := takeoverFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/slow"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	grant, err := manager.AcquireTakeover("operator", time.Minute)
	if err != nil {
		t.Fatalf("acquire takeover: %v", err)
	}

	// The press stalls in the renderer for a quarter second; end the hold while
	// it is in there.
	go func() {
		time.Sleep(60 * time.Millisecond)
		_, _ = manager.RenewTakeover(grant.Token, time.Millisecond)
	}()
	err = manager.DispatchTakeoverInput(ctx, grant.Token,
		TakeoverInput{Kind: "mouse", Type: "mousePressed", X: 120, Y: 40, Button: "left", Buttons: 1, ClickCount: 1})
	if !errors.Is(err, ErrTakeoverNotHeld) {
		t.Fatalf("dispatch across the end of the hold = %v, want ErrTakeoverNotHeld", err)
	}
}

// ReleaseTakeover has to wait for an event already on its way to the renderer.
// Releasing out from under one puts the human's click on a page the agent has
// just been told it may drive again.
//
// The event is a real one: the /slow fixture stalls a mousedown in the renderer
// for a quarter second, so the dispatch genuinely sits between its token check
// and the page while the release is attempted. Taking the dispatch lock in the
// test body instead would assert nothing about DispatchTakeoverInput — it would
// only re-measure an RWMutex.
func TestReleaseWaitsForAnEventAlreadyOnItsWayToTheRenderer(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := takeoverFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/slow"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	grant, err := manager.AcquireTakeover("operator", time.Minute)
	if err != nil {
		t.Fatalf("acquire takeover: %v", err)
	}

	dispatched := make(chan error, 1)
	go func() {
		dispatched <- manager.DispatchTakeoverInput(ctx, grant.Token,
			TakeoverInput{Kind: "mouse", Type: "mousePressed", X: 120, Y: 40, Button: "left", Buttons: 1, ClickCount: 1})
	}()

	// Long enough for the press to reach the stalled listener, short enough to
	// leave most of the fixture's quarter second for the release to be blocked by.
	const (
		inFlight  = 100 * time.Millisecond
		mustBlock = 40 * time.Millisecond
	)
	time.Sleep(inFlight)
	if err := manager.guardTakeover("click"); err == nil {
		t.Fatal("an agent click was accepted while a human event was still in flight")
	}

	releaseStarted := time.Now()
	releaseErr := manager.ReleaseTakeover(grant.Token)
	blocked := time.Since(releaseStarted)
	dispatchErr := <-dispatched

	if releaseErr != nil {
		t.Fatalf("release: %v", releaseErr)
	}
	// A dispatch that came back refused means the release got in first and
	// invalidated the token, which is the race this lock exists to close.
	if dispatchErr != nil {
		t.Fatalf("the forwarded press = %v with the fixture still stalling it; the release ended the hold out from under it", dispatchErr)
	}
	if blocked < mustBlock {
		t.Fatalf("release returned after %s with a press still in the renderer; it must wait for the event, not race it", blocked)
	}
	t.Logf("release waited %s for the in-flight press", blocked)

	if manager.TakeoverState().Held {
		t.Fatal("the hold survived its own release")
	}
	if err := manager.guardTakeover("click"); err != nil {
		t.Fatalf("agent actions are still refused after release: %v", err)
	}
}

// An assertion only reads, and every assert step is classified read-only so a
// hold does not stop the agent finding out what the human did. The assertion
// evaluator reads through the same generated getters script brw_get uses, so
// this asserts the whole path rather than the guard's classification: a step the
// guard admits must not then be refused one layer below it.
func TestAssertionsStillAnswerWhileAHumanHoldsTakeover(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := takeoverFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/page"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := manager.AcquireTakeover("operator", time.Minute); err != nil {
		t.Fatalf("acquire takeover: %v", err)
	}

	one := 1
	tests := []struct {
		name string
		req  AssertRequest
	}{
		{"url", AssertRequest{Assertion: AssertionURL, Expected: srv.URL + "/page", Mode: AssertModeExact}},
		{"http status", AssertRequest{Assertion: AssertionHTTPStatus, Status: 200}},
		{"element count", AssertRequest{Assertion: AssertionElementCount, Selector: "#go", Count: &one}},
		{"element state", AssertRequest{Assertion: AssertionElementState, Ref: "#field", State: AssertStateEditable}},
		{"attribute", AssertRequest{Assertion: AssertionAttribute, Ref: "#field", Attribute: "aria-label", Expected: "Field"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Assert(ctx, manager, tt.req)
			if err != nil {
				t.Fatalf("assert during a hold = %v (%+v); a read-only step the guard admits must not be refused below it", err, result)
			}
			if !result.OK {
				t.Fatalf("assert during a hold reported %+v", result)
			}
		})
	}
}
