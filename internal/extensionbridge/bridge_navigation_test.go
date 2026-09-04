package extensionbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/coder/websocket"
)

type navigationFakeExtension struct {
	mu sync.Mutex

	documentID string
	epoch      uint64
	worker     string
	origin     string
	frameID    string
	loaderID   string
	url        string
	// reportedFrameURL models Page.getFrameTree retaining a differently
	// serialized SPA URL while Runtime.evaluate(location.href) is current.
	reportedFrameURL string

	targetDocumentID string
	targetOrigin     string
	targetLoaderID   string
	targetURL        string
	delay            time.Duration
	sameDocument     bool
	replaced         bool
	earlyEvaluate    bool
	navigateCalls    int

	unrelatedDelay      time.Duration
	unrelatedDocumentID string
	unrelatedOrigin     string
	unrelatedLoaderID   string
	unrelatedURL        string
	replaceDuringReady  bool
	readyRaceDone       bool
	suppressNavCommit   bool
	spuriousLoaderRoute bool
}

func (f *navigationFakeExtension) commitTarget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.url = f.targetURL
	if !f.sameDocument {
		f.documentID = f.targetDocumentID
		f.loaderID = f.targetLoaderID
		f.origin = f.targetOrigin
		f.epoch++
	}
	f.replaced = true
}

func (f *navigationFakeExtension) commitUnrelated() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.documentID = f.unrelatedDocumentID
	f.loaderID = f.unrelatedLoaderID
	f.origin = f.unrelatedOrigin
	f.url = f.unrelatedURL
	f.epoch++
	f.replaced = true
}

func (f *navigationFakeExtension) serve(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg request
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		var result any = map[string]any{}
		ok := true
		errText := ""
		f.mu.Lock()
		switch msg.Type {
		case "get_document_identity":
			result = map[string]any{
				"document_id": f.documentID, "document_epoch": f.epoch,
				"worker_instance": f.worker, "origin": f.origin, "tab_id": 42,
			}
		case "cdp":
			method, _ := msg.Params["method"].(string)
			switch method {
			case "Page.getFrameTree":
				frameURL := f.url
				if f.reportedFrameURL != "" {
					frameURL = f.reportedFrameURL
				}
				result = map[string]any{"frameTree": map[string]any{"frame": map[string]any{
					"id": f.frameID, "loaderId": f.loaderID, "url": frameURL,
				}}}
			case "Page.navigate":
				f.navigateCalls++
				loaderID := f.targetLoaderID
				if f.sameDocument {
					loaderID = ""
				}
				result = map[string]any{"frameId": f.frameID}
				if loaderID != "" {
					result.(map[string]any)["loaderId"] = loaderID
				}
				if f.spuriousLoaderRoute {
					// Model an SPA which accepts the route in-place while Chrome's
					// Page.navigate response nevertheless advertises a loader that never
					// becomes the main frame's loader.
					f.url = f.targetURL
					f.replaced = true
				} else if !f.suppressNavCommit {
					delay := f.delay
					f.mu.Unlock()
					if delay == 0 {
						f.commitTarget()
					} else {
						time.AfterFunc(delay, f.commitTarget)
					}
					if f.unrelatedDelay > 0 {
						time.AfterFunc(f.unrelatedDelay, f.commitUnrelated)
					}
					f.mu.Lock()
				}
			case "Runtime.evaluate":
				expression, _ := msg.Params["params"].(map[string]any)["expression"].(string)
				value := any(true)
				locationRead := expression == "location.href" || expression == "globalThis.location.href"
				if locationRead {
					value = f.url
				}
				if !f.replaced && !locationRead {
					f.earlyEvaluate = true
				}
				if f.replaceDuringReady && !f.readyRaceDone && strings.Contains(expression, "MutationObserver") {
					f.readyRaceDone = true
					f.documentID = "doc-after-ready-race"
					f.loaderID = "loader-after-ready-race"
					f.origin = "https://race.test"
					f.url = "https://race.test/after"
					f.epoch++
				}
				result = map[string]any{"result": map[string]any{"value": value}}
			}
		case "cached_snapshot":
			result = map[string]any{"cached": true, "snapshot": f.snapshotLocked()}
		case "snapshot_result":
			result = map[string]any{"stored": true}
		default:
			ok = false
			errText = "unknown message type " + msg.Type
		}
		f.mu.Unlock()
		reply, _ := json.Marshal(response{ID: msg.ID, OK: ok, Result: mustNavigationJSON(result), Error: errText})
		_ = conn.Write(ctx, websocket.MessageText, reply)
	}
}

func (f *navigationFakeExtension) snapshotLocked() map[string]any {
	return map[string]any{
		"url": f.url, "title": "replacement", "elements": []any{},
		"metadata": map[string]any{"version": 1},
	}
}

func mustNavigationJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func newNavigationFake(t *testing.T, finalURL, finalOrigin string, delay time.Duration, sameDocument bool) (*Bridge, *navigationFakeExtension, func()) {
	t.Helper()
	b := New("", 4*time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	conn, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		srv.Close()
		t.Fatalf("dial navigation fake: %v", err)
	}
	waitUntil(t, b.liveConn)
	fake := &navigationFakeExtension{
		documentID: "doc-source", worker: "worker-a", origin: "https://source.test",
		frameID: "frame-42", loaderID: "loader-source", url: "https://source.test/start",
		targetDocumentID: "doc-target", targetOrigin: finalOrigin,
		targetLoaderID: "loader-target", targetURL: finalURL,
		delay: delay, sameDocument: sameDocument,
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	go fake.serve(serveCtx, conn)
	return b, fake, func() {
		cancel()
		_ = conn.CloseNow()
		srv.Close()
	}
}

func TestBatchAndPlanNavigateWaitForReplacementBeforeNextStep(t *testing.T) {
	for _, runner := range []string{"standalone", "batch", "plan"} {
		t.Run(runner, func(t *testing.T) {
			const target = "data:text/html,<main>replacement</main>"
			b, fake, cleanup := newNavigationFake(t, target, "null", 80*time.Millisecond, false)
			defer cleanup()
			ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 3*time.Second)
			defer cancel()
			started := time.Now()
			if runner == "standalone" {
				res, err := b.NavigateTo(ctx, target)
				if err != nil || !res.OK || res.URL != target {
					t.Fatalf("standalone navigate: result=%+v err=%v", res, err)
				}
			} else if runner == "batch" {
				res, err := b.ExecuteBatch(ctx, []browser.BatchStep{
					{Action: "navigate_to", URL: target},
					{Action: "wait", Condition: "text:replacement", TimeoutMS: 500},
				})
				if err != nil || !res.OK {
					t.Fatalf("batch navigate: result=%+v err=%v", res, err)
				}
			} else {
				res, err := b.ExecutePlan(ctx, []browser.PlanStep{
					{Action: "navigate_to", URL: target},
					{Action: "wait", Condition: "text:replacement", TimeoutMS: 500},
				})
				if err != nil || !res.OK {
					t.Fatalf("plan navigate: result=%+v err=%v", res, err)
				}
			}
			fake.mu.Lock()
			early, replaced, calls := fake.earlyEvaluate, fake.replaced, fake.navigateCalls
			fake.mu.Unlock()
			if early || !replaced || calls != 1 {
				t.Fatalf("navigation boundary early_evaluate=%t replaced=%t navigate_calls=%d", early, replaced, calls)
			}
			if elapsed := time.Since(started); elapsed < 80*time.Millisecond {
				t.Fatalf("runner returned before delayed replacement committed: %v", elapsed)
			}
		})
	}
}

func TestNavigateCompletionAllowsRedirectAndRejectsDisallowedFinalURL(t *testing.T) {
	t.Run("allowed redirect", func(t *testing.T) {
		b, _, cleanup := newNavigationFake(t, "https://final.test/landing", "https://final.test", 10*time.Millisecond, false)
		defer cleanup()
		b.SetNavigationPolicy(navpolicy.Parse("start.test,final.test", ""))
		ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 2*time.Second)
		defer cancel()
		if err := b.navigateToURLAndWait(ctx, "https://start.test/go"); err != nil {
			t.Fatalf("allowed redirect: %v", err)
		}
	})

	t.Run("disallowed redirect", func(t *testing.T) {
		b, _, cleanup := newNavigationFake(t, "https://blocked.test/landing", "https://blocked.test", 10*time.Millisecond, false)
		defer cleanup()
		b.SetNavigationPolicy(navpolicy.Parse("start.test", ""))
		ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 2*time.Second)
		defer cancel()
		err := b.navigateToURLAndWait(ctx, "https://start.test/go")
		if err == nil || !strings.Contains(err.Error(), "final browser destination rejected") {
			t.Fatalf("disallowed redirect error = %v", err)
		}
	})
}

func TestNavigateCompletionCannotBeSatisfiedByUnrelatedAllowedReplacement(t *testing.T) {
	b, fake, cleanup := newNavigationFake(t, "https://final.test/landing", "https://final.test", 90*time.Millisecond, false)
	defer cleanup()
	fake.mu.Lock()
	fake.unrelatedDelay = 10 * time.Millisecond
	fake.unrelatedDocumentID = "doc-unrelated"
	fake.unrelatedLoaderID = "loader-unrelated"
	fake.unrelatedOrigin = "https://unrelated.test"
	fake.unrelatedURL = "https://unrelated.test/page"
	fake.mu.Unlock()
	b.SetNavigationPolicy(navpolicy.Parse("start.test,final.test,unrelated.test", ""))
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 2*time.Second)
	defer cancel()
	started := time.Now()
	if err := b.navigateToURLAndWait(ctx, "https://start.test/go"); err != nil {
		t.Fatalf("target navigation after unrelated replacement: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 90*time.Millisecond {
		t.Fatalf("unrelated allowed replacement satisfied target-bound wait in %v", elapsed)
	}
}

func TestNavigateCompletionFailsIfDocumentChangesWhileWaitingForReady(t *testing.T) {
	b, fake, cleanup := newNavigationFake(t, "https://target.test/landing", "https://target.test", 10*time.Millisecond, false)
	defer cleanup()
	fake.mu.Lock()
	fake.replaceDuringReady = true
	fake.mu.Unlock()
	b.SetNavigationPolicy(navpolicy.Parse("target.test,race.test", ""))
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 2*time.Second)
	defer cancel()
	err := b.navigateToURLAndWait(ctx, "https://target.test/landing")
	if err == nil || !strings.Contains(err.Error(), "page changed again") {
		t.Fatalf("ready continuity error = %v", err)
	}
}

func TestNavigateCompletionAcceptsSameDocumentFragment(t *testing.T) {
	b, fake, cleanup := newNavigationFake(t, "https://source.test/start#details", "https://source.test", 0, true)
	defer cleanup()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 2*time.Second)
	defer cancel()
	if err := b.navigateToURLAndWait(ctx, "https://source.test/start#details"); err != nil {
		t.Fatalf("same-document fragment: %v", err)
	}
	fake.mu.Lock()
	doc, loader, gotURL, calls := fake.documentID, fake.loaderID, fake.url, fake.navigateCalls
	fake.mu.Unlock()
	if doc != "doc-source" || loader != "loader-source" || gotURL != "https://source.test/start#details" {
		t.Fatalf("same-document identity changed: doc=%q loader=%q url=%q", doc, loader, gotURL)
	}
	if calls != 1 {
		t.Fatalf("same-document fragment Page.navigate calls = %d, want 1", calls)
	}
}

func TestNavigateCompletionTreatsExactCurrentURLAsIdempotent(t *testing.T) {
	const currentURL = "https://source.test/start#search/in%3Ainbox+is%3Aunread"
	b, fake, cleanup := newNavigationFake(t, currentURL, "https://source.test", 0, false)
	defer cleanup()
	// Model the live failure: chrome.tabs/location.href report the requested SPA
	// URL, Page.getFrameTree retains another serialization, and Page.navigate
	// reports a target loader that it never commits. A frame-URL-only preflight
	// therefore waited until its context expired.
	fake.mu.Lock()
	fake.url = currentURL
	fake.reportedFrameURL = "https://source.test/start#search/in:inbox+is:unread"
	fake.suppressNavCommit = true
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 500*time.Millisecond)
	defer cancel()
	if err := b.navigateToURLAndWait(ctx, currentURL); err != nil {
		t.Fatalf("exact-current navigate_to: %v", err)
	}
	fake.mu.Lock()
	calls, doc, loader, gotURL := fake.navigateCalls, fake.documentID, fake.loaderID, fake.url
	fake.mu.Unlock()
	if calls != 0 {
		t.Fatalf("exact-current navigate_to issued Page.navigate %d time(s), want 0", calls)
	}
	if doc != "doc-source" || loader != "loader-source" || gotURL != currentURL {
		t.Fatalf("exact-current identity changed: doc=%q loader=%q url=%q", doc, loader, gotURL)
	}
}

func TestNavigateCompletionDoesNotTrustStaleFrameURLForNoop(t *testing.T) {
	const targetURL = "https://source.test/start#target"
	b, fake, cleanup := newNavigationFake(t, targetURL, "https://source.test", 0, false)
	defer cleanup()
	// Invert the discrepancy: only the frame tree claims the target while the
	// document is still at another URL. The bridge must issue Page.navigate.
	fake.mu.Lock()
	fake.url = "https://source.test/start#old"
	fake.reportedFrameURL = targetURL
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), time.Second)
	defer cancel()
	if err := b.navigateToURLAndWait(ctx, targetURL); err != nil {
		t.Fatalf("navigate from stale frame URL: %v", err)
	}
	fake.mu.Lock()
	calls, gotURL := fake.navigateCalls, fake.url
	fake.mu.Unlock()
	if calls != 1 || gotURL != targetURL {
		t.Fatalf("stale frame URL navigation: calls=%d url=%q, want calls=1 url=%q", calls, gotURL, targetURL)
	}
}

func TestNavigateCompletionAcceptsSameDocumentRouteWithSpuriousLoader(t *testing.T) {
	const (
		sourceURL = "https://source.test/start#search/in%3Ainbox+is%3Aunread"
		targetURL = "https://source.test/start#inbox"
	)
	b, fake, cleanup := newNavigationFake(t, targetURL, "https://source.test", 0, false)
	defer cleanup()
	fake.mu.Lock()
	fake.url = sourceURL
	// Keep the frame-tree URL stale as observed on the real SPA. Only the pinned
	// document's location.href exposes the completed same-document route.
	fake.reportedFrameURL = sourceURL
	fake.spuriousLoaderRoute = true
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), time.Second)
	defer cancel()
	if err := b.navigateToURLAndWait(ctx, targetURL); err != nil {
		t.Fatalf("same-document route with spurious loader: %v", err)
	}
	fake.mu.Lock()
	calls, doc, loader, gotURL := fake.navigateCalls, fake.documentID, fake.loaderID, fake.url
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("spurious-loader same-document Page.navigate calls = %d, want 1", calls)
	}
	if doc != "doc-source" || loader != "loader-source" || gotURL != targetURL {
		t.Fatalf("spurious-loader route crossed document boundary: doc=%q loader=%q url=%q", doc, loader, gotURL)
	}
}

func TestNavigateCompletionRejectsSpuriousLoaderAcrossDocumentURL(t *testing.T) {
	const targetURL = "https://source.test/other-path"
	b, fake, cleanup := newNavigationFake(t, targetURL, "https://source.test", 0, false)
	defer cleanup()
	fake.mu.Lock()
	fake.url = "https://source.test/start#old"
	fake.spuriousLoaderRoute = true
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 100*time.Millisecond)
	defer cancel()
	err := b.navigateToURLAndWait(ctx, targetURL)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for the requested navigation to commit") {
		t.Fatalf("cross-document spurious-loader error = %v", err)
	}
}

func TestIsExactFragmentTransition(t *testing.T) {
	tests := []struct {
		name          string
		before, after string
		want          bool
	}{
		{"fragment replacement", "https://x.test/a#old", "https://x.test/a#new", true},
		{"fragment addition", "https://x.test/a", "https://x.test/a#new", true},
		{"fragment removal", "https://x.test/a#old", "https://x.test/a", true},
		{"exact URL", "https://x.test/a#old", "https://x.test/a#old", false},
		{"path change", "https://x.test/a#old", "https://x.test/b#new", false},
		{"query change", "https://x.test/a?q=1#old", "https://x.test/a?q=2#new", false},
		{"origin change", "https://x.test/a#old", "https://y.test/a#new", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isExactFragmentTransition(tt.before, tt.after); got != tt.want {
				t.Fatalf("isExactFragmentTransition(%q, %q) = %t, want %t", tt.before, tt.after, got, tt.want)
			}
		})
	}
}

func TestNavigateCompletionTimeoutHasActionableError(t *testing.T) {
	b, fake, cleanup := newNavigationFake(t, "https://target.test/landing", "https://target.test", 0, false)
	defer cleanup()
	fake.mu.Lock()
	fake.suppressNavCommit = true
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 100*time.Millisecond)
	defer cancel()
	err := b.navigateToURLAndWait(ctx, "https://target.test/landing")
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for the requested navigation to commit") {
		t.Fatalf("non-committing navigation error = %v", err)
	}
}

func TestNavigateCompletionExactCurrentURLFailsOnReadyRace(t *testing.T) {
	const currentURL = "https://source.test/start"
	b, fake, cleanup := newNavigationFake(t, currentURL, "https://source.test", 0, false)
	defer cleanup()
	fake.mu.Lock()
	fake.suppressNavCommit = true
	fake.replaceDuringReady = true
	fake.mu.Unlock()
	b.SetNavigationPolicy(navpolicy.Parse("source.test,race.test", ""))
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), time.Second)
	defer cancel()
	err := b.navigateToURLAndWait(ctx, currentURL)
	if err == nil || !strings.Contains(err.Error(), "page changed again") {
		t.Fatalf("exact-current ready-race error = %v", err)
	}
	fake.mu.Lock()
	calls := fake.navigateCalls
	fake.mu.Unlock()
	if calls != 0 {
		t.Fatalf("exact-current ready-race issued Page.navigate %d time(s), want 0", calls)
	}
}
