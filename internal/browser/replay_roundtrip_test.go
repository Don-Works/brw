package browser

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/Don-Works/brw/internal/snapshot"
	"strings"
	"testing"
	"time"
)

// formSite serves a form that records what it was submitted with, so a replay
// can be checked against the page's real end state rather than against brw's
// own report of what it did.
func formSite(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body>
<form id="f">
  <input type="text" aria-label="Full name" id="name">
  <select aria-label="Country" id="country">
    <option value="">Choose</option>
    <option value="uk">United Kingdom</option>
    <option value="us">United States</option>
  </select>
  <button type="button" id="go">Submit</button>
</form>
<p id="result">nothing yet</p>
<script>
  document.getElementById("go").addEventListener("click", function () {
    document.getElementById("result").textContent =
      "submitted:" + document.getElementById("name").value +
      "/" + document.getElementById("country").value;
  });
</script>
</body></html>`))
	}))
}

func snapshotAllOptions() snapshot.SnapshotOptions {
	return snapshot.SnapshotOptions{Mode: "all"}
}

func refForName(elements []snapshot.Element, name string) string {
	for _, el := range elements {
		if strings.EqualFold(strings.TrimSpace(el.Name), name) {
			return el.Ref
		}
	}
	return ""
}

func fillOptionsFor(ref, text string) snapshot.FillOptions {
	return snapshot.FillOptions{Ref: ref, Text: text, Replace: true}
}

func resultText(t *testing.T, m *Manager, ctx context.Context) string {
	t.Helper()
	value, err := m.Evaluate(ctx, `document.getElementById("result").textContent`)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	text, _ := value.(string)
	return text
}

// TestTraceReplayRoundTripReproducesEndState is the claim the feature makes:
// perform a flow once, export it, run the export, and land on the same page
// state — with no model in the replay loop. Anything less is a demo.
func TestTraceReplayRoundTripReproducesEndState(t *testing.T) {
	site := formSite(t)
	defer site.Close()

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)

	// A snapshot populates the ref store, which is what gives the trace the
	// element identities the guards are built from.
	snap, err := m.Snapshot(tabCtx, snapshotAllOptions())
	if err != nil {
		t.Fatal(err)
	}
	nameRef := refForName(snap.Elements, "Full name")
	countryRef := refForName(snap.Elements, "Country")
	submitRef := refForName(snap.Elements, "Submit")
	if nameRef == "" || countryRef == "" || submitRef == "" {
		t.Fatalf("could not resolve form refs: %+v", snap.Elements)
	}

	m.ClearTrace()

	// Perform the flow by hand, exactly as an agent would.
	if _, err := m.Fill(tabCtx, fillOptionsFor(nameRef, "Ada Lovelace")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Select(tabCtx, countryRef, "uk"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Click(tabCtx, submitRef); err != nil {
		t.Fatal(err)
	}

	const want = "submitted:Ada Lovelace/uk"
	if got := resultText(t, m, tabCtx); got != want {
		t.Fatalf("recording did not reach the expected state: got %q want %q", got, want)
	}

	// Export the flow, then prove the export is complete enough to stand alone.
	replay := TraceToBatch(m.GetTrace(), ReplayOptions{})
	if replay.Actions != 3 {
		t.Fatalf("exported %d actions, want the fill, select and click: %+v", replay.Actions, replay.Steps)
	}
	if replay.Guards != 1 {
		t.Fatalf("guards = %d, want one identity check on the Submit button", replay.Guards)
	}

	// The recorded element identities must have been captured from the ref
	// store; without them the guard is silently absent.
	var sawGuard bool
	for _, step := range replay.Steps {
		if step.Action == "assert_text" && step.Text == "Submit" {
			sawGuard = true
		}
	}
	if !sawGuard {
		t.Fatalf("no assert_text guard for the Submit button: %+v", replay.Steps)
	}

	// Reset the page, then replay the exported steps and nothing else.
	if _, err := m.NavigateTo(tabCtx, site.URL); err != nil {
		t.Fatal(err)
	}
	if got := resultText(t, m, tabCtx); got != "nothing yet" {
		t.Fatalf("page did not reset before replay: %q", got)
	}

	batch, err := m.ExecuteBatch(tabCtx, replay.Steps)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !batch.OK {
		t.Fatalf("replay failed: %+v", batch)
	}
	if got := resultText(t, m, tabCtx); got != want {
		t.Fatalf("replay did not reproduce the end state: got %q want %q", got, want)
	}
}

// A guard exists to stop a replay acting on an element that is no longer the
// one that was recorded. If it does not actually fail on a changed page, it is
// decoration.
func TestReplayGuardFailsWhenTheElementChanged(t *testing.T) {
	site := formSite(t)
	defer site.Close()

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)

	snap, err := m.Snapshot(tabCtx, snapshotAllOptions())
	if err != nil {
		t.Fatal(err)
	}
	submitRef := refForName(snap.Elements, "Submit")
	if submitRef == "" {
		t.Fatal("no Submit button")
	}

	m.ClearTrace()
	if _, err := m.Click(tabCtx, submitRef); err != nil {
		t.Fatal(err)
	}
	replay := TraceToBatch(m.GetTrace(), ReplayOptions{})
	if replay.Guards != 1 {
		t.Fatalf("expected a guard for the button, got %+v", replay.Steps)
	}

	// Repurpose the button: same ref, different element identity. A replay must
	// refuse rather than click whatever is there now.
	if _, err := m.Evaluate(tabCtx, `document.getElementById("go").textContent = "Delete account"; true`); err != nil {
		t.Fatal(err)
	}

	batch, err := m.ExecuteBatch(tabCtx, replay.Steps)
	if err == nil && batch.OK {
		t.Fatal("replay proceeded against a changed element; the guard did nothing")
	}
}
