package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIdenticalCapturesShareOneBlobWithIndependentHandles(t *testing.T) {
	store := newTestStore(t, 1<<20, 4<<20)
	payload := bytes.Repeat([]byte("duplicate-capture-line\n"), 512)

	first, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain", TTL: time.Hour}, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain", TTL: 30 * time.Minute}, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("dedup collapsed two captures into one handle")
	}
	if first.SHA256 != second.SHA256 {
		t.Fatalf("sha mismatch for identical payloads: %s vs %s", first.SHA256, second.SHA256)
	}

	firstInfo, err := os.Stat(store.blobPath(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(store.blobPath(second.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Fatal("identical payloads were stored as two separate blobs")
	}

	live, _, err := store.scanLocked()
	if err != nil {
		t.Fatal(err)
	}
	if used := store.bytesUsedLocked(live); used != int64(len(payload)) {
		t.Fatalf("quota charge = %d bytes, want %d for one shared payload", used, len(payload))
	}

	if !second.ExpiresAt.Before(first.ExpiresAt) {
		t.Fatalf("handles share an expiry: %s vs %s", first.ExpiresAt, second.ExpiresAt)
	}

	if err := store.Delete(first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Info(first.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted handle still resolves: %v", err)
	}
	surviving, _, _, err := store.Read(second.ID, 0, len(payload))
	if err != nil {
		t.Fatalf("deleting one handle broke the other: %v", err)
	}
	if !bytes.Equal(surviving, payload) {
		t.Fatal("surviving handle returned different bytes after its twin was deleted")
	}
}

func TestDeduplicatedHandlesStayUnlinkableAndNonEnumerable(t *testing.T) {
	store := newTestStore(t, 1<<20, 8<<20)
	payload := []byte("shared-capture-body")
	digest := sha256.Sum256(payload)
	contentAddress := hex.EncodeToString(digest[:])

	const captures = 32
	metas := make([]Meta, 0, captures)
	seen := make(map[string]bool, captures)
	for range captures {
		meta, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		if meta.SHA256 != contentAddress {
			t.Fatalf("sha256 = %s, want %s", meta.SHA256, contentAddress)
		}
		if seen[meta.ID] {
			t.Fatalf("duplicate handle id %q", meta.ID)
		}
		seen[meta.ID] = true
		metas = append(metas, meta)
	}

	base, err := os.Stat(store.blobPath(metas[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, meta := range metas[1:] {
		info, statErr := os.Stat(store.blobPath(meta.ID))
		if statErr != nil {
			t.Fatal(statErr)
		}
		if !os.SameFile(base, info) {
			t.Fatalf("handle %s did not adopt the payload already in the store", meta.ID)
		}
	}

	for index, meta := range metas {
		raw, readErr := os.ReadFile(store.metaPath(meta.ID))
		if readErr != nil {
			t.Fatal(readErr)
		}
		for other, candidate := range metas {
			if other == index {
				continue
			}
			if bytes.Contains(raw, []byte(candidate.ID)) {
				t.Fatalf("metadata for %s names the handle it shares a blob with (%s)", meta.ID, candidate.ID)
			}
		}
	}

	entries, err := os.ReadDir(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), contentAddress[:16]) {
			t.Fatalf("store file %q is named after the content address", entry.Name())
		}
	}

	for _, guess := range []string{
		"art_" + contentAddress[:32],
		"art_" + contentAddress[32:],
	} {
		if _, err := store.Info(guess); err == nil {
			t.Fatalf("content-derived id %q resolved to an artifact", guess)
		}
	}
}

func TestQuotaChargesPayloadsThatAreNotActuallyShared(t *testing.T) {
	payload := bytes.Repeat([]byte("unshared-copy\n"), 64)
	const copies = 2

	store := newTestStore(t, int64(len(payload)), int64(copies*len(payload)+8))

	first, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	twin := cloneArtifactAsSeparateFile(t, store, first)

	firstInfo, err := os.Stat(store.blobPath(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	twinInfo, err := os.Stat(store.blobPath(twin))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(firstInfo, twinInfo) {
		t.Fatal("the fixture did not produce two separate files, so it proves nothing")
	}

	live, _, err := store.scanLocked()
	if err != nil {
		t.Fatal(err)
	}
	if used := store.bytesUsedLocked(live); used != int64(copies*len(payload)) {
		t.Fatalf("quota charge = %d bytes, want %d for two unshared copies of the same digest",
			used, copies*len(payload))
	}

	if _, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"},
		bytes.NewReader(bytes.Repeat([]byte("other\n"), 64))); err == nil ||
		!strings.Contains(err.Error(), "quota") {
		t.Fatalf("a full store accepted another payload: %v", err)
	}
}

func cloneArtifactAsSeparateFile(t *testing.T, store *Store, source Meta) string {
	t.Helper()
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.blobPath(source.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.blobPath(id), data, 0o600); err != nil {
		t.Fatal(err)
	}
	clone := source
	clone.ID = id
	encoded, err := json.Marshal(clone)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.metaPath(id), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOrphanBlobsStillOccupyTheQuota(t *testing.T) {
	store := newTestStore(t, 1<<20, 4<<20)
	orphan := bytes.Repeat([]byte("orphaned-capture\n"), 100)
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.blobPath(id), orphan, 0o600); err != nil {
		t.Fatal(err)
	}

	live, _, err := store.scanLocked()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("live artifacts = %d, want none; the fixture wrote no metadata", len(live))
	}
	if used := store.bytesUsedLocked(live); used != int64(len(orphan)) {
		t.Fatalf("quota charge = %d bytes, want %d for an unreferenced blob", used, len(orphan))
	}
}

func TestFullStoreStillAcceptsBytesItAlreadyHolds(t *testing.T) {
	payload := bytes.Repeat([]byte("repeat-capture\n"), 128)
	store := newTestStore(t, int64(len(payload)), int64(len(payload))+16)

	first, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"},
		bytes.NewReader(bytes.Repeat([]byte("different\n"), 128))); err == nil {
		t.Fatal("the store was supposed to be full")
	}

	second, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("a capture of bytes the store already holds was rejected: %v", err)
	}
	firstInfo, err := os.Stat(store.blobPath(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(store.blobPath(second.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Fatal("the admitted capture cost a second copy after all")
	}
}

func TestPutDoesNotHoldTheStoreLockAcrossItsSource(t *testing.T) {
	store := newTestStore(t, 1<<20, 8<<20)
	existing, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, strings.NewReader("already here"))
	if err != nil {
		t.Fatal(err)
	}

	source := &gatedReader{started: make(chan struct{}), release: make(chan struct{}), body: []byte("slow capture body")}
	captured := make(chan error, 1)
	go func() {
		_, putErr := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, source)
		captured <- putErr
	}()
	<-source.started

	maintenance := make(chan error, 1)
	go func() { maintenance <- store.Delete(existing.ID) }()
	select {
	case err := <-maintenance:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("delete blocked behind an in-flight capture's source")
	}

	close(source.release)
	if err := <-captured; err != nil {
		t.Fatal(err)
	}
}

type gatedReader struct {
	started  chan struct{}
	release  chan struct{}
	body     []byte
	offset   int
	signaled bool
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if !r.signaled {
		r.signaled = true
		close(r.started)
		<-r.release
	}
	if r.offset >= len(r.body) {
		return 0, io.EOF
	}
	n := copy(p, r.body[r.offset:])
	r.offset += n
	return n, nil
}

func TestStagingSurvivesAConcurrentOrphanSweep(t *testing.T) {
	store := newTestStore(t, 1<<20, 8<<20)
	source := &gatedReader{started: make(chan struct{}), release: make(chan struct{}), body: []byte("long transfer body")}
	captured := make(chan error, 1)
	go func() {
		_, putErr := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, source)
		captured <- putErr
	}()
	<-source.started

	staging := findStagingFile(t, store)
	old := time.Now().Add(-2 * orphanGrace)
	if err := os.Chtimes(staging, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, strings.NewReader("unrelated")); err != nil {
		t.Fatal(err)
	}

	close(source.release)
	if err := <-captured; err != nil {
		t.Fatalf("the in-flight capture lost its staging file: %v", err)
	}
}

func findStagingFile(t *testing.T, store *Store) string {
	t.Helper()
	entries, err := os.ReadDir(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".artifact-") {
			return filepath.Join(store.Root(), entry.Name())
		}
	}
	t.Fatal("no staging file while a capture is in flight")
	return ""
}
