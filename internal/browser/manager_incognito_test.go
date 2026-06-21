package browser

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
)

func TestManagerOpenIncognitoCreatesIsolatedContext(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := m.OpenIncognito(ctx, "about:blank")
	if err != nil {
		t.Fatalf("OpenIncognito: %v", err)
	}
	if res.Tab.ID == "" {
		t.Fatal("expected a tab id")
	}
	if res.Tab.BrowserContextID == "" {
		t.Fatal("expected a non-empty context_id (incognito context)")
	}

	// The opened target must belong to the new incognito context, not the default.
	infos := getTargets(t, m, ctx)
	var found *target.Info
	for _, in := range infos {
		if string(in.TargetID) == res.Tab.ID {
			found = in
			break
		}
	}
	if found == nil {
		t.Fatalf("opened incognito target %s not found among %d targets", res.Tab.ID, len(infos))
	}
	if string(found.BrowserContextID) != res.Tab.BrowserContextID {
		t.Fatalf("target context = %q, want %q", found.BrowserContextID, res.Tab.BrowserContextID)
	}

	// Disposing the context must close its tab.
	if err := m.CloseContext(ctx, res.Tab.BrowserContextID); err != nil {
		t.Fatalf("CloseContext: %v", err)
	}
	for _, in := range getTargets(t, m, ctx) {
		if string(in.TargetID) == res.Tab.ID {
			t.Fatalf("incognito tab %s still present after CloseContext", res.Tab.ID)
		}
	}

	// An empty context id is an explicit error, never a silent no-op.
	if err := m.CloseContext(ctx, ""); err == nil {
		t.Fatal("expected error for empty context_id")
	}
}

func getTargets(t *testing.T, m *Manager, ctx context.Context) []*target.Info {
	t.Helper()
	var infos []*target.Info
	if err := m.runBrowser(ctx, func(ctx context.Context) error {
		var e error
		infos, e = target.GetTargets().Do(ctx)
		return e
	}); err != nil {
		t.Fatalf("GetTargets: %v", err)
	}
	return infos
}
