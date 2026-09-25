package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// A site registers its tools while its first document loads. Open has to arm
// the shim on the blank tab BEFORE navigating, or those registrations land on a
// page with no document.modelContext and are lost; before this, only a later
// Snapshot armed it, which was too late for exactly this page.
func TestOpenArmsWebMCPBeforeTheFirstDocument(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>shop</h1><script>
  if (document.modelContext) document.modelContext.registerTool({
    name: 'find_product', description: 'Find a product by name',
    annotations: { readOnlyHint: true }, execute: function(a){ return { q: a.q }; } });
</script></body></html>`))
	}))
	defer srv.Close()

	cases := []struct {
		name      string
		enabled   bool
		wantTools []snapshot.PageToolSummary
	}{
		{name: "enabled: the load-time registration is seen", enabled: true,
			wantTools: []snapshot.PageToolSummary{{Name: "find_product", Description: "Find a product by name", ReadOnly: true}}},
		{name: "disabled: no runtime is fabricated", enabled: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newHeadlessManager(t)
			m.webmcpEnabled = tc.enabled
			m.webmcpTabs = map[string]bool{}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if _, err := m.Open(ctx, srv.URL); err != nil {
				t.Fatalf("open: %v", err)
			}
			digest, err := snapshot.ReadPageSurfaces(ctx, func(ctx context.Context, expression string) (any, error) {
				return m.Evaluate(ctx, expression)
			})
			if err != nil {
				t.Fatalf("read page surfaces: %v", err)
			}
			if len(digest.Tools) != len(tc.wantTools) {
				t.Fatalf("tools = %+v, want %+v", digest.Tools, tc.wantTools)
			}
			for i := range tc.wantTools {
				if digest.Tools[i] != tc.wantTools[i] {
					t.Fatalf("tools[%d] = %+v, want %+v", i, digest.Tools[i], tc.wantTools[i])
				}
			}
		})
	}
}
