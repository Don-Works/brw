package pagewatch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/browsertest"
	"github.com/Don-Works/brw/internal/cdp"
)

func TestLiveStaticPageRefreshAndPrivateTabIsolation(t *testing.T) {
	chrome, err := cdp.FindChrome("")
	if err != nil {
		t.Skipf("Chrome unavailable: %v", err)
	}
	var value atomic.Int64
	var requests atomic.Int64
	var loggedOut atomic.Bool
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/inbox" {
			requests.Add(1)
			if loggedOut.Load() {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			fmt.Fprintf(w, `<!doctype html><title>Inbox</title><div id="messages">%d</div>`, value.Load())
			return
		}
		if r.URL.Path == "/login" {
			fmt.Fprint(w, `<!doctype html><title>Sign in</title><div id="messages">private login document</div>`)
			return
		}
		fmt.Fprint(w, `<!doctype html><title>Other</title><div>Another agent's page</div>`)
	}))
	defer site.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	profile := browsertest.NewProfile(t)
	m, err := browser.New(ctx, browser.Config{ChromePath: chrome, UserDataDir: profile.Dir(), Headless: true, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	profile.StopWith(func() { _ = m.Close() })
	working, err := m.Open(ctx, site.URL+"/working")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, m, filepath.Join(t.TempDir(), "watchers"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.cancel()
	<-s.done
	defer s.Close()
	m.SetTabAccessGuard(s.CheckTabAccess)
	w := registerFixture(t, s, RegisterOptions{ID: "live", URL: site.URL + "/inbox", Selector: "#messages", RefreshIntervalMS: 5000})
	sampleFixture(s, w.ID)
	r := s.records[w.ID]
	if r.Status != "watching" {
		t.Fatalf("baseline sample: %+v", r)
	}
	active, err := m.ActiveTabID(ctx)
	if err != nil || active != working.Tab.ID {
		t.Fatalf("watcher moved default tab: %s %v", active, err)
	}
	if _, err := m.Evaluate(browser.WithTabID(ctx, r.TabID), "document.title"); err == nil {
		t.Fatal("ordinary browser call read private watcher tab")
	}
	value.Store(1)
	sampleFixture(s, w.ID)
	if out := eventsFixture(t, s, w.ID, 0); out.LatestSeq != 0 {
		t.Fatalf("static document changed before refresh: %+v", out)
	}
	r = s.records[w.ID]
	r.NextRefresh = time.Now().Add(-time.Second)
	s.records[w.ID] = r
	sampleFixture(s, w.ID)
	if out := eventsFixture(t, s, w.ID, 0); out.LatestSeq != 1 || len(out.Events) != 1 || out.Events[0].Kind != "changed" {
		t.Fatalf("refresh did not observe changed document: %+v record=%+v", out, s.records[w.ID])
	}
	r = s.records[w.ID]
	r.NextRefresh = time.Now().Add(-time.Second)
	s.records[w.ID] = r
	sampleFixture(s, w.ID)
	if out := eventsFixture(t, s, w.ID, 0); out.LatestSeq != 1 || requests.Load() != 3 {
		t.Fatalf("unchanged refresh emitted signal or failed to load: %+v requests=%d", out, requests.Load())
	}
	if active, err := m.ActiveTabID(ctx); err != nil || active != working.Tab.ID {
		t.Fatalf("refresh moved default tab: %s %v", active, err)
	}
	loggedOut.Store(true)
	r = s.records[w.ID]
	r.NextRefresh = time.Now().Add(-time.Second)
	s.records[w.ID] = r
	sampleFixture(s, w.ID)
	sampleFixture(s, w.ID)
	if out := eventsFixture(t, s, w.ID, 1); len(out.Events) != 1 || out.Events[0].Kind != "unavailable" || out.Events[0].Reason != "url_mismatch" {
		t.Fatalf("real login redirect did not produce one unavailable signal: %+v", out)
	}
	state, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state), "private login document") {
		t.Fatal("login document content entered persistent state")
	}
	loggedOut.Store(false)
	if _, err := s.PageWatchers(ctx, ManageOptions{Action: "resume", ID: w.ID}); err != nil {
		t.Fatal(err)
	}
	sampleFixture(s, w.ID)
	sampleFixture(s, w.ID)
	if out := eventsFixture(t, s, w.ID, 2); len(out.Events) != 1 || out.Events[0].Kind != "recovered" {
		t.Fatalf("real login recovery with unchanged content: %+v", out)
	}
	if _, err := s.PageWatchers(ctx, ManageOptions{Action: "remove", ID: w.ID}); err != nil {
		t.Fatal(err)
	}
}
