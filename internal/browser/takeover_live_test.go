package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// The button is pinned at a known position so a forwarded event can name real
// viewport coordinates without first resolving a ref — which is the whole point
// of takeover: the human aims with their eyes, not with the ref graph.
const takeoverFixture = `<!doctype html><html><head><title>Takeover fixture</title></head><body style="margin:0">
<button id="go" style="position:fixed;left:0;top:0;width:240px;height:80px">Go</button>
<input id="field" style="position:fixed;left:0;top:100px;width:240px;height:40px" aria-label="Field">
<script>
window.__clicks = 0;
document.getElementById('go').addEventListener('click', function(){ window.__clicks++; });
</script>
</body></html>`

func takeoverFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, takeoverFixture)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func clickCount(t *testing.T, ctx context.Context, m *Manager) float64 {
	t.Helper()
	value, err := m.Evaluate(ctx, "window.__clicks")
	if err != nil {
		t.Fatalf("read the click counter: %v", err)
	}
	count, ok := value.(float64)
	if !ok {
		t.Fatalf("click counter is %T (%v), want a number", value, value)
	}
	return count
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
