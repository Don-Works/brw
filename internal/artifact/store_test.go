package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
)

func newTestStore(t *testing.T, maxArtifact, maxTotal int64) *Store {
	t.Helper()
	store, err := NewStore(Config{Root: t.TempDir(), MaxArtifactBytes: maxArtifact, MaxTotalBytes: maxTotal, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestStorePersistsPayloadFreeMetadataAndBoundedReads(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(Config{Root: root, MaxArtifactBytes: 1 << 20, MaxTotalBytes: 2 << 20, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("PRIVATE-PAGE-SENTINEL\nInvoice 2026-09: paid\n")
	meta, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain; charset=utf-8"}, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(meta)
	if bytes.Contains(encoded, payload) || bytes.Contains(encoded, []byte("PRIVATE-PAGE-SENTINEL")) {
		t.Fatal("artifact metadata leaked page content")
	}
	wantHash := sha256.Sum256(payload)
	if meta.SHA256 != hex.EncodeToString(wantHash[:]) || meta.SizeBytes != int64(len(payload)) {
		t.Fatalf("metadata = %+v", meta)
	}
	if info, _ := os.Stat(root); info.Mode().Perm() != 0o700 {
		t.Fatalf("root mode = %o", info.Mode().Perm())
	}
	for _, suffix := range []string{".blob", ".json"} {
		info, err := os.Stat(filepath.Join(root, meta.ID+suffix))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode/error = %v %v", suffix, info, err)
		}
	}
	first, _, more, err := store.Read(meta.ID, 0, 17)
	if err != nil || !bytes.Equal(first, payload[:17]) || !more {
		t.Fatalf("first=%q more=%v err=%v", first, more, err)
	}
	reopened, err := NewStore(Config{Root: root, MaxArtifactBytes: 1 << 20, MaxTotalBytes: 2 << 20, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.Info(meta.ID); err != nil || got.SHA256 != meta.SHA256 {
		t.Fatalf("reopened=%+v err=%v", got, err)
	}
}

func TestStoreRemovesCorruptCommittedPairAndRemainsUsable(t *testing.T) {
	root := t.TempDir()
	config := Config{Root: root, MaxArtifactBytes: 1 << 20, MaxTotalBytes: 2 << 20, TTL: time.Hour}
	store, err := NewStore(config)
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, strings.NewReader("healthy"))
	if err != nil {
		t.Fatal(err)
	}
	corruptID := "art_11111111111111111111111111111111"
	if err := os.WriteFile(filepath.Join(root, corruptID+".blob"), []byte("orphaned private bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, corruptID+".json"), []byte(`{"artifact_id":`), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewStore(config)
	if err != nil {
		t.Fatalf("corrupt pair bricked store reopen: %v", err)
	}
	for _, suffix := range []string{".blob", ".json"} {
		if _, err := os.Stat(filepath.Join(root, corruptID+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("corrupt %s survived reconciliation: %v", suffix, err)
		}
	}
	if _, err := reopened.Info(healthy.ID); err != nil {
		t.Fatalf("healthy neighbor was damaged: %v", err)
	}
	invalidID := "art_22222222222222222222222222222222"
	if err := os.WriteFile(filepath.Join(root, invalidID+".blob"), []byte("invalid metadata payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidMeta, err := json.Marshal(Meta{ID: invalidID, SizeBytes: -1, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, invalidID+".json"), invalidMeta, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, strings.NewReader("still writable")); err != nil {
		t.Fatalf("store remained unusable after corrupt-pair cleanup: %v", err)
	}
	for _, suffix := range []string{".blob", ".json"} {
		if _, err := os.Stat(filepath.Join(root, invalidID+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("semantically invalid %s survived Put reconciliation: %v", suffix, err)
		}
	}
}

func TestArtifactMetadataSizeDoesNotScaleWithPayload(t *testing.T) {
	store := newTestStore(t, 2<<20, 4<<20)
	payload := bytes.Repeat([]byte("synthetic-page-byte\n"), 1<<16)
	meta, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	inlineBytes := base64.StdEncoding.EncodedLen(len(payload))
	if len(encoded) >= 1024 || len(encoded)*1000 >= inlineBytes || bytes.Contains(encoded, []byte("synthetic-page-byte")) {
		t.Fatalf("payload=%d inline=%d metadata=%d; capture boundary leaked or grew", len(payload), inlineBytes, len(encoded))
	}
	t.Logf("payload=%d bytes, base64-inline=%d bytes, metadata=%d bytes (%.1fx smaller)", len(payload), inlineBytes, len(encoded), float64(inlineBytes)/float64(len(encoded)))
}

func TestReadArtifactPreservesUTF8WhenWindowBisectsRune(t *testing.T) {
	store := newTestStore(t, 1<<20, 2<<20)
	meta, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, strings.NewReader("éclair"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(store, serviceFakeBrowser{})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := service.ReadArtifact(context.Background(), meta.ID, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString([]byte("é")[:1])
	if chunk.Encoding != "base64" || chunk.Base64 != want || chunk.Text != "" || chunk.NextOffset != 1 {
		t.Fatalf("chunk=%+v want exact base64=%q", chunk, want)
	}
}

func TestStoreSearchKindsQuotaExpiryAndConfinement(t *testing.T) {
	store := newTestStore(t, 64, 80)
	meta, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/markdown"}, strings.NewReader("Overview\nInvoice_2026_09.pdf paid\n"))
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.SearchText(meta.ID, "invoice_2026_09", 3)
	if err != nil || len(hits) != 1 || hits[0].Line != 2 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	if _, err := store.Put(PutOptions{Kind: "screenshot", MIMEType: "text/html"}, strings.NewReader("x")); err == nil {
		t.Fatal("kind/MIME confusion accepted")
	}
	if _, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, strings.NewReader(strings.Repeat("x", 65))); err == nil {
		t.Fatal("oversize artifact accepted")
	}
	if _, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, strings.NewReader(strings.Repeat("x", 50))); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("total quota error = %v", err)
	}
	for _, id := range []string{"../../etc/passwd", "art_deadbeef", "ART_00000000000000000000000000000000"} {
		if _, err := store.Info(id); err == nil {
			t.Fatalf("invalid id %q accepted", id)
		}
	}
	store.now = func() time.Time { return meta.ExpiresAt.Add(time.Second) }
	if purged, err := store.PurgeExpired(); err != nil || purged != 1 {
		t.Fatalf("purged=%d err=%v", purged, err)
	}
}

func TestStoreSearchHandlesHugeSingleLineAndFragmentBoundary(t *testing.T) {
	store := newTestStore(t, 4<<20, 8<<20)
	// Put the needle across the 64 KiB reader boundary and keep the entire
	// payload on one line. Search must not inherit bufio.Scanner's token limit.
	payload := strings.Repeat("x", (64<<10)-3) + "INVOICE-BOUNDARY" + strings.Repeat("y", 3<<20)
	meta, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.SearchText(meta.ID, "invoice-boundary", 1)
	if err != nil || len(hits) != 1 || hits[0].Line != 1 || !strings.Contains(strings.ToLower(hits[0].Excerpt), "invoice-boundary") {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
}

func TestStoreSearchUsesOriginalUnicodeByteOffsets(t *testing.T) {
	store := newTestStore(t, 1<<20, 2<<20)
	payload := strings.Repeat("Ⱥ", 200) + " INVOICE-UNICODE"
	meta, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.SearchText(meta.ID, "invoice-unicode", 1)
	if err != nil || len(hits) != 1 || !strings.Contains(strings.ToLower(hits[0].Excerpt), "invoice-unicode") {
		t.Fatalf("unicode hits=%+v err=%v", hits, err)
	}
}

func TestStoreRejectsSymlinkAndGitCheckoutRoots(t *testing.T) {
	if _, err := NewStore(Config{Root: string(filepath.Separator)}); err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("filesystem root error = %v", err)
	}
	publicRoot := filepath.Join(t.TempDir(), "public-artifacts")
	if err := os.Mkdir(publicRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(publicRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(Config{Root: publicRoot}); err != nil {
		t.Fatalf("harden public root: %v", err)
	}
	if info, err := os.Stat(publicRoot); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("root permissions were not hardened: info=%v err=%v", info, err)
	}

	realRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "artifacts")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(Config{Root: alias}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink root error = %v", err)
	}
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(Config{Root: filepath.Join(repo, "tmp", "artifacts")}); err == nil || !strings.Contains(err.Error(), "git checkout") {
		t.Fatalf("git root error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(repo, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	parentAlias := filepath.Join(t.TempDir(), "innocent-parent")
	if err := os.Symlink(filepath.Join(repo, "nested"), parentAlias); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(Config{Root: filepath.Join(parentAlias, "artifacts")}); err == nil || !strings.Contains(err.Error(), "git checkout") {
		t.Fatalf("symlinked parent into git checkout error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "nested", "artifacts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected root was created inside checkout: %v", err)
	}
}

func TestStoreConcurrentWritersAreUnique(t *testing.T) {
	store := newTestStore(t, 1<<20, 128<<20)
	const count = 64
	ids := make(chan string, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			meta, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, strings.NewReader(strings.Repeat("x", i+1)))
			if err != nil {
				t.Errorf("put: %v", err)
				return
			}
			ids <- meta.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
	if len(seen) != count {
		t.Fatalf("count=%d", len(seen))
	}
}

type cancelAfterFirstRead struct {
	cancel context.CancelFunc
	read   bool
}

func (r *cancelAfterFirstRead) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	for index := range p {
		p[index] = 'x'
	}
	r.cancel()
	return len(p), nil
}

func TestStorePutContextRemovesPartialArtifactOnCancellation(t *testing.T) {
	store := newTestStore(t, 1<<20, 2<<20)
	ctx, cancel := context.WithCancel(context.Background())
	_, err := store.PutContext(ctx, PutOptions{Kind: "text", MIMEType: "text/plain"}, &cancelAfterFirstRead{cancel: cancel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PutContext error = %v, want cancellation", err)
	}
	entries, err := os.ReadDir(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".artifact-") || strings.HasSuffix(entry.Name(), ".blob") || strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("cancelled write left partial artifact %q", entry.Name())
		}
	}
}

func TestStoreReclaimsOnlyStaleCrashOrphans(t *testing.T) {
	root := t.TempDir()
	oldID := "art_11111111111111111111111111111111"
	youngID := "art_22222222222222222222222222222222"
	oldBlob := filepath.Join(root, oldID+".blob")
	youngBlob := filepath.Join(root, youngID+".blob")
	if err := os.WriteFile(oldBlob, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(youngBlob, []byte("in-flight"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-orphanGrace - time.Minute)
	if err := os.Chtimes(oldBlob, old, old); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(Config{Root: root, MaxArtifactBytes: 1 << 20, MaxTotalBytes: 2 << 20, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldBlob); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale orphan survived recovery: %v", err)
	}
	if _, err := os.Stat(youngBlob); err != nil {
		t.Fatalf("young possible in-flight blob was removed: %v", err)
	}
	store.now = func() time.Time { return time.Now().Add(orphanGrace + time.Minute) }
	if _, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, strings.NewReader("ok")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(youngBlob); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aged orphan survived next write recovery: %v", err)
	}
}

func TestStoreJanitorPurgesExpiredArtifactsWhileIdle(t *testing.T) {
	store := newTestStore(t, 1<<20, 2<<20)
	now := time.Now().UTC()
	store.now = func() time.Time { return now }
	meta, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain", TTL: time.Second}, strings.NewReader("expires while idle"))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		store.RunJanitor(ctx, 5*time.Millisecond, func(err error) { t.Errorf("janitor: %v", err) })
		close(done)
	}()
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		_, blobErr := os.Stat(store.blobPath(meta.ID))
		_, metaErr := os.Stat(store.metaPath(meta.ID))
		if errors.Is(blobErr, os.ErrNotExist) && errors.Is(metaErr, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("idle janitor did not purge artifact: blob=%v metadata=%v", blobErr, metaErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("artifact janitor did not stop on cancellation")
	}
}

type serviceFakeBrowser struct {
	browser.Controller
	read       readability.PageRead
	screenshot browser.Screenshot
}

type rawServiceFakeBrowser struct {
	serviceFakeBrowser
	rawCalls int
}

type driftingRawServiceFakeBrowser struct {
	serviceFakeBrowser
	origin     string
	documentID string
}

type navigatingCaptureBrowser struct {
	serviceFakeBrowser
	current      browser.DocumentIdentity
	next         browser.DocumentIdentity
	capturedURL  string
	identityErr  error
	transitioned bool
}

type capturedURLServiceFakeBrowser struct {
	serviceFakeBrowser
	origin   string
	snapshot snapshot.PageSnapshot
}

type downloadServiceFakeBrowser struct {
	serviceFakeBrowser
	result browser.DownloadsResult
	calls  int
	origin string
}

type managedDownloadServiceFakeBrowser struct {
	downloadServiceFakeBrowser
	cleanupCalls int
	cleanupErr   error
}

type deadlineVideoBrowser struct {
	serviceFakeBrowser
	deadlineSeen chan time.Time
	release      <-chan struct{}
	returned     chan<- struct{}
}

func (f *capturedURLServiceFakeBrowser) Evaluate(context.Context, string) (any, error) {
	return f.origin, nil
}

func (f *capturedURLServiceFakeBrowser) DocumentIdentity(context.Context) (browser.DocumentIdentity, error) {
	return browser.DocumentIdentity{ID: "captured-url-doc", Origin: f.origin}, nil
}

func (f *capturedURLServiceFakeBrowser) Snapshot(context.Context, snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	return f.snapshot, nil
}

func (f *downloadServiceFakeBrowser) Downloads(context.Context) (browser.DownloadsResult, error) {
	f.calls++
	return f.result, nil
}

func (f *downloadServiceFakeBrowser) Evaluate(ctx context.Context, expression string) (any, error) {
	if f.origin != "" {
		return f.origin, nil
	}
	return f.serviceFakeBrowser.Evaluate(ctx, expression)
}

func (f *downloadServiceFakeBrowser) DocumentIdentity(context.Context) (browser.DocumentIdentity, error) {
	return browser.DocumentIdentity{ID: "download-doc", Origin: f.origin}, nil
}

func (f *managedDownloadServiceFakeBrowser) CleanupManagedDownload(item browser.DownloadEntry) (bool, error) {
	f.cleanupCalls++
	if f.cleanupErr != nil {
		return true, f.cleanupErr
	}
	return true, os.Remove(item.Path)
}

func (f *driftingRawServiceFakeBrowser) Evaluate(context.Context, string) (any, error) {
	return f.origin, nil
}

func (f *driftingRawServiceFakeBrowser) DocumentIdentity(context.Context) (browser.DocumentIdentity, error) {
	documentID := f.documentID
	if documentID == "" {
		documentID = "drift-start"
	}
	return browser.DocumentIdentity{ID: documentID, Origin: f.origin}, nil
}

func (f *driftingRawServiceFakeBrowser) CaptureArtifactScreenshot(context.Context, string) (browser.Screenshot, error) {
	f.origin = "https://drifted.example.test"
	f.documentID = "drift-finish"
	return f.screenshot, nil
}

func (f *navigatingCaptureBrowser) DocumentIdentity(context.Context) (browser.DocumentIdentity, error) {
	if f.transitioned && f.identityErr != nil {
		return browser.DocumentIdentity{}, f.identityErr
	}
	return f.current, nil
}

func (f *navigatingCaptureBrowser) transition() {
	f.transitioned = true
	f.current = f.next
}

func (f *navigatingCaptureBrowser) Read(context.Context) (readability.PageRead, error) {
	f.transition()
	return readability.PageRead{URL: f.capturedURL, Title: "replacement", Main: "replacement payload"}, nil
}

func (f *navigatingCaptureBrowser) Snapshot(context.Context, snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	f.transition()
	return snapshot.PageSnapshot{URL: f.capturedURL, Title: "replacement"}, nil
}

func (f *navigatingCaptureBrowser) CaptureArtifactScreenshot(context.Context, string) (browser.Screenshot, error) {
	f.transition()
	return f.screenshot, nil
}

func (f *navigatingCaptureBrowser) CapturePDF(context.Context) ([]byte, error) {
	f.transition()
	return []byte("%PDF-1.7\nreplacement\n%%EOF"), nil
}

func (f *rawServiceFakeBrowser) CaptureArtifactScreenshot(context.Context, string) (browser.Screenshot, error) {
	f.rawCalls++
	return f.screenshot, nil
}

func (f *deadlineVideoBrowser) Screenshot(ctx context.Context) (browser.Screenshot, error) {
	if deadline, ok := ctx.Deadline(); ok {
		select {
		case f.deadlineSeen <- deadline:
		default:
		}
	}
	// Deliberately ignore ctx until the test releases us. The service must still
	// return on its internal deadline; a context-aware fake would not prove the
	// hard boundary around a faulty browser transport.
	<-f.release
	if f.returned != nil {
		f.returned <- struct{}{}
	}
	return browser.Screenshot{}, errors.New("released synthetic screenshot hang")
}

func (f serviceFakeBrowser) Read(context.Context) (readability.PageRead, error) { return f.read, nil }
func (f serviceFakeBrowser) Screenshot(context.Context) (browser.Screenshot, error) {
	return f.screenshot, nil
}
func (f serviceFakeBrowser) Evaluate(context.Context, string) (any, error) {
	return map[string]any{"url": f.read.URL, "title": f.read.Title}, nil
}

func TestServiceCapturesTextScreenshotAndExplicitChunks(t *testing.T) {
	pngBytes := onePixelPNG(t)
	fake := serviceFakeBrowser{
		read:       readability.PageRead{URL: "https://example.test/billing", Title: "Billing", Main: strings.Repeat("invoice line\n", 1000)},
		screenshot: browser.Screenshot{MIMEType: "image/png", Base64: base64.StdEncoding.EncodeToString(pngBytes)},
	}
	service, err := NewService(newTestStore(t, 2<<20, 4<<20), fake)
	if err != nil {
		t.Fatal(err)
	}
	textMeta, err := service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "text"})
	if err != nil {
		t.Fatal(err)
	}
	shotMeta, err := service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "screenshot"})
	if err != nil {
		t.Fatal(err)
	}
	resultJSON, _ := json.Marshal([]Meta{textMeta, shotMeta})
	if bytes.Contains(resultJSON, []byte("invoice line")) || bytes.Contains(resultJSON, pngBytes) {
		t.Fatal("capture result leaked artifact bytes")
	}
	chunk, err := service.ReadArtifact(context.Background(), textMeta.ID, 0, 32)
	if err != nil || chunk.Encoding != "utf-8" || len(chunk.Text) != 32 || !chunk.More {
		t.Fatalf("chunk=%+v err=%v", chunk, err)
	}
	imageChunk, err := service.ReadArtifact(context.Background(), shotMeta.ID, 0, 64<<10)
	if err != nil || imageChunk.Encoding != "base64" || imageChunk.Base64 == "" {
		t.Fatalf("image chunk=%+v err=%v", imageChunk, err)
	}
}

func TestServiceCapturesAnAlreadyObservedCompletedDownload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invoice.pdf")
	payload := []byte("%PDF-1.7\nsynthetic source\n")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(newTestStore(t, 2<<20, 4<<20), serviceFakeBrowser{})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := service.CaptureCompletedDownload(context.Background(), browser.DownloadEntry{
		GUID: "guid-1", SuggestedFilename: "invoice.pdf", State: "completed", Path: path,
	}, CaptureOptions{Kind: "download", DownloadGUID: "guid-1"})
	if err != nil || meta.Kind != "download" || meta.MIMEType != "application/pdf" || meta.SizeBytes != int64(len(payload)) {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
}

func TestServiceDownloadOpenTimeoutIsBoundedAndDoesNotAmplify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invoice.pdf")
	payload := []byte("%PDF-1.7\nsource remains user-owned\n")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(newTestStore(t, 2<<20, 4<<20), serviceFakeBrowser{})
	if err != nil {
		t.Fatal(err)
	}
	service.downloadSourceOpenTimeout = 40 * time.Millisecond

	releaseOpen := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseOpen) })
	openerReturned := make(chan struct{}, 1)
	var openMu sync.Mutex
	openCalls := 0
	service.downloadSourceOpener = func(name string) (*os.File, error) {
		openMu.Lock()
		openCalls++
		openMu.Unlock()
		<-releaseOpen
		select {
		case openerReturned <- struct{}{}:
		default:
		}
		return os.Open(name)
	}
	item := browser.DownloadEntry{
		GUID: "blocked-guid", SuggestedFilename: "invoice.pdf", State: "completed", Path: path,
	}
	capture := func() (Meta, error) {
		return service.CaptureCompletedDownload(context.Background(), item, CaptureOptions{Kind: "download", DownloadGUID: item.GUID})
	}

	started := time.Now()
	if _, err := capture(); err == nil || !strings.Contains(err.Error(), "source open did not complete") {
		t.Fatalf("blocked open error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked open returned after %s, want a bounded failure", elapsed)
	}

	// Retrying the request must not start another uninterruptible open. This was
	// the live failure mode: every client retry stranded another goroutine and OS
	// thread in the same macOS TCC syscall.
	const retries = 32
	errorsSeen := make(chan error, retries)
	var retriesWG sync.WaitGroup
	for range retries {
		retriesWG.Add(1)
		go func() {
			defer retriesWG.Done()
			_, retryErr := capture()
			errorsSeen <- retryErr
		}()
	}
	retriesWG.Wait()
	close(errorsSeen)
	for retryErr := range errorsSeen {
		if retryErr == nil || !strings.Contains(retryErr.Error(), "in progress or blocked") {
			t.Fatalf("retry error = %v, want fail-fast blocked error", retryErr)
		}
	}
	openMu.Lock()
	gotOpenCalls := openCalls
	openMu.Unlock()
	if gotOpenCalls != 1 {
		t.Fatalf("source opener calls = %d, want exactly one despite %d retries", gotOpenCalls, retries)
	}

	releaseOnce.Do(func() { close(releaseOpen) })
	select {
	case <-openerReturned:
	case <-time.After(time.Second):
		t.Fatal("synthetic blocked opener did not return after release")
	}
	deadline := time.Now().Add(time.Second)
	for len(service.downloadOpenGate) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(service.downloadOpenGate) != 0 {
		t.Fatal("download open gate stayed occupied after the source opener recovered")
	}
	meta, err := capture()
	if err != nil || meta.SizeBytes != int64(len(payload)) {
		t.Fatalf("capture did not recover after open resumed: meta=%+v err=%v", meta, err)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("user-owned source changed: data=%q err=%v", data, err)
	}
}

func TestServiceDownloadBlockedOpenHonorsCallerCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invoice.pdf")
	if err := os.WriteFile(path, []byte("%PDF fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(newTestStore(t, 2<<20, 4<<20), serviceFakeBrowser{})
	if err != nil {
		t.Fatal(err)
	}
	service.downloadSourceOpenTimeout = time.Second
	releaseOpen := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseOpen) })
	openerStarted := make(chan struct{})
	openerReturned := make(chan struct{})
	var startOnce sync.Once
	service.downloadSourceOpener = func(name string) (*os.File, error) {
		startOnce.Do(func() { close(openerStarted) })
		<-releaseOpen
		close(openerReturned)
		return os.Open(name)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, captureErr := service.CaptureCompletedDownload(ctx, browser.DownloadEntry{
			GUID: "cancel-guid", SuggestedFilename: "invoice.pdf", State: "completed", Path: path,
		}, CaptureOptions{Kind: "download", DownloadGUID: "cancel-guid"})
		result <- captureErr
	}()
	select {
	case <-openerStarted:
	case <-time.After(time.Second):
		t.Fatal("synthetic source opener did not start")
	}
	cancel()
	select {
	case captureErr := <-result:
		if !errors.Is(captureErr, context.Canceled) {
			t.Fatalf("cancelled capture error = %v, want context.Canceled", captureErr)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled capture did not return while its source open was blocked")
	}
	releaseOnce.Do(func() { close(releaseOpen) })
	select {
	case <-openerReturned:
	case <-time.After(time.Second):
		t.Fatal("cancelled source opener did not return after release")
	}
	deadline := time.Now().Add(time.Second)
	for len(service.downloadOpenGate) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(service.downloadOpenGate) != 0 {
		t.Fatal("cancelled open did not release its single-flight gate after recovery")
	}
}

func TestServiceDownloadsListingThenCaptureWorksAndPreservesUserOriginal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invoice.pdf")
	payload := []byte("%PDF-1.7\nlisted then captured\n")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &downloadServiceFakeBrowser{result: browser.DownloadsResult{Supported: true, Count: 1, Downloads: []browser.DownloadEntry{{
		GUID: "guid-1", SuggestedFilename: "invoice.pdf", TabID: "tab-1", State: "completed", Path: path,
	}}}}
	service, err := NewService(newTestStore(t, 2<<20, 4<<20), fake)
	if err != nil {
		t.Fatal(err)
	}
	if listed, err := fake.Downloads(context.Background()); err != nil || listed.Count != 1 {
		t.Fatalf("list before capture: %+v err=%v", listed, err)
	}
	ctx := browser.WithTabID(context.Background(), "tab-1")
	meta, err := service.CaptureArtifact(ctx, CaptureOptions{Kind: "download", DownloadGUID: "guid-1"})
	if err != nil || meta.Kind != "download" || fake.calls != 2 {
		t.Fatalf("meta=%+v calls=%d err=%v", meta, fake.calls, err)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("user-owned download original changed: data=%q err=%v", data, err)
	}
}

func TestServiceDownloadMatchingRejectsWrongTabAndAmbiguousFilename(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "one.pdf"), filepath.Join(dir, "two.pdf")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("%PDF fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fake := &downloadServiceFakeBrowser{result: browser.DownloadsResult{Supported: true, Downloads: []browser.DownloadEntry{
		{GUID: "other-tab", SuggestedFilename: "invoice.pdf", TabID: "tab-2", State: "completed", Path: paths[0]},
		{GUID: "same-tab-1", SuggestedFilename: "invoice.pdf", TabID: "tab-1", State: "completed", Path: paths[0]},
		{GUID: "same-tab-2", SuggestedFilename: "invoice.pdf", TabID: "tab-1", State: "completed", Path: paths[1]},
	}}}
	service, err := NewService(newTestStore(t, 2<<20, 4<<20), fake)
	if err != nil {
		t.Fatal(err)
	}
	ctx := browser.WithTabID(context.Background(), "tab-1")
	if _, err := service.CaptureArtifact(ctx, CaptureOptions{Kind: "download", DownloadGUID: "other-tab"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("cross-tab GUID capture error = %v", err)
	}
	if _, err := service.CaptureArtifact(ctx, CaptureOptions{Kind: "download", Filename: "invoice.pdf"}); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("ambiguous filename capture error = %v", err)
	}
	fake.origin = "https://allowed.example.test"
	fake.result = browser.DownloadsResult{Supported: true, Downloads: []browser.DownloadEntry{{
		GUID: "unknown-tab", SuggestedFilename: "invoice.pdf", State: "completed", Path: paths[0],
	}}}
	recipeCtx := browser.WithAllowedOrigins(ctx, []string{"https://allowed.example.test"})
	if _, err := service.CaptureArtifact(recipeCtx, CaptureOptions{Kind: "download", DownloadGUID: "unknown-tab"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("recipe accepted unattributed download: %v", err)
	}
}

func TestServiceRemovesManagedDownloadOnlyAfterPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed-guid")
	if err := os.WriteFile(path, []byte("managed download"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &managedDownloadServiceFakeBrowser{downloadServiceFakeBrowser: downloadServiceFakeBrowser{result: browser.DownloadsResult{
		Supported: true,
		Downloads: []browser.DownloadEntry{{GUID: "managed-guid", SuggestedFilename: "data.bin", State: "completed", Path: path}},
	}}}
	store := newTestStore(t, 2<<20, 4<<20)
	service, err := NewService(store, fake)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "download", DownloadGUID: "managed-guid"})
	if err != nil || fake.cleanupCalls != 1 {
		t.Fatalf("meta=%+v cleanup_calls=%d err=%v", meta, fake.cleanupCalls, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed source survived successful persistence: %v", err)
	}
	if _, err := store.Info(meta.ID); err != nil {
		t.Fatalf("persisted artifact unavailable after source cleanup: %v", err)
	}
}

func TestServiceRollsBackArtifactWhenManagedCleanupFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed-guid")
	if err := os.WriteFile(path, []byte("managed download"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &managedDownloadServiceFakeBrowser{
		downloadServiceFakeBrowser: downloadServiceFakeBrowser{result: browser.DownloadsResult{Supported: true, Downloads: []browser.DownloadEntry{{
			GUID: "managed-guid", SuggestedFilename: "data.bin", State: "completed", Path: path,
		}}}},
		cleanupErr: errors.New("synthetic cleanup failure"),
	}
	store := newTestStore(t, 2<<20, 4<<20)
	service, err := NewService(store, fake)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "download", DownloadGUID: "managed-guid"}); err == nil || !strings.Contains(err.Error(), "synthetic cleanup failure") {
		t.Fatalf("cleanup failure = %v", err)
	}
	entries, err := os.ReadDir(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".blob") || strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("cleanup failure left inaccessible artifact %q", entry.Name())
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("failed cleanup unexpectedly removed source: %v", err)
	}
}

func TestRecipeCaptureValidatesReturnedReadAndSnapshotURLs(t *testing.T) {
	for _, kind := range []string{"text", "semantic_json"} {
		t.Run(kind, func(t *testing.T) {
			store := newTestStore(t, 2<<20, 4<<20)
			fake := &capturedURLServiceFakeBrowser{
				serviceFakeBrowser: serviceFakeBrowser{read: readability.PageRead{URL: "https://denied.example.test/private", Title: "Denied", Main: "secret"}},
				origin:             "https://allowed.example.test",
				snapshot:           snapshot.PageSnapshot{URL: "https://denied.example.test/private", Title: "Denied"},
			}
			service, err := NewService(store, fake)
			if err != nil {
				t.Fatal(err)
			}
			ctx := browser.WithAllowedOrigins(context.Background(), []string{"https://allowed.example.test"})
			if _, err := service.CaptureArtifact(ctx, CaptureOptions{Kind: kind}); err == nil || !strings.Contains(err.Error(), "captured page origin") {
				t.Fatalf("returned URL race was accepted: %v", err)
			}
			entries, err := os.ReadDir(store.Root())
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".blob") || strings.HasSuffix(entry.Name(), ".json") {
					t.Fatalf("disallowed returned URL persisted artifact %q", entry.Name())
				}
			}
		})
	}
}

func TestGuardCapturedPageURLUsesExactOrigin(t *testing.T) {
	ctx := browser.WithAllowedOrigins(context.Background(), []string{"https://allowed.example.test"})
	userinfoConfusionURL := "https://allowed.example.test" + "@evil.invalid/private"
	for _, test := range []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "allowed path", url: "https://allowed.example.test/billing?month=9#invoice"},
		{name: "lookalike host", url: "https://allowed.example.test.evil.invalid/private", wantErr: true},
		{name: "userinfo confusion", url: userinfoConfusionURL, wantErr: true},
		{name: "scheme relative", url: "//allowed.example.test/private", wantErr: true},
		{name: "empty", url: "", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := guardCapturedPageURL(ctx, test.url)
			if (err != nil) != test.wantErr {
				t.Fatalf("guard(%q) error=%v wantErr=%v", test.url, err, test.wantErr)
			}
		})
	}
	if err := guardCapturedPageURL(context.Background(), "not a URL"); err != nil {
		t.Fatalf("ordinary non-recipe capture was unexpectedly restricted: %v", err)
	}
}

func TestServicePrefersRawScreenshotCapability(t *testing.T) {
	pngBytes := onePixelPNG(t)
	fake := &rawServiceFakeBrowser{serviceFakeBrowser: serviceFakeBrowser{
		read:       readability.PageRead{URL: "https://example.test", Title: "Raw"},
		screenshot: browser.Screenshot{MIMEType: "image/png", Data: pngBytes},
	}}
	service, err := NewService(newTestStore(t, 2<<20, 4<<20), fake)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "screenshot"})
	if err != nil || fake.rawCalls != 1 || meta.SizeBytes != int64(len(pngBytes)) {
		t.Fatalf("meta=%+v raw_calls=%d err=%v", meta, fake.rawCalls, err)
	}
}

func TestRecipeCaptureDeletesArtifactOnOriginDrift(t *testing.T) {
	pngBytes := onePixelPNG(t)
	store := newTestStore(t, 2<<20, 4<<20)
	fake := &driftingRawServiceFakeBrowser{
		serviceFakeBrowser: serviceFakeBrowser{screenshot: browser.Screenshot{MIMEType: "image/png", Data: pngBytes}},
		origin:             "https://allowed.example.test",
	}
	service, err := NewService(store, fake)
	if err != nil {
		t.Fatal(err)
	}
	ctx := browser.WithAllowedOrigins(context.Background(), []string{"https://allowed.example.test"})
	if _, err := service.CaptureArtifact(ctx, CaptureOptions{Kind: "screenshot"}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("origin-drifting capture error = %v", err)
	}
	entries, err := os.ReadDir(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".blob") || strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("origin-drifting capture left persisted artifact %q", entry.Name())
		}
	}
}

func TestRecipeCaptureRejectsReplacementDocumentForEveryPageArtifact(t *testing.T) {
	pngBytes := onePixelPNG(t)
	if _, err := resolveFFmpegPath(); err != nil {
		t.Log("ffmpeg unavailable; video continuity subtests will be skipped")
	}
	scenarios := []struct {
		name    string
		start   browser.DocumentIdentity
		finish  browser.DocumentIdentity
		allowed []string
		url     string
	}{
		{
			name:    "navigation between two allowed origins",
			start:   browser.DocumentIdentity{ID: "tab\x00doc-a\x000", Origin: "https://one.example.test"},
			finish:  browser.DocumentIdentity{ID: "tab\x00doc-b\x001", Origin: "https://two.example.test"},
			allowed: []string{"https://one.example.test", "https://two.example.test"},
			url:     "https://two.example.test/replacement",
		},
		{
			name:    "same-origin replacement document",
			start:   browser.DocumentIdentity{ID: "tab\x00doc-a\x000", Origin: "https://same.example.test"},
			finish:  browser.DocumentIdentity{ID: "tab\x00doc-b\x001", Origin: "https://same.example.test"},
			allowed: []string{"https://same.example.test"},
			url:     "https://same.example.test/reloaded",
		},
	}
	for _, scenario := range scenarios {
		for _, kind := range []string{"text", "semantic_json", "screenshot", "pdf", "video"} {
			t.Run(scenario.name+"/"+kind, func(t *testing.T) {
				if kind == "video" {
					if _, err := resolveFFmpegPath(); err != nil {
						t.Skip("ffmpeg not installed")
					}
				}
				store := newTestStore(t, 4<<20, 8<<20)
				fake := &navigatingCaptureBrowser{
					serviceFakeBrowser: serviceFakeBrowser{screenshot: browser.Screenshot{MIMEType: "image/png", Data: pngBytes}},
					current:            scenario.start, next: scenario.finish, capturedURL: scenario.url,
				}
				service, err := NewService(store, fake)
				if err != nil {
					t.Fatal(err)
				}
				opts := CaptureOptions{Kind: kind}
				if kind == "video" {
					opts.DurationMS = 100
					opts.FPS = 1
				}
				ctx := browser.WithAllowedOrigins(context.Background(), scenario.allowed)
				if _, err := service.CaptureArtifact(ctx, opts); err == nil || !strings.Contains(err.Error(), "crossed a main-document boundary") {
					t.Fatalf("replacement-document capture error = %v", err)
				}
				assertNoCommittedArtifacts(t, store.Root())
			})
		}
	}
}

func TestRecipeCaptureAllowsSameDocumentSPARouteChange(t *testing.T) {
	store := newTestStore(t, 2<<20, 4<<20)
	fake := &navigatingCaptureBrowser{
		current:     browser.DocumentIdentity{ID: "tab\x00spa-doc\x007", Origin: "https://spa.example.test"},
		next:        browser.DocumentIdentity{ID: "tab\x00spa-doc\x007", Origin: "https://spa.example.test"},
		capturedURL: "https://spa.example.test/invoices/2026",
	}
	service, err := NewService(store, fake)
	if err != nil {
		t.Fatal(err)
	}
	ctx := browser.WithAllowedOrigins(context.Background(), []string{"https://spa.example.test"})
	meta, err := service.CaptureArtifact(ctx, CaptureOptions{Kind: "text"})
	if err != nil {
		t.Fatalf("same-document SPA capture failed: %v", err)
	}
	if _, err := store.Info(meta.ID); err != nil {
		t.Fatalf("same-document SPA artifact was not persisted: %v", err)
	}
}

func TestRecipeCaptureFailsClosedWithoutIdentityButManualCaptureDoesNotProbe(t *testing.T) {
	pngBytes := onePixelPNG(t)
	fake := &rawServiceFakeBrowser{serviceFakeBrowser: serviceFakeBrowser{
		screenshot: browser.Screenshot{MIMEType: "image/png", Data: pngBytes},
	}}
	service, err := NewService(newTestStore(t, 2<<20, 4<<20), fake)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "screenshot"}); err != nil {
		t.Fatalf("manual capture was burdened by recipe continuity: %v", err)
	}
	ctx := browser.WithAllowedOrigins(context.Background(), []string{"https://allowed.example.test"})
	if _, err := service.CaptureArtifact(ctx, CaptureOptions{Kind: "screenshot"}); err == nil || !strings.Contains(err.Error(), "requires main-document identity") {
		t.Fatalf("recipe capture without identity capability error = %v", err)
	}
	if fake.rawCalls != 1 {
		t.Fatalf("unsupported recipe capture actuated browser %d times, want only the prior manual capture", fake.rawCalls)
	}
}

func TestRecipeCaptureRollsBackWhenEndIdentityProbeFails(t *testing.T) {
	store := newTestStore(t, 2<<20, 4<<20)
	fake := &navigatingCaptureBrowser{
		serviceFakeBrowser: serviceFakeBrowser{screenshot: browser.Screenshot{MIMEType: "image/png", Data: onePixelPNG(t)}},
		current:            browser.DocumentIdentity{ID: "doc-a", Origin: "https://allowed.example.test"},
		next:               browser.DocumentIdentity{ID: "doc-a", Origin: "https://allowed.example.test"},
		identityErr:        errors.New("https://private.example.test/internal transport failure"),
	}
	service, err := NewService(store, fake)
	if err != nil {
		t.Fatal(err)
	}
	ctx := browser.WithAllowedOrigins(context.Background(), []string{"https://allowed.example.test"})
	_, err = service.CaptureArtifact(ctx, CaptureOptions{Kind: "screenshot"})
	if err == nil || !strings.Contains(err.Error(), "could not verify the main document") {
		t.Fatalf("end identity failure = %v", err)
	}
	if strings.Contains(err.Error(), "private.example.test") {
		t.Fatalf("identity failure leaked a page URL: %v", err)
	}
	assertNoCommittedArtifacts(t, store.Root())
}

func assertNoCommittedArtifacts(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".blob") || strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("rejected capture left persisted artifact %q", entry.Name())
		}
	}
}

func TestServiceRejectsIgnoredOrOversizedCaptureFields(t *testing.T) {
	service, err := NewService(newTestStore(t, 2<<20, 4<<20), serviceFakeBrowser{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []CaptureOptions{
		{Kind: "text", Ref: "e1"},
		{Kind: "screenshot", Ref: strings.Repeat("e", 501)},
		{Kind: "text", DurationMS: 100, FPS: 1},
		{Kind: "download", DownloadGUID: "one", Filename: "two"},
		{Kind: "text", Redaction: strings.Repeat("x", 257)},
		{Kind: "text", TTL: time.Second, TTLSeconds: 1},
	}
	for _, opts := range tests {
		if _, err := service.CaptureArtifact(context.Background(), opts); err == nil {
			t.Errorf("capture accepted conflicting fields: %+v", opts)
		}
	}
}

func TestServiceRejectsTTLSecondsThatWouldOverflowDuration(t *testing.T) {
	const maxDurationSeconds = int64(^uint64(0)>>1) / int64(time.Second)
	if int64(int(^uint(0)>>1)) <= maxDurationSeconds {
		t.Skip("int cannot represent a duration-overflowing seconds value on this architecture")
	}
	service, err := NewService(newTestStore(t, 2<<20, 4<<20), serviceFakeBrowser{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CaptureArtifact(context.Background(), CaptureOptions{
		Kind: "text", TTLSeconds: int(maxDurationSeconds + 1),
	})
	if err == nil || !strings.Contains(err.Error(), "supported duration range") {
		t.Fatalf("duration overflow was not rejected: %v", err)
	}
}

func TestServiceCreatesBoundedVideoArtifactWhenFFmpegAvailable(t *testing.T) {
	if _, err := resolveFFmpegPath(); err != nil {
		t.Skip("ffmpeg not installed")
	}
	pngBytes := onePixelPNG(t)
	fake := serviceFakeBrowser{
		read:       readability.PageRead{URL: "https://example.test", Title: "Video"},
		screenshot: browser.Screenshot{MIMEType: "image/png", Data: pngBytes},
	}
	service, err := NewService(newTestStore(t, 4<<20, 8<<20), fake)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "video", DurationMS: 150, FPS: 2})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Kind != "video" || meta.MIMEType != "video/webm" || meta.SizeBytes == 0 {
		t.Fatalf("meta=%+v", meta)
	}
	entries, err := os.ReadDir(service.Store().Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".video-") {
			t.Fatalf("temporary video payload left behind: %s", entry.Name())
		}
	}
	if _, err := service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "video", DurationMS: 30_000, FPS: 30}); err == nil || !strings.Contains(err.Error(), "300 frames") {
		t.Fatalf("video frame bomb was not rejected: %v", err)
	}
}

func TestVideoCaptureBudgetAddsBoundedHeadroom(t *testing.T) {
	for _, test := range []struct {
		durationMS int
		want       time.Duration
	}{
		{durationMS: 100, want: 2100 * time.Millisecond},
		{durationMS: 10_000, want: 12 * time.Second},
		{durationMS: 30_000, want: 35 * time.Second},
	} {
		if got := videoCaptureBudget(test.durationMS); got != test.want {
			t.Errorf("videoCaptureBudget(%d) = %s, want %s", test.durationMS, got, test.want)
		}
	}
}

func TestBoundedTailBufferKeepsFixedSizeSuffix(t *testing.T) {
	buffer := newBoundedTailBuffer(16)
	for _, value := range []string{"discarded-prefix", "-middle-", "wanted-tail"} {
		if n, err := buffer.Write([]byte(value)); err != nil || n != len(value) {
			t.Fatalf("Write(%q) = %d, %v", value, n, err)
		}
	}
	if got, want := buffer.String(), "ddle-wanted-tail"; got != want {
		t.Fatalf("bounded tail = %q, want %q", got, want)
	}
	if len(buffer.buf) != 16 || cap(buffer.buf) != 16 {
		t.Fatalf("bounded tail len/cap = %d/%d, want 16/16", len(buffer.buf), cap(buffer.buf))
	}
}

func TestVideoInternalDeadlineStopsContextBlockingScreenshotAndEncoder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg fixture is a POSIX shell script")
	}
	pidPath := filepath.Join(t.TempDir(), "ffmpeg.pid")
	ffmpeg := writeExecutableFixture(t, `#!/bin/sh
printf '%s\n' "$$" > "$BRW_VIDEO_TEST_PID"
exec sleep 60
`)
	t.Setenv("BRW_FFMPEG_PATH", ffmpeg)
	t.Setenv("BRW_VIDEO_TEST_PID", pidPath)

	deadlineSeen := make(chan time.Time, 1)
	release := make(chan struct{})
	returned := make(chan struct{}, 1)
	fake := &deadlineVideoBrowser{
		serviceFakeBrowser: serviceFakeBrowser{read: readability.PageRead{URL: "https://example.test", Title: "Video"}},
		deadlineSeen:       deadlineSeen,
		release:            release,
		returned:           returned,
	}
	store := newTestStore(t, 4<<20, 8<<20)
	service, err := NewService(store, fake)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "video", DurationMS: 100, FPS: 1})
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "bounded runtime") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context-blocked screenshot error = %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("context-blocked screenshot returned after %s, want under 5s", elapsed)
	}
	select {
	case deadline := <-deadlineSeen:
		remaining := time.Until(deadline) + elapsed
		if remaining < 1800*time.Millisecond || remaining > 3*time.Second {
			t.Fatalf("screenshot context budget was approximately %s", remaining)
		}
	default:
		t.Fatal("screenshot did not receive the internal deadline")
	}
	close(release)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("released screenshot fixture did not return")
	}
	assertFixtureProcessStopped(t, pidPath)
	assertNoVideoTemps(t, store.Root())
}

func TestVideoInternalDeadlineTerminatesHangingEncoderAndCleansTemp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg fixture is a POSIX shell script")
	}
	pidPath := filepath.Join(t.TempDir(), "ffmpeg.pid")
	ffmpeg := writeExecutableFixture(t, `#!/bin/sh
printf '%s\n' "$$" > "$BRW_VIDEO_TEST_PID"
cat >/dev/null
exec sleep 60
`)
	t.Setenv("BRW_FFMPEG_PATH", ffmpeg)
	t.Setenv("BRW_VIDEO_TEST_PID", pidPath)
	pngBytes := onePixelPNG(t)
	fake := serviceFakeBrowser{
		read:       readability.PageRead{URL: "https://example.test", Title: "Video"},
		screenshot: browser.Screenshot{MIMEType: "image/png", Data: pngBytes},
	}
	store := newTestStore(t, 4<<20, 8<<20)
	service, err := NewService(store, fake)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "video", DurationMS: 100, FPS: 1})
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("hanging encoder returned after %s, want under 5s", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "bounded runtime") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hanging encoder error = %v", err)
	}
	assertFixtureProcessStopped(t, pidPath)
	assertNoVideoTemps(t, store.Root())
}

func TestVideoNoisyEncoderHasBoundedDiagnosticAndCleansTemp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg fixture is a POSIX shell script")
	}
	ffmpeg := writeExecutableFixture(t, `#!/bin/sh
cat >/dev/null
i=0
while [ "$i" -lt 8192 ]; do
  printf '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef' >&2
  i=$((i + 1))
done
printf 'TAIL_SENTINEL\n' >&2
exit 23
`)
	t.Setenv("BRW_FFMPEG_PATH", ffmpeg)
	pngBytes := onePixelPNG(t)
	fake := serviceFakeBrowser{
		read:       readability.PageRead{URL: "https://example.test", Title: "Video"},
		screenshot: browser.Screenshot{MIMEType: "image/png", Data: pngBytes},
	}
	store := newTestStore(t, 4<<20, 8<<20)
	service, err := NewService(store, fake)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "video", DurationMS: 100, FPS: 1})
	if err == nil || !strings.Contains(err.Error(), "TAIL_SENTINEL") {
		t.Fatalf("noisy encoder error = %v", err)
	}
	if len(err.Error()) > 1200 {
		t.Fatalf("noisy encoder diagnostic grew to %d bytes", len(err.Error()))
	}
	assertNoVideoTemps(t, store.Root())
}

func writeExecutableFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertFixtureProcessStopped(t *testing.T, pidPath string) {
	t.Helper()
	data, err := os.ReadFile(pidPath)
	// Under a heavily contended race run CommandContext can terminate the child
	// after Start succeeds but before the shell executes its first instruction.
	// Wait has already returned in that case, so no fixture process can remain.
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("read fake ffmpeg pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse fake ffmpeg pid: %v", err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := process.Signal(os.Kill); err == nil {
		t.Fatalf("fake ffmpeg process %d survived CaptureArtifact return", pid)
	}
}

func assertNoVideoTemps(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".video-") {
			t.Fatalf("temporary video payload left behind: %s", entry.Name())
		}
	}
}

func TestResolveFFmpegPathHonorsAbsoluteOverride(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRW_FFMPEG_PATH", executable)
	got, err := resolveFFmpegPath()
	if err != nil {
		t.Fatal(err)
	}
	if got == "" || !filepath.IsAbs(got) {
		t.Fatalf("resolved path = %q", got)
	}
}

func TestResolveFFmpegPathRejectsRelativeOverride(t *testing.T) {
	t.Setenv("BRW_FFMPEG_PATH", "ffmpeg")
	if _, err := resolveFFmpegPath(); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("relative override was not rejected: %v", err)
	}
}

func onePixelPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func FuzzArtifactIDConfinement(f *testing.F) {
	f.Add("../../etc/passwd")
	f.Add("art_00000000000000000000000000000000")
	f.Add("\x00")
	f.Fuzz(func(t *testing.T, id string) {
		if err := validateID(id); err == nil && !artifactIDPattern.MatchString(id) {
			t.Fatalf("validator accepted non-matching id %q", id)
		}
	})
}
