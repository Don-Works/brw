package browser

import (
	"context"
	"testing"
	"time"
)

func TestInitScriptRunsImmediatelyAndOnReload(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	url := serveEnvironmentFixture(t)
	emulationTab(t, m, ctx, url)

	added, err := m.AddInitScript(ctx, InitScriptOptions{Source: "window.__brwInit = (window.__brwInit||0)+1;"})
	if err != nil {
		t.Fatalf("AddInitScript: %v", err)
	}
	if added.Added == nil || added.Added.ID == "" {
		t.Fatalf("added = %+v", added)
	}
	if evaluateString(t, m, ctx, `String(window.__brwInit||0)`) != "1" {
		t.Fatal("script did not run immediately on the open document")
	}

	if _, err := m.NavigateTo(ctx, url+"?again=1"); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if evaluateString(t, m, ctx, `String(window.__brwInit||0)`) != "1" {
		t.Fatal("script did not run at document-start on the next navigation")
	}

	if _, err := m.RemoveInitScript(ctx, InitScriptRemoveOptions{ID: added.Added.ID}); err != nil {
		t.Fatalf("RemoveInitScript: %v", err)
	}
}
