package browser

import (
	"context"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/store"
)

// ActiveTabID is what lets a WebMCP page-tool report name a tab on direct CDP,
// where nothing pins one into the request context. It has to answer from what is
// already known: ensureActive, the resolution it shadows, opens about:blank when
// there is nothing to report, and a caller asking which tab to NAME must not get
// a new one made for it.
//
// This Manager has no browser context at all, so any round trip would panic
// rather than pass — which is the assertion that the answer stayed local.
func TestManagerNamesTheActiveTabWithoutReachingTheBrowser(t *testing.T) {
	m := &Manager{refs: store.New(), timeout: time.Second}
	m.refs.SetActive("tab-3")

	got, err := m.ActiveTabID(context.Background())
	if err != nil {
		t.Fatalf("active tab: %v", err)
	}
	if got != "tab-3" {
		t.Fatalf("active tab = %q, want tab-3", got)
	}
}
