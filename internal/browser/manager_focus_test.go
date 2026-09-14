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
