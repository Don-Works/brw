package pagewatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/navpolicy"
)

type fixtureController struct {
	browser.Controller
	url          string
	value        string
	source       string
	closed       bool
	disconnected bool
	opened       int
	reads        int
	refreshes    int
	lastTab      string
	openErr      error
	redirectURL  string
}

func (f *fixtureController) Open(ctx context.Context, rawURL string) (browser.OpenResult, error) {
	if !browser.IsBackgroundPage(ctx) {
		panic("watcher did not open a background tab")
	}
	f.opened++
	f.closed = false
	f.url = rawURL
	if f.redirectURL != "" {
		f.url = f.redirectURL
	}
	return browser.OpenResult{Tab: browser.Tab{ID: "private", URL: rawURL}, Ready: true}, f.openErr
}
func (f *fixtureController) ListTabs(context.Context) ([]browser.Tab, error) {
	if f.disconnected {
		return nil, errors.New("connection lost with private page contents")
	}
	tabs := []browser.Tab{{ID: "somebody-else", URL: "https://other.test/"}}
	if !f.closed {
		tabs = append(tabs, browser.Tab{ID: "private", URL: f.url})
	}
	return tabs, nil
}
func (f *fixtureController) Evaluate(ctx context.Context, _ string) (any, error) {
	f.reads++
	f.lastTab = browser.TabIDFromContext(ctx)
	if f.lastTab != "private" || !browser.IsBackgroundPage(ctx) {
		panic("watcher read a foreign tab")
	}
	return map[string]any{"value": f.value, "count": 1}, nil
}
func (f *fixtureController) CloseTab(ctx context.Context, id string) error {
	if id != "private" || !browser.IsBackgroundPage(ctx) {
		panic("watcher closed a foreign tab")
	}
	f.closed = true
	return nil
}
func (f *fixtureController) NavigateTo(ctx context.Context, rawURL string) (browser.ActionResult, error) {
	if browser.TabIDFromContext(ctx) != "private" || !browser.IsBackgroundPage(ctx) {
		panic("watcher refreshed a foreign tab")
	}
	f.refreshes++
	f.url = rawURL
	f.value = f.source
	return browser.ActionResult{OK: true}, nil
}

func (f *fixtureController) ReloadPage(ctx context.Context) error {
	_, err := f.NavigateTo(ctx, f.url)
	return err
}
func (f *fixtureController) WaitFor(context.Context, string, time.Duration) error { return nil }

func serviceFixture(t *testing.T, root string, f *fixtureController) *Service {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), f, root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.cancel()
	<-s.done
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func registerFixture(t *testing.T, s *Service, opts RegisterOptions) Watcher {
	t.Helper()
	w, err := s.WatchPage(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func sampleFixture(s *Service, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sample(context.Background(), id)
}
func eventsFixture(t *testing.T, s *Service, id string, since uint64) EventsResult {
	t.Helper()
	out, err := s.PageEvents(context.Background(), EventsOptions{WatcherID: id, SinceSeq: since, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDurableBaselineChangesAndIdempotentRegistration(t *testing.T) {
	root := t.TempDir()
	f := &fixtureController{value: "private first message"}
	s := serviceFixture(t, root, f)
	opts := RegisterOptions{ID: "stable", URL: "https://chat.test/inbox#room", Selector: "#messages", IntervalMS: 1000}
	w := registerFixture(t, s, opts)
	if w.Mode != "text" || !w.Enabled {
		t.Fatalf("watcher: %+v", w)
	}
	if retry := registerFixture(t, s, opts); retry.ID != w.ID {
		t.Fatalf("retry changed id: %+v", retry)
	}
	opts.Selector = "#other"
	if _, err := s.WatchPage(context.Background(), opts); err == nil {
		t.Fatal("mismatching retry accepted")
	}
	sampleFixture(s, w.ID)
	sampleFixture(s, w.ID)
	if out := eventsFixture(t, s, w.ID, 0); len(out.Events) != 0 || out.LatestSeq != 0 {
		t.Fatalf("baseline emitted event: %+v", out)
	}
	f.value = "private next message"
	sampleFixture(s, w.ID)
	sampleFixture(s, w.ID)
	out := eventsFixture(t, s, w.ID, 0)
	if len(out.Events) != 1 || out.Events[0].Kind != "changed" || out.Events[0].Seq != 1 {
		t.Fatalf("changes: %+v", out)
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private first message") || strings.Contains(string(data), "private next message") {
		t.Fatal("page text persisted")
	}
	info, _ := os.Stat(s.path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v", info.Mode())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored := serviceFixture(t, root, f)
	sampleFixture(restored, w.ID)
	if out := eventsFixture(t, restored, w.ID, 0); out.LatestSeq != 1 || len(out.Events) != 1 {
		t.Fatalf("restart lost baseline/queue: %+v", out)
	}
	f.value = "third"
	sampleFixture(restored, w.ID)
	if out := eventsFixture(t, restored, w.ID, 1); len(out.Events) != 1 || out.Events[0].Seq != 2 {
		t.Fatalf("restart cursor: %+v", out)
	}
}

func TestUnavailableRecoveryDoesNotReadRedirectOrDuplicateAfterRestart(t *testing.T) {
	root := t.TempDir()
	f := &fixtureController{value: "same"}
	s := serviceFixture(t, root, f)
	w := registerFixture(t, s, RegisterOptions{ID: "login", URL: "https://chat.test/inbox"})
	sampleFixture(s, w.ID)
	reads := f.reads
	f.url = "https://accounts.test/login?secret=never-record"
	f.redirectURL = f.url
	sampleFixture(s, w.ID)
	sampleFixture(s, w.ID)
	if f.reads != reads {
		t.Fatal("read redirect page")
	}
	out := eventsFixture(t, s, w.ID, 0)
	if len(out.Events) != 1 || out.Events[0].Kind != "unavailable" || out.Events[0].Reason != "url_mismatch" {
		t.Fatalf("unavailable: %+v", out)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = serviceFixture(t, root, f)
	sampleFixture(s, w.ID)
	if out := eventsFixture(t, s, w.ID, 0); out.LatestSeq != 1 {
		t.Fatalf("restart duplicated unavailable: %+v", out)
	}
	f.url = w.URL
	f.redirectURL = ""
	sampleFixture(s, w.ID)
	sampleFixture(s, w.ID)
	out = eventsFixture(t, s, w.ID, 1)
	if len(out.Events) != 1 || out.Events[0].Kind != "recovered" {
		t.Fatalf("recovery with unchanged content: %+v", out)
	}
	f.disconnected = true
	sampleFixture(s, w.ID)
	sampleFixture(s, w.ID)
	out = eventsFixture(t, s, w.ID, 2)
	if len(out.Events) != 1 || out.Events[0].Reason != "disconnected" {
		t.Fatalf("disconnect: %+v", out)
	}
	data, _ := os.ReadFile(s.path)
	if strings.Contains(string(data), "secret=never-record") || strings.Contains(string(data), "private page contents") {
		t.Fatal("private error data persisted")
	}
	f.disconnected = false
}

func TestPauseRemoveClosedTabAndOwnership(t *testing.T) {
	f := &fixtureController{value: "same"}
	s := serviceFixture(t, t.TempDir(), f)
	w := registerFixture(t, s, RegisterOptions{ID: "lifecycle", URL: "https://chat.test/"})
	sampleFixture(s, w.ID)
	if !s.OwnsTab("private") || s.CheckTabAccess(context.Background(), "private") == nil {
		t.Fatal("watcher tab is not reserved")
	}
	if err := s.CheckTabAccess(browser.WithBackgroundPage(context.Background()), "private"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PageWatchers(context.Background(), ManageOptions{Action: "pause", ID: w.ID}); err != nil {
		t.Fatal(err)
	}
	if s.records[w.ID].Enabled {
		t.Fatal("pause did not disable sampling")
	}
	f.closed = true
	if _, err := s.PageWatchers(context.Background(), ManageOptions{Action: "resume", ID: w.ID}); err != nil {
		t.Fatal(err)
	}
	sampleFixture(s, w.ID)
	if s.records[w.ID].Status != "tab_closed" {
		t.Fatalf("closed status: %+v", s.records[w.ID])
	}
	if _, err := s.PageWatchers(context.Background(), ManageOptions{Action: "resume", ID: w.ID}); err != nil {
		t.Fatal(err)
	}
	sampleFixture(s, w.ID)
	if f.opened != 2 {
		t.Fatalf("resume did not recreate owned tab: %d", f.opened)
	}
	out, err := s.PageWatchers(context.Background(), ManageOptions{Action: "remove", ID: w.ID})
	if err != nil || !out.Removed || len(out.Watchers) != 0 || !f.closed || s.OwnsTab("private") {
		t.Fatalf("remove: %+v %v", out, err)
	}
	if _, err := s.PageWatchers(context.Background(), ManageOptions{Action: "remove", ID: w.ID}); err != nil {
		t.Fatalf("remove retry: %v", err)
	}
}

func TestFailedSaveRetainsBaselineAndUnpublishedSequence(t *testing.T) {
	f := &fixtureController{value: "old"}
	s := serviceFixture(t, t.TempDir(), f)
	w := registerFixture(t, s, RegisterOptions{ID: "storage", URL: "https://chat.test/"})
	sampleFixture(s, w.ID)
	path := s.path
	s.path = filepath.Join(filepath.Dir(path), "missing", "state.json")
	f.value = "new"
	sampleFixture(s, w.ID)
	if out := eventsFixture(t, s, w.ID, 0); out.LatestSeq != 0 || len(out.Events) != 0 {
		t.Fatalf("unpersisted event published: %+v", out)
	}
	s.path = path
	sampleFixture(s, w.ID)
	if out := eventsFixture(t, s, w.ID, 0); len(out.Events) != 1 || out.Events[0].Kind != "changed" {
		t.Fatalf("change lost after failed save: %+v", out)
	}
}

func TestPartialOpenFailureRetainsOwnershipWithoutOpeningMoreTabs(t *testing.T) {
	f := &fixtureController{value: "ready", openErr: errors.New("navigation stalled")}
	s := serviceFixture(t, t.TempDir(), f)
	w := registerFixture(t, s, RegisterOptions{ID: "partial", URL: "https://chat.test/"})
	sampleFixture(s, w.ID)
	if f.opened != 1 || !s.OwnsTab("private") || s.records[w.ID].TabID != "private" {
		t.Fatal("partial tab was not retained and reserved")
	}
	sampleFixture(s, w.ID)
	if f.opened != 1 {
		t.Fatal("partial open error leaked another tab")
	}
	if _, err := s.PageWatchers(context.Background(), ManageOptions{Action: "remove", ID: w.ID}); err != nil {
		t.Fatal(err)
	}
	if !f.closed {
		t.Fatal("partial tab was not closed on removal")
	}
}

type recycledController struct {
	browser.Controller
	humanURL string
	freshURL string
	reads    int
	closed   []string
}

func (c *recycledController) Open(_ context.Context, rawURL string) (browser.OpenResult, error) {
	c.freshURL = rawURL
	return browser.OpenResult{Tab: browser.Tab{ID: "fresh", URL: rawURL}}, nil
}
func (c *recycledController) ListTabs(context.Context) ([]browser.Tab, error) {
	tabs := []browser.Tab{{ID: "private", URL: c.humanURL}}
	if c.freshURL != "" {
		tabs = append(tabs, browser.Tab{ID: "fresh", URL: c.freshURL})
	}
	return tabs, nil
}
func (c *recycledController) Evaluate(ctx context.Context, _ string) (any, error) {
	if browser.TabIDFromContext(ctx) != "fresh" {
		panic("read recycled human tab")
	}
	c.reads++
	return map[string]any{"value": "same", "count": 1}, nil
}
func (c *recycledController) CloseTab(_ context.Context, id string) error {
	c.closed = append(c.closed, id)
	return nil
}

func TestRestartNeverReadsOrClosesRecycledTabIDs(t *testing.T) {
	for _, humanURL := range []string{"https://other.test/human", "https://chat.test/"} {
		t.Run(humanURL, func(t *testing.T) {
			root := t.TempDir()
			old := serviceFixture(t, root, &fixtureController{value: "same"})
			w := registerFixture(t, old, RegisterOptions{ID: "restart", URL: "https://chat.test/"})
			sampleFixture(old, w.ID)
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			c := &recycledController{humanURL: humanURL}
			s, err := New(context.Background(), c, root, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			s.cancel()
			<-s.done
			defer s.Close()
			if s.OwnsTab("private") || s.records[w.ID].TabID != "" {
				t.Fatal("restart adopted an unproven tab id")
			}
			sampleFixture(s, w.ID)
			if c.reads != 1 || s.records[w.ID].TabID != "fresh" {
				t.Fatal("restart failed to acquire a fresh private tab")
			}
			if out := eventsFixture(t, s, w.ID, 0); out.LatestSeq != 0 {
				t.Fatalf("new tab lost baseline: %+v", out)
			}
			if _, err := s.PageWatchers(context.Background(), ManageOptions{Action: "remove", ID: w.ID}); err != nil {
				t.Fatal(err)
			}
			if len(c.closed) != 1 || c.closed[0] != "fresh" {
				t.Fatalf("remove closed unproven tab: %v", c.closed)
			}
		})
	}
}

type epochController struct {
	fixtureController
	lost bool
}

func (c *epochController) CheckBackgroundTab(context.Context, string) error {
	if c.lost {
		return browser.ErrBackgroundOwnershipLost
	}
	return nil
}
func (c *epochController) ReleaseBackgroundTab(string) {}
func (c *epochController) Open(ctx context.Context, rawURL string) (browser.OpenResult, error) {
	c.lost = false
	return c.fixtureController.Open(ctx, rawURL)
}

func TestConnectionEpochLossReacquiresMissingTabAndNeverClosesRecycledID(t *testing.T) {
	c := &epochController{fixtureController: fixtureController{value: "same"}}
	s, err := New(context.Background(), c, filepath.Join(t.TempDir(), "watchers"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.cancel()
	<-s.done
	defer s.Close()
	w := registerFixture(t, s, RegisterOptions{ID: "epoch", URL: "https://chat.test/"})
	sampleFixture(s, w.ID)
	c.lost = true
	c.closed = true
	sampleFixture(s, w.ID)
	if s.records[w.ID].TabID != "" || s.OwnsTab("private") {
		t.Fatal("stale connection claim survived")
	}
	if out := eventsFixture(t, s, w.ID, 0); len(out.Events) != 1 || out.Events[0].Reason != "ownership_lost" {
		t.Fatalf("lost-connection event: %+v", out)
	}
	sampleFixture(s, w.ID)
	if c.opened != 2 || s.records[w.ID].Status != "watching" {
		t.Fatal("missing old id after reconnect was not reacquired")
	}
	if out := eventsFixture(t, s, w.ID, 1); len(out.Events) != 1 || out.Events[0].Kind != "recovered" {
		t.Fatalf("reacquisition: %+v", out)
	}
	if _, err := s.PageWatchers(context.Background(), ManageOptions{Action: "pause", ID: w.ID}); err != nil {
		t.Fatal(err)
	}
	c.lost = true
	c.closed = false
	c.url = w.URL
	if _, err := s.PageWatchers(context.Background(), ManageOptions{Action: "remove", ID: w.ID}); err != nil {
		t.Fatal(err)
	}
	if c.closed {
		t.Fatal("remove closed a recycled human tab after connection loss")
	}
}

func TestStaticRefreshKeepsBaselineAndChangesOnlyAfterReload(t *testing.T) {
	f := &fixtureController{value: "old", source: "old"}
	s := serviceFixture(t, t.TempDir(), f)
	w := registerFixture(t, s, RegisterOptions{ID: "static", URL: "https://static.test/", Selector: "#messages", RefreshIntervalMS: 5000})
	sampleFixture(s, w.ID)
	f.source = "new"
	sampleFixture(s, w.ID)
	if out := eventsFixture(t, s, w.ID, 0); out.LatestSeq != 0 {
		t.Fatalf("unsampled server state changed: %+v", out)
	}
	s.mu.Lock()
	r := s.records[w.ID]
	r.NextRefresh = time.Now().Add(-time.Second)
	s.records[w.ID] = r
	s.mu.Unlock()
	sampleFixture(s, w.ID)
	if f.refreshes != 1 || eventsFixture(t, s, w.ID, 0).LatestSeq != 1 {
		t.Fatal("refresh did not observe changed server state")
	}
	s.mu.Lock()
	r = s.records[w.ID]
	r.NextRefresh = time.Now().Add(-time.Second)
	s.records[w.ID] = r
	s.mu.Unlock()
	sampleFixture(s, w.ID)
	if f.refreshes != 2 || eventsFixture(t, s, w.ID, 0).LatestSeq != 1 {
		t.Fatal("unchanged refresh emitted change")
	}
}

func TestValidationRetentionAndPolicy(t *testing.T) {
	for _, opts := range []RegisterOptions{{URL: "javascript:alert(1)"}, {URL: "file:///private"}, {URL: "https://user:pass@chat.test"}, {URL: "https://chat.test", Mode: "text"}, {URL: "https://chat.test", IntervalMS: 999}, {URL: "https://chat.test", RefreshIntervalMS: 4999}} {
		if _, err := normalize(opts); err == nil {
			t.Errorf("accepted invalid options: %+v", opts)
		}
	}
	f := &fixtureController{}
	s := serviceFixture(t, t.TempDir(), f)
	s.policy = navpolicy.Parse("allowed.test", "")
	if _, err := s.WatchPage(context.Background(), RegisterOptions{URL: "https://denied.test"}); err == nil {
		t.Fatal("policy ignored")
	}
	s.policy = nil
	w := registerFixture(t, s, RegisterOptions{ID: "ring", URL: "https://allowed.test/"})
	s.mu.Lock()
	r := s.records[w.ID]
	for range maxEvents + 3 {
		appendEvent(&r, Event{Kind: "changed", Digest: "metadata"})
	}
	if err := s.store(w.ID, &r); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	out := eventsFixture(t, s, w.ID, 0)
	if !out.Gap || !out.HasMore || out.OldestSeq != 4 || len(out.Events) != 100 {
		t.Fatalf("retention: %+v", out)
	}
	var encoded map[string]any
	raw, _ := json.Marshal(out)
	if err := json.Unmarshal(raw, &encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PageEvents(context.Background(), EventsOptions{WatcherID: w.ID, Limit: 101}); err == nil {
		t.Fatal("unbounded limit accepted")
	}
}
