package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The guard has to assert what the element said BEFORE the click, not after.
// recordTrace resolved the name from the ref store, which the post-action
// observation had already refreshed, so a button whose label flips on click was
// recorded under its new label — and the replay then asserted that new label on
// a fresh page where the button still shows the old one, failing every time.
func TestTraceRecordsPreActionIdentity(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body>
<button type="button" id="toggle">Enable</button>
<script>
  document.getElementById("toggle").addEventListener("click", function () {
    this.textContent = "Disable";
  });
</script>
</body></html>`))
	}))
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
	toggleRef := refForName(snap.Elements, "Enable")
	if toggleRef == "" {
		t.Fatalf("no Enable button: %+v", snap.Elements)
	}

	m.ClearTrace()
	if _, err := m.Click(tabCtx, toggleRef); err != nil {
		t.Fatal(err)
	}

	entries := m.GetTrace().Entries
	if len(entries) != 1 {
		t.Fatalf("trace = %+v, want one click", entries)
	}
	if entries[0].Name != "Enable" {
		t.Fatalf("recorded name = %q, want the label the button had when it was clicked", entries[0].Name)
	}

	// The exported guard must therefore assert the pre-click label, which is what
	// a fresh page will show.
	replay := TraceToBatch(m.GetTrace(), ReplayOptions{})
	var guard *BatchStep
	for i := range replay.Steps {
		if replay.Steps[i].Action == "assert_text" {
			guard = &replay.Steps[i]
		}
	}
	if guard == nil {
		t.Fatalf("no guard emitted for a text-labelled button: %+v", replay.Steps)
	}
	if guard.Text != "Enable" {
		t.Fatalf("guard asserts %q, want Enable — the label a fresh page shows", guard.Text)
	}

	// And the whole thing must actually replay against a fresh page.
	if _, err := m.NavigateTo(tabCtx, site.URL); err != nil {
		t.Fatal(err)
	}
	batch, err := m.ExecuteBatch(tabCtx, replay.Steps)
	if err != nil || !batch.OK {
		t.Fatalf("replay failed: err=%v result=%+v", err, batch)
	}
}
