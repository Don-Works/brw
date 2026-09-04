package browser

import (
	"context"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/cdp"
)

func TestManagerDocumentIdentityTracksReplacementButNotSPADocuments(t *testing.T) {
	if _, err := cdp.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	srv := navFixtureServer()
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m, err := New(ctx, Config{
		Timeout:    20 * time.Second,
		ChromeArgs: []string{"--headless=new", "--disable-gpu", "--no-sandbox"},
	})
	if err != nil {
		t.Skipf("could not launch headless Chrome: %v", err)
	}
	defer m.Close()

	opened, err := m.Open(ctx, srv.URL+"/a")
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	waitForMarker(t, tabCtx, m, "page-A-marker")
	first, err := m.DocumentIdentity(tabCtx)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.Origin == "" {
		t.Fatalf("empty first identity: %+v", first)
	}

	if _, err := m.Evaluate(tabCtx, `history.pushState({}, "", "/a?spa=1#same-document")`); err != nil {
		t.Fatal(err)
	}
	spa, err := m.DocumentIdentity(tabCtx)
	if err != nil {
		t.Fatal(err)
	}
	if spa != first {
		t.Fatalf("same-document SPA history changed identity: first=%+v spa=%+v", first, spa)
	}

	if _, err := m.NavigateTo(tabCtx, srv.URL+"/b"); err != nil {
		t.Fatal(err)
	}
	waitForMarker(t, tabCtx, m, "page-B-marker")
	replacement, err := m.DocumentIdentity(tabCtx)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == first.ID {
		t.Fatal("same-origin replacement document retained its prior identity")
	}
	if replacement.Origin != first.Origin {
		t.Fatalf("same-origin navigation changed security origin: first=%q replacement=%q", first.Origin, replacement.Origin)
	}

	if _, err := m.Navigate(tabCtx, NavigateBack); err != nil {
		t.Fatal(err)
	}
	waitForMarker(t, tabCtx, m, "page-A-marker")
	returned, err := m.DocumentIdentity(tabCtx)
	if err != nil {
		t.Fatal(err)
	}
	if returned.ID == first.ID {
		t.Fatal("A -> B -> back-to-A transition was mistaken for uninterrupted document continuity")
	}
}
