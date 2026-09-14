package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

const focusFixture = `<!doctype html><html><head><meta charset="utf-8"><title>focus fixture</title></head>
<body style="margin:0">
<input id="outer" aria-label="Outer Field">
<iframe id="embed" style="width:300px;height:120px;border:0"
  srcdoc="<!doctype html><html><body><input id='inner' aria-label='Inner Field'></body></html>"></iframe>
</body></html>`

// TestFocusRefFocusesByRefAcrossFrames covers the explicit focus surface: give
// one element the keyboard focus without clicking it, including an element
// inside a same-origin iframe, where a plain document.activeElement assignment
// in the top document cannot reach.
func TestFocusRefFocusesByRefAcrossFrames(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, focusFixture)
	}))
	defer srv.Close()
	opened, err := m.Open(ctx, srv.URL)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)

	snap, err := m.Snapshot(tabCtx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	refs := map[string]string{}
	for _, el := range snap.Elements {
		refs[el.Name] = el.Ref
	}
	if refs["Outer Field"] == "" || refs["Inner Field"] == "" {
		t.Fatalf("snapshot did not surface both fields; got %v", refs)
	}

	if err := m.FocusRef(tabCtx, refs["Outer Field"]); err != nil {
		t.Fatalf("focus outer field: %v", err)
	}
	if active := evalString(t, m, tabCtx, "document.activeElement.id"); active != "outer" {
		t.Fatalf("document.activeElement = %q after focusing the outer field, want outer", active)
	}

	if err := m.FocusRef(tabCtx, refs["Inner Field"]); err != nil {
		t.Fatalf("focus in-frame field: %v", err)
	}
	if active := evalString(t, m, tabCtx, "document.activeElement.id"); active != "embed" {
		t.Fatalf("top-level activeElement = %q, want the iframe to hold focus", active)
	}
	inner := evalString(t, m, tabCtx, "document.getElementById('embed').contentDocument.activeElement.id")
	if inner != "inner" {
		t.Fatalf("in-frame activeElement = %q, want inner", inner)
	}

	if err := m.FocusRef(tabCtx, "e-nonexistent"); err == nil {
		t.Fatal("focusing an unknown ref should fail rather than silently focus nothing")
	}
}

// TestFocusObservesThePageAfterFocusing covers the tool-facing surface rather
// than the recipe-internal FocusRef the test above drives. brw_focus is an
// action tool, and every action tool answers with the post-action observation:
// an agent that focuses a field and gets back {ok:true,ref:"e4"} cannot tell
// whether focus landed there, on nothing, or on whatever the control moved it
// to. result.Focus is what makes that checkable.
func TestFocusObservesThePageAfterFocusing(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, focusFixture)
	}))
	defer srv.Close()
	opened, err := m.Open(ctx, srv.URL)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)

	snap, err := m.Snapshot(tabCtx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	ref := ""
	for _, el := range snap.Elements {
		if el.Name == "Outer Field" {
			ref = el.Ref
		}
	}
	if ref == "" {
		t.Fatalf("no ref for the outer field; snapshot returned %d elements", len(snap.Elements))
	}

	result, err := m.Focus(tabCtx, ref)
	if err != nil {
		t.Fatalf("focus: %v", err)
	}
	if !result.OK {
		t.Fatalf("focus result = %+v, want ok", result)
	}
	if result.Focus != ref {
		t.Fatalf("focus result reported focus %q, want the ref it focused (%s)", result.Focus, ref)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"message", result.Message},
		{"url", result.URL},
		{"title", result.Title},
		{"tab_id", result.TabID},
	} {
		if field.value == "" {
			t.Fatalf("focus result carried no %s: %+v", field.name, result)
		}
	}
	// The observation has to agree with the document, not just with itself.
	if active := evalString(t, m, tabCtx, "document.activeElement.id"); active != "outer" {
		t.Fatalf("document.activeElement = %q, want outer", active)
	}

	if _, err := m.Focus(tabCtx, "  "); err == nil {
		t.Fatal("focus with a blank ref should be rejected")
	}
	if _, err := m.Focus(tabCtx, "e-nonexistent"); err == nil {
		t.Fatal("focusing an unknown ref should fail rather than report a successful no-op")
	}
}
