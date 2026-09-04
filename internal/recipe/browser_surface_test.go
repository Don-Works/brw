package recipe

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

type findOnlyController struct {
	browser.Controller
	result snapshot.FindResult
	opts   snapshot.FindOptions
}

type trustedOriginController struct {
	browser.Controller
	identity    browser.DocumentIdentity
	identityErr error
	evaluated   bool
}

func (c *trustedOriginController) DocumentIdentity(context.Context) (browser.DocumentIdentity, error) {
	return c.identity, c.identityErr
}

func (c *trustedOriginController) Evaluate(context.Context, string) (any, error) {
	c.evaluated = true
	return "https://spoofed.example.test", nil
}

func TestBrowserSurfaceOriginPrefersTrustedBrowserMetadata(t *testing.T) {
	controller := &trustedOriginController{identity: browser.DocumentIdentity{
		ID: "document-1", Origin: "https://allowed.example.test",
	}}
	surface := BrowserSurface{Browser: controller}
	origin, err := surface.Origin(context.Background())
	if err != nil || origin != "https://allowed.example.test" {
		t.Fatalf("trusted origin = %q, err=%v", origin, err)
	}
	if controller.evaluated {
		t.Fatal("origin check evaluated page JavaScript despite trusted browser metadata")
	}

	controller.identityErr = errors.New("https://private.example.test/transport detail")
	if _, err := surface.Origin(context.Background()); err == nil || strings.Contains(err.Error(), "private.example.test") {
		t.Fatalf("trusted origin failure was not fail-closed and sanitized: %v", err)
	}
	if controller.evaluated {
		t.Fatal("trusted identity failure fell back to page-controlled JavaScript")
	}
}

type valueOnlyController struct {
	browser.Controller
	mu          sync.Mutex
	result      snapshot.FindResult
	values      map[string]string
	assertCalls int
}

func (v *valueOnlyController) Find(_ context.Context, _ snapshot.FindOptions) (snapshot.FindResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.result, nil
}

func (v *valueOnlyController) AssertValue(_ context.Context, ref, expected string, _ time.Duration) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.assertCalls++
	if v.values[ref] != expected {
		return errors.New("assertion did not pass within timeout")
	}
	return nil
}

func (v *valueOnlyController) AssertText(_ context.Context, ref, expected string, _ time.Duration) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.assertCalls++
	if !strings.Contains(strings.ToLower(v.values[ref]), strings.ToLower(expected)) {
		return errors.New("assertion did not pass within timeout")
	}
	return nil
}

func (v *valueOnlyController) AssertValueContains(_ context.Context, ref, expected string, _ time.Duration) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.assertCalls++
	if !strings.Contains(strings.ToLower(v.values[ref]), strings.ToLower(expected)) {
		return errors.New("assertion did not pass within timeout")
	}
	return nil
}

func TestBrowserSurfaceElementValueIsExactWaitableAndFailsClosed(t *testing.T) {
	element := snapshot.Element{Ref: "e1", Role: "textbox", Name: "Composer", Visible: true}
	controller := &valueOnlyController{
		result: snapshot.FindResult{Elements: []snapshot.Element{element}},
		values: map[string]string{"e1": "draft"},
	}
	surface := BrowserSurface{Browser: controller}
	event := Event{Kind: "element.value", Match: "sent", TimeoutMS: 500, Target: &Target{Role: "textbox", Name: "Composer"}}

	satisfied, err := surface.EventSatisfied(context.Background(), event)
	if err != nil || satisfied {
		t.Fatalf("initial satisfied=%v err=%v", satisfied, err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		controller.mu.Lock()
		controller.values["e1"] = "sent"
		controller.mu.Unlock()
	}()
	if err := surface.WaitEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	satisfied, err = surface.EventSatisfied(context.Background(), event)
	if err != nil || !satisfied {
		t.Fatalf("final satisfied=%v err=%v", satisfied, err)
	}

	controller.mu.Lock()
	controller.result.Elements = append(controller.result.Elements, snapshot.Element{Ref: "e2", Role: "textbox", Name: "Composer", Visible: true})
	controller.mu.Unlock()
	if _, err := surface.EventSatisfied(context.Background(), event); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("ambiguous element.value error=%v", err)
	}
}

func TestBrowserSurfaceElementValueContainsTargetsOnlyTheComposer(t *testing.T) {
	controller := &valueOnlyController{
		result: snapshot.FindResult{Elements: []snapshot.Element{{Ref: "e1", Role: "textbox", Name: "Composer", Visible: true}}},
		values: map[string]string{"e1": "Quoted source\nNew reply"},
	}
	surface := BrowserSurface{Browser: controller}
	event := Event{Kind: "element.value_contains", Match: "new reply", TimeoutMS: 100, Target: &Target{Role: "textbox", Name: "Composer"}}
	satisfied, err := surface.EventSatisfied(context.Background(), event)
	if err != nil || !satisfied {
		t.Fatalf("value_contains satisfied=%v err=%v", satisfied, err)
	}
	event.Match = "not in composer"
	satisfied, err = surface.EventSatisfied(context.Background(), event)
	if err != nil || satisfied {
		t.Fatalf("missing value_contains satisfied=%v err=%v", satisfied, err)
	}
}

func (f *findOnlyController) Find(_ context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	f.opts = opts
	return f.result, nil
}

func TestBrowserSurfaceResolveFailsClosedWhenCandidateSetIsTruncated(t *testing.T) {
	controller := &findOnlyController{result: snapshot.FindResult{
		Elements: []snapshot.Element{{Ref: "e1", Role: "button", Name: "Submit", Visible: true}},
		Metadata: map[string]interface{}{"truncated": true},
	}}
	surface := BrowserSurface{Browser: controller}

	_, err := surface.Resolve(context.Background(), Target{Role: "button", Name: "Submit"})
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("Resolve error = %v, want truncated candidate failure", err)
	}
	if controller.opts.Limit != 200 || controller.opts.ViewportOnly {
		t.Fatalf("Find options = %+v, want bounded all-page search", controller.opts)
	}
}

type downloadOnlyController struct {
	browser.Controller
	results []browser.DownloadsResult
	calls   int
}

type networkOnlyController struct {
	browser.Controller
	mu      sync.Mutex
	results [][]snapshot.CapturedRequest
	calls   int
}

func (n *networkOnlyController) NetworkCapture(context.Context, string) ([]snapshot.CapturedRequest, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.calls >= len(n.results) {
		return nil, nil
	}
	result := n.results[n.calls]
	n.calls++
	return result, nil
}

func (d *downloadOnlyController) Downloads(context.Context) (browser.DownloadsResult, error) {
	result := d.results[d.calls]
	d.calls++
	return result, nil
}

type completedDownloadArtifacts struct {
	artifact.API
	directCalls int
	completed   browser.DownloadEntry
}

func (a *completedDownloadArtifacts) CaptureArtifact(context.Context, artifact.CaptureOptions) (artifact.Meta, error) {
	a.directCalls++
	return artifact.Meta{}, nil
}

func (a *completedDownloadArtifacts) CaptureCompletedDownload(_ context.Context, entry browser.DownloadEntry, _ artifact.CaptureOptions) (artifact.Meta, error) {
	a.completed = entry
	return artifact.Meta{ID: "art_33333333333333333333333333333333", Kind: "download"}, nil
}

func TestDownloadPostconditionPreservesOnlyNewEventForFollowingCapture(t *testing.T) {
	old := browser.DownloadEntry{GUID: "old", SuggestedFilename: "invoice.pdf", State: "completed", Path: "/old"}
	fresh := browser.DownloadEntry{GUID: "fresh", SuggestedFilename: "invoice.pdf", State: "completed", Path: "/fresh"}
	controller := &downloadOnlyController{results: []browser.DownloadsResult{
		{Supported: true, Downloads: []browser.DownloadEntry{old}},
		{Supported: true, Downloads: []browser.DownloadEntry{fresh}},
	}}
	artifacts := &completedDownloadArtifacts{}
	surface := &BrowserSurface{Browser: controller, Artifacts: artifacts}
	ctx := browser.WithTabID(context.Background(), "tab-7")
	surface.cacheCompletedDownloads(ctx, []browser.DownloadEntry{old})
	wait, err := surface.ArmEvent(ctx, Event{Kind: "download.completed", Match: "invoice.pdf", TimeoutMS: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := wait(ctx); err != nil {
		t.Fatal(err)
	}
	meta, err := surface.Capture(ctx, CaptureSpec{Kind: "download", Filename: "invoice.pdf"})
	if err != nil || meta.ID == "" || artifacts.completed.GUID != "fresh" || artifacts.directCalls != 0 {
		t.Fatalf("meta=%+v completed=%+v direct=%d err=%v", meta, artifacts.completed, artifacts.directCalls, err)
	}
	if leftover, ok, err := surface.takeCompletedDownload(ctx, "old", ""); err != nil || ok {
		t.Fatalf("pre-arm stale cache lookup: entry=%+v found=%v err=%v", leftover, ok, err)
	}
}

func TestNetworkPostconditionIgnoresPreArmInflightRequestByLifecycleID(t *testing.T) {
	// A is already pending at arm time. It completes before action-caused B and
	// has the same matching URL, so status/URL alone would acknowledge the wrong
	// request. B uses sequence 1 again after a simulated navigation; the document
	// epoch in capture_id keeps it distinct from A.
	const requestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:1"
	const requestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb:1"
	controller := &networkOnlyController{results: [][]snapshot.CapturedRequest{
		{{CaptureID: requestA, URL: "https://example.test/api/save", Status: 0}},
		{{CaptureID: requestA, Completed: true, URL: "https://example.test/api/save", Status: 200, OK: true}},
		{{CaptureID: requestB, Completed: true, URL: "https://example.test/api/save", Status: 200, OK: true}},
	}}
	surface := &BrowserSurface{Browser: controller}
	event := Event{Kind: "network.response", Match: "/api/save", TimeoutMS: 500}
	wait, err := surface.ArmEvent(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if err := wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	calls := controller.calls
	controller.mu.Unlock()
	if calls != 3 {
		t.Fatalf("network capture calls=%d, want arm + ignored A + accepted B", calls)
	}
}

func TestDownloadCaptureFailsClosedForMultipleFreshSameNameMatches(t *testing.T) {
	first := browser.DownloadEntry{GUID: "download-1", SuggestedFilename: "invoice.pdf", State: "completed", Path: "/first"}
	second := browser.DownloadEntry{GUID: "download-2", SuggestedFilename: "invoice.pdf", State: "completed", Path: "/second"}
	artifacts := &completedDownloadArtifacts{}
	surface := &BrowserSurface{Artifacts: artifacts}
	ctx := browser.WithTabID(context.Background(), "tab-7")
	surface.cacheCompletedDownloads(ctx, []browser.DownloadEntry{first, second})

	_, err := surface.Capture(ctx, CaptureSpec{Kind: "download", Filename: "invoice.pdf"})
	if err == nil || err.Error() != "multiple completed downloads match; use a unique download_guid" {
		t.Fatalf("ambiguous capture error=%v", err)
	}
	if artifacts.completed.GUID != "" || artifacts.directCalls != 0 {
		t.Fatalf("ambiguous capture reached artifact service: completed=%+v direct=%d", artifacts.completed, artifacts.directCalls)
	}

	meta, err := surface.Capture(ctx, CaptureSpec{Kind: "download", DownloadGUID: "download-1"})
	if err != nil || meta.ID == "" || artifacts.completed.GUID != "download-1" || artifacts.directCalls != 0 {
		t.Fatalf("unique capture meta=%+v completed=%+v direct=%d err=%v", meta, artifacts.completed, artifacts.directCalls, err)
	}
}

func TestEventPollBackoffIsBoundedAndResponsive(t *testing.T) {
	got := []time.Duration{initialEventPollInterval}
	for range 5 {
		got = append(got, nextEventPollInterval(got[len(got)-1]))
	}
	want := []time.Duration{
		25 * time.Millisecond,
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
		250 * time.Millisecond,
		250 * time.Millisecond,
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("poll interval %d = %s, want %s (all=%v)", index, got[index], want[index], got)
		}
	}

	// A maximum-length event wait used to issue roughly 1,200 browser calls at
	// a fixed 100 ms cadence. The bounded backoff keeps the first probes faster
	// while cutting that idle-call load by more than half.
	elapsed := time.Duration(0)
	interval := initialEventPollInterval
	waits := 0
	for elapsed < 120*time.Second {
		elapsed += interval
		waits++
		interval = nextEventPollInterval(interval)
	}
	fixedWaits := int((120 * time.Second) / (100 * time.Millisecond))
	if waits*2 >= fixedWaits {
		t.Fatalf("adaptive waits=%d, want less than half of fixed waits=%d", waits, fixedWaits)
	}
}

func TestEventPollChecksImmediatelyAndNeverAfterCancellation(t *testing.T) {
	surface := &BrowserSurface{}
	calls := 0
	err := surface.poll(context.Background(), 5*time.Millisecond, func(context.Context) (bool, error) {
		calls++
		return false, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("short poll err=%v calls=%d, want one immediate check then deadline", err, calls)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	err = surface.poll(cancelled, time.Second, func(context.Context) (bool, error) {
		calls++
		return false, nil
	})
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("cancelled poll err=%v calls=%d, want no browser check", err, calls)
	}
}
