package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	brwcdp "github.com/Don-Works/brw/internal/cdp"
	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	cdpio "github.com/chromedp/cdproto/io"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Two properties of Chrome's Fetch domain that brw's design rests on, measured
// against a real browser rather than asserted in prose.
//
// Both decided a feature brw does NOT have, which is exactly the kind of claim
// that rots: nothing else in the suite exercises either path, so a Chrome that
// changed its mind would leave docs/install.md and docs/recipes-and-artifacts.md
// describing a browser that no longer exists. These tests fail when that
// happens, which is the signal to revisit the decision rather than the prose.

// interceptionFactsBrowser is a bare browser context. The Manager is not used:
// these tests drive the Fetch domain directly because what is being measured is
// Chrome's behaviour, not brw's handling of it.
func interceptionFactsBrowser(t *testing.T) (context.Context, func()) {
	t.Helper()
	chromePath, err := brwcdp.FindChrome("")
	if err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	// Not t.TempDir for the profile: its cleanup FAILS the test if the directory
	// is not empty, and Chrome's cache writers outlive the browser process this
	// helper cancels. These tests intercept at the RESPONSE stage, so they leave
	// a populated HTTP cache behind where the rest of the suite does not, and
	// that race showed up as a flaky failure with nothing to do with the result.
	profileDir, err := os.MkdirTemp("", "brw-interception-facts-")
	if err != nil {
		t.Fatalf("create a browser profile directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profileDir) })
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("disable-gpu", true),
		chromedp.UserDataDir(profileDir),
		chromedp.WSURLReadTimeout(45*time.Second),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		t.Skipf("headless Chrome did not start: %v", err)
	}
	return browserCtx, func() { browserCancel(); allocCancel() }
}

// A URL rewritten with Fetch.continueRequest is NOT offered to the interception
// again, while the server's own 30x hop is.
//
// This is why brw_route has no redirect behaviour on direct CDP. Every other
// route answers the request brw was already shown; a rewritten URL is a
// destination the containment listener never sees, so the one behaviour that
// would widen what a page can reach would also be the one the boundary could not
// gate. The 30x half is measured alongside it because it is the reason that gap
// is easy to miss: redirects DO come back, just not this one.
func TestRewrittenRequestURLIsNotPausedAgain(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	count := func(name string) {
		mu.Lock()
		hits[name]++
		mu.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/original", func(w http.ResponseWriter, _ *http.Request) {
		count("original")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprint(w, "ORIGINAL")
	})
	mux.HandleFunc("/rewritten", func(w http.ResponseWriter, r *http.Request) {
		count("rewritten")
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, _ *http.Request) {
		count("final")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprint(w, "FINAL")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><script>
		  window.__r = 'pending';
		  fetch('/original').then(function(r){return r.text();})
		    .then(function(t){ window.__r = t; })
		    .catch(function(){ window.__r = 'ERR'; });
		</script></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, done := interceptionFactsBrowser(t)
	defer done()

	var paused []string
	rewritten := 0
	chromedp.ListenTarget(ctx, func(ev any) {
		event, ok := ev.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		mu.Lock()
		paused = append(paused, event.Request.URL)
		// Bounded so a Chrome that DID re-pause the rewritten URL loops once and
		// is reported, rather than rewriting forever and timing the test out with
		// nothing to read.
		rewrite := strings.HasSuffix(event.Request.URL, "/original") && rewritten < 3
		if rewrite {
			rewritten++
		}
		mu.Unlock()
		go func() {
			answerCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			_ = chromedp.Run(answerCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
				if rewrite {
					return fetch.ContinueRequest(event.RequestID).WithURL(srv.URL + "/rewritten").Do(runCtx)
				}
				return fetch.ContinueRequest(event.RequestID).Do(runCtx)
			}))
		}()
	})
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		return fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*"}}).Do(c)
	})); err != nil {
		t.Fatalf("enable interception: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, 40*time.Second)
	defer runCancel()
	if err := chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
		_, _, _, _, err := page.Navigate(srv.URL).Do(c)
		return err
	})); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	var result string
	for i := 0; i < 100; i++ {
		_ = chromedp.Run(runCtx, chromedp.Evaluate(`window.__r`, &result))
		if result != "" && result != "pending" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if result != "FINAL" {
		t.Fatalf("page received %q, want FINAL: the rewrite did not take effect, so nothing below is measuring what it claims", result)
	}

	mu.Lock()
	defer mu.Unlock()
	sawRewritten, sawFinal := false, false
	for _, url := range paused {
		if strings.HasSuffix(url, "/rewritten") {
			sawRewritten = true
		}
		if strings.HasSuffix(url, "/final") {
			sawFinal = true
		}
	}
	if sawRewritten {
		t.Errorf("Chrome DID pause the rewritten URL (%v); a redirect behaviour could now be gated by the existing containment listener, so revisit ErrRouteRedirectUnsupported", paused)
	}
	if !sawFinal {
		t.Errorf("Chrome did not pause the server's own 30x hop (%v); containment relies on seeing it", paused)
	}
	// The server is the other half of the same fact: the rewritten URL was
	// fetched for real, it just never came back through the interception.
	if hits["rewritten"] != 1 {
		t.Errorf("rewritten destination was fetched %d times, want 1", hits["rewritten"])
	}
	if hits["original"] != 0 {
		t.Errorf("original destination was fetched %d times, want 0", hits["original"])
	}
}

// Taking a download's body as a Fetch stream replaces Chrome's own download
// rather than duplicating it: the browser writes no file, and the download
// manager is left with either no record of the transfer at all or a cancelled
// one of zero bytes.
//
// That is the answer to "would Fetch.takeResponseBodyAsStream let brw put a
// download straight into the artifact store?" — it would, and the download would
// stop existing: no guid to select by, no suggested filename, nothing for
// brw_wait_for{condition:"download"} to resolve on. brw keeps the staged-file
// path instead, and docs/recipes-and-artifacts.md says so.
func TestFetchResponseStageStreamsADownloadInsteadOfTheBrowserTakingIt(t *testing.T) {
	const chunks = 12
	const chunkSize = 128 << 10
	for _, tc := range []struct {
		name string
		// clickToDownload picks the trigger: a user click on an <a download>
		// rather than a navigation straight at the attachment. Both are the
		// shapes a real download starts in, and both have to be measured or the
		// answer only covers half of them.
		clickToDownload bool
	}{
		{name: "navigation turned into a download by content-disposition"},
		{name: "anchor with the download attribute", clickToDownload: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var writeDone time.Time
			mux := http.NewServeMux()
			mux.HandleFunc("/payload.bin", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Disposition", `attachment; filename="payload.bin"`)
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", fmt.Sprint(chunks*chunkSize))
				w.WriteHeader(http.StatusOK)
				block := make([]byte, chunkSize)
				for i := 0; i < chunks; i++ {
					if _, err := w.Write(block); err != nil {
						return
					}
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
					// Paced so "the first chunk arrived before the server had
					// finished" is a fact about streaming rather than a race.
					// The margin is wider than it needs to be on an idle
					// machine because a loaded one is where a timing assertion
					// turns into a flake, and a flake here reads as a Chrome
					// behaviour change.
					time.Sleep(200 * time.Millisecond)
				}
				mu.Lock()
				writeDone = time.Now()
				mu.Unlock()
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, `<html><body><a id="dl" href="/payload.bin" download="payload.bin">get</a></body></html>`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			ctx, done := interceptionFactsBrowser(t)
			defer done()

			downloadDir := t.TempDir()
			if err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
				return cdpbrowser.SetDownloadBehavior(cdpbrowser.SetDownloadBehaviorBehaviorAllowAndName).
					WithDownloadPath(downloadDir).
					WithEventsEnabled(true).
					Do(cdp.WithExecutor(c, chromedp.FromContext(c).Browser))
			})); err != nil {
				t.Fatalf("set download behaviour: %v", err)
			}

			var (
				pausedResource network.ResourceType
				pausedStatus   int64
				firstRead      time.Time
				streamed       int
				streamErr      error
				downloadStates []string
			)
			finished := make(chan struct{})
			var once sync.Once
			recordDownload := func(ev any) {
				switch event := ev.(type) {
				case *cdpbrowser.EventDownloadWillBegin:
					mu.Lock()
					downloadStates = append(downloadStates, "willBegin")
					mu.Unlock()
				case *cdpbrowser.EventDownloadProgress:
					mu.Lock()
					downloadStates = append(downloadStates, fmt.Sprintf("%s:%d", event.State, int64(event.ReceivedBytes)))
					mu.Unlock()
				}
			}
			chromedp.ListenBrowser(ctx, recordDownload)
			chromedp.ListenTarget(ctx, func(ev any) {
				recordDownload(ev)
				event, ok := ev.(*fetch.EventRequestPaused)
				if !ok {
					return
				}
				isPayload := strings.HasSuffix(event.Request.URL, "/payload.bin")
				go func() {
					answerCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
					defer cancel()
					if !isPayload {
						_ = chromedp.Run(answerCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
							if event.ResponseStatusCode == 0 {
								return fetch.ContinueRequest(event.RequestID).Do(runCtx)
							}
							return fetch.ContinueResponse(event.RequestID).Do(runCtx)
						}))
						return
					}
					if event.ResponseStatusCode == 0 {
						_ = chromedp.Run(answerCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
							return fetch.ContinueRequest(event.RequestID).Do(runCtx)
						}))
						return
					}
					mu.Lock()
					pausedResource = event.ResourceType
					pausedStatus = event.ResponseStatusCode
					mu.Unlock()
					var handle cdpio.StreamHandle
					if err := chromedp.Run(answerCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
						stream, err := fetch.TakeResponseBodyAsStream(event.RequestID).Do(runCtx)
						handle = stream
						return err
					})); err != nil {
						mu.Lock()
						streamErr = err
						mu.Unlock()
						once.Do(func() { close(finished) })
						return
					}
					total := 0
					for {
						var read cdpio.ReadReturns
						params := cdpio.Read(handle).WithSize(32 << 10)
						if err := chromedp.Run(answerCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
							return cdp.Execute(runCtx, cdpio.CommandRead, params, &read)
						})); err != nil {
							mu.Lock()
							streamErr = err
							mu.Unlock()
							break
						}
						size := len(read.Data)
						if read.Base64encoded {
							decoded, err := base64.StdEncoding.DecodeString(read.Data)
							if err != nil {
								mu.Lock()
								streamErr = err
								mu.Unlock()
								break
							}
							size = len(decoded)
						}
						mu.Lock()
						if firstRead.IsZero() && size > 0 {
							firstRead = time.Now()
						}
						mu.Unlock()
						total += size
						if read.EOF {
							break
						}
					}
					_ = chromedp.Run(answerCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
						return cdpio.Close(handle).Do(runCtx)
					}))
					mu.Lock()
					streamed = total
					mu.Unlock()
					// After the stream is taken the request cannot be continued
					// as it was; failing it is what leaves the browser with
					// nothing to write.
					_ = chromedp.Run(answerCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
						return fetch.FailRequest(event.RequestID, network.ErrorReasonAborted).Do(runCtx)
					}))
					once.Do(func() { close(finished) })
				}()
			})

			if err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
				return fetch.Enable().WithPatterns([]*fetch.RequestPattern{
					{URLPattern: "*", RequestStage: fetch.RequestStageResponse},
				}).Do(c)
			})); err != nil {
				t.Fatalf("enable response-stage interception: %v", err)
			}

			runCtx, runCancel := context.WithTimeout(ctx, 60*time.Second)
			defer runCancel()
			target := srv.URL + "/payload.bin"
			if tc.clickToDownload {
				target = srv.URL
			}
			if err := chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
				_, _, _, _, err := page.Navigate(target).Do(c)
				return err
			})); err != nil {
				if !tc.clickToDownload {
					// A navigation that becomes a download reports its own
					// failure once the request is aborted; the assertions below
					// are what decide this case.
					t.Logf("navigate returned %v", err)
				} else {
					t.Fatalf("navigate to the page holding the download link: %v", err)
				}
			}
			if tc.clickToDownload {
				deadline := time.Now().Add(20 * time.Second)
				for time.Now().Before(deadline) {
					var ready bool
					_ = chromedp.Run(runCtx, chromedp.Evaluate(`!!document.getElementById('dl')`, &ready))
					if ready {
						break
					}
					time.Sleep(200 * time.Millisecond)
				}
				if err := chromedp.Run(runCtx, chromedp.Evaluate(`document.getElementById('dl').click()`, nil)); err != nil {
					t.Fatalf("click the download link: %v", err)
				}
			}

			select {
			case <-finished:
			case <-time.After(60 * time.Second):
				t.Fatal("the payload request never reached the response stage")
			}
			// The download manager's verdict lands after the request is failed.
			time.Sleep(3 * time.Second)

			mu.Lock()
			defer mu.Unlock()
			if streamErr != nil {
				t.Fatalf("Fetch.takeResponseBodyAsStream failed: %v", streamErr)
			}
			if pausedStatus != http.StatusOK {
				t.Fatalf("payload paused with status %d, want 200: the response stage never saw it", pausedStatus)
			}
			// A download that starts as a navigation is a document request, which
			// is the shape brw's own interception patterns already cover.
			if pausedResource != network.ResourceTypeDocument {
				t.Errorf("payload paused as resource type %q, want Document", pausedResource)
			}
			if streamed != chunks*chunkSize {
				t.Errorf("streamed %d bytes, want %d", streamed, chunks*chunkSize)
			}
			if writeDone.IsZero() {
				t.Fatal("the server never finished writing the payload, so nothing here measures streaming")
			}
			if firstRead.IsZero() || !firstRead.Before(writeDone) {
				t.Errorf("the first chunk arrived at %v, not before the server finished writing at %v: the browser buffered the whole body rather than streaming it",
					firstRead, writeDone)
			}
			entries, err := os.ReadDir(downloadDir)
			if err != nil {
				t.Fatalf("read download directory: %v", err)
			}
			if len(entries) != 0 {
				names := make([]string, 0, len(entries))
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				t.Errorf("the browser also wrote %v; taking the stream was supposed to replace its copy, not duplicate it", names)
			}
			// The reason brw does not take this route: the transfer the rest of
			// brw tracks never completes. Logged as well as asserted, because
			// what the download manager DOES report is the part a reader of
			// docs/recipes-and-artifacts.md has to be able to check.
			t.Logf("download lifecycle after taking the stream: %v", downloadStates)
			// Two shapes were measured, and the assertion covers both rather
			// than pinning the one this trigger happens to produce: a
			// navigation turned into a download raises no Browser.download*
			// event at all, and an <a download> click raises downloadWillBegin
			// followed by a cancellation at zero bytes. What must never appear
			// is a completed transfer, because that is the entry
			// brw_downloads, brw_wait_for{condition:"download"} and
			// brw_artifact_capture{kind:"download"} all select on.
			for _, state := range downloadStates {
				if strings.HasPrefix(state, string(cdpbrowser.DownloadProgressStateCompleted)) {
					t.Errorf("download lifecycle reported %q; taking the stream is supposed to leave no completed transfer (%v)", state, downloadStates)
				}
			}
			if len(downloadStates) > 0 {
				last := downloadStates[len(downloadStates)-1]
				if last != string(cdpbrowser.DownloadProgressStateCanceled)+":0" {
					t.Errorf("the download manager heard about the transfer and left it at %q, want a cancellation at zero bytes (%v)", last, downloadStates)
				}
			}
		})
	}
}
