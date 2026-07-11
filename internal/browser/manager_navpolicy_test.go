package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/snapshot"
)

// This is deliberately a real Chrome test: input validation alone cannot prove
// that a link click or server redirect is confined after the browser commits it.
func TestManagerNavigationPolicyConfinesCommittedDestinations(t *testing.T) {
	var blockedBase string
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			fmt.Fprintf(w, `<!doctype html><a href="%s/blocked">Escape destination</a>`, blockedBase)
		case "/redirect":
			http.Redirect(w, r, blockedBase+"/blocked", http.StatusFound)
		default:
			fmt.Fprint(w, "blocked destination")
		}
	}))
	defer site.Close()
	u, err := url.Parse(site.URL)
	if err != nil {
		t.Fatal(err)
	}
	// httptest listens on 127.0.0.1. The alternate localhost spelling reaches
	// the same server but is a distinct policy host, making a deterministic
	// cross-boundary redirect/link without external network access.
	blockedBase = "http://localhost:" + u.Port()

	m := newHeadlessManager(t)
	m.SetNavigationPolicy(navpolicy.Parse("127.0.0.1", ""))
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, site.URL+"/")
	if err != nil {
		t.Fatalf("open allowlisted page: %v", err)
	}
	snap, err := m.Snapshot(WithTabID(ctx, opened.Tab.ID), snapshot.SnapshotOptions{})
	if err != nil {
		t.Fatalf("snapshot allowlisted page: %v", err)
	}
	var escapeRef string
	for _, el := range snap.Elements {
		if strings.Contains(el.Name, "Escape destination") {
			escapeRef = el.Ref
			break
		}
	}
	if escapeRef == "" {
		t.Fatalf("escape link missing from snapshot: %+v", snap.Elements)
	}

	clicked, err := m.Click(WithTabID(ctx, opened.Tab.ID), escapeRef)
	if err != nil {
		t.Fatalf("click escaped as transport error instead of an observed policy result: %v", err)
	}
	if clicked.OK || !strings.Contains(clicked.Message, "navigation policy") {
		t.Fatalf("click result = %+v, want failed navigation-policy observation", clicked)
	}
	tab, err := m.tabByID(ctx, opened.Tab.ID)
	if err != nil {
		t.Fatalf("inspect contained tab: %v", err)
	}
	if tab.URL != "about:blank" {
		t.Fatalf("escaped click left tab at %q, want about:blank containment", tab.URL)
	}

	before, err := m.ListTabs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(ctx, site.URL+"/redirect"); err == nil || !strings.Contains(err.Error(), "disallowed final destination") {
		t.Fatalf("redirect open error = %v, want final-destination policy rejection", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		after, listErr := m.ListTabs(ctx)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(after) <= len(before) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("blocked redirect leaked a tab: before=%d after=%d (%+v)", len(before), len(after), after)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
