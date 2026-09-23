package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Don-Works/brw/internal/usagelog"
)

func ownersOf(t *testing.T, runs ...[]string) []string {
	t.Helper()
	var mu sync.Mutex
	var owners []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		owners = append(owners, r.Header.Get(usagelog.HeaderOwnerID))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"url": "https://example.test/", "title": "t"})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BRW_URL", srv.URL)
	for _, args := range runs {
		var stdout, stderr bytes.Buffer
		Run(context.Background(), args, &stdout, &stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), owners...)
}

func TestSeparateCLIRunsShareOneLeaseOwner(t *testing.T) {
	t.Setenv("BRW_OWNER_ID", "")
	t.Setenv("MCPLEXER_BROWSER_SESSION_ID", "")
	owners := ownersOf(t, []string{"read"}, []string{"read"})
	if len(owners) < 2 {
		t.Fatalf("daemon saw %d requests, want 2", len(owners))
	}
	if owners[0] == "" || owners[0] != owners[len(owners)-1] {
		t.Fatalf("each brw run leased as a different owner, so the tab one opened is locked to the next: %q", owners)
	}
}

func TestBRWOwnerIDOverridesTheCLIOwner(t *testing.T) {
	t.Setenv("MCPLEXER_BROWSER_SESSION_ID", "")
	t.Setenv("BRW_OWNER_ID", "agent-a")
	a := ownersOf(t, []string{"read"})
	t.Setenv("BRW_OWNER_ID", "agent-b")
	b := ownersOf(t, []string{"read"})
	if len(a) == 0 || len(b) == 0 || a[0] == b[0] {
		t.Fatalf("BRW_OWNER_ID did not separate owners: %q vs %q", a, b)
	}
}
