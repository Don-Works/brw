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

// TestIdenticalCapturesShareOneBlobWithIndependentHandles is the whole dedup
// contract in one place: same bytes stored once, handles that stay separate
// objects with separate lifetimes, and a quota that is charged once.
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

	// Two handles, one payload: the quota must see one payload. Charging twice
	// would shrink the store by bytes that were never written.
	live, _, err := store.scanLocked()
	if err != nil {
		t.Fatal(err)
	}
	if used := store.bytesUsedLocked(live); used != int64(len(payload)) {
		t.Fatalf("quota charge = %d bytes, want %d for one shared payload", used, len(payload))
	}

	// Expiry is per handle, not per payload.
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

// TestDeduplicatedHandlesStayUnlinkableAndNonEnumerable proves dedup did not
// turn the store into an oracle. It asserts against blobs that really are
// shared — remove the hard link and the shared-inode precondition fails — so it
// can detect the dedup code regressing, not only newID staying random.
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

	// The precondition: these handles really do share one payload, so everything
	// below is asserted about deduplicated blobs and not about unrelated ones.
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

	// A shared payload must not give one handle a route to the others: the
	// on-disk metadata carries the digest and nothing that names another handle.
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

	// Nothing on disk is addressed by content, so the directory listing itself
	// cannot answer "have these bytes been captured?".
	entries, err := os.ReadDir(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), contentAddress[:16]) {
			t.Fatalf("store file %q is named after the content address", entry.Name())
		}
	}

	// The obvious guesses a content-addressed store would answer.
	for _, guess := range []string{
		"art_" + contentAddress[:32],
		"art_" + contentAddress[32:],
	} {
		if _, err := store.Info(guess); err == nil {
			t.Fatalf("content-derived id %q resolved to an artifact", guess)
		}
	}
}

// TestQuotaChargesPayloadsThatAreNotActuallyShared covers the case dedup does
// not reach: two real files with the same digest, which is what a failed
// os.Link leaves behind and what a store written before dedup is full of.
// Charging those once would let the store accept writes past its total.
func TestQuotaChargesPayloadsThatAreNotActuallyShared(t *testing.T) {
	payload := bytes.Repeat([]byte("unshared-copy\n"), 64)
	const copies = 2
	// Room for the two unshared copies and nothing more.
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

	// And the miscount has to be visible where it matters: the store is full.
	if _, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"},
		bytes.NewReader(bytes.Repeat([]byte("other\n"), 64))); err == nil ||
		!strings.Contains(err.Error(), "quota") {
		t.Fatalf("a full store accepted another payload: %v", err)
	}
}

// cloneArtifactAsSeparateFile writes a second handle over the same bytes as a
// real second file, which is what a filesystem without hard links, or any brw
// that predates dedup, leaves in the store.
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

// TestOrphanBlobsStillOccupyTheQuota keeps a crash from hiding bytes. A blob
// whose metadata never landed is real disk inside its reconciliation grace
// window, and a quota that cannot see it can be walked past one crash at a time.
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

// TestFullStoreStillAcceptsBytesItAlreadyHolds is the other side of charging by
// identity: a capture that will be a hard link costs nothing, so the quota must
// not reject it on a store with no room for a second copy.
func TestFullStoreStillAcceptsBytesItAlreadyHolds(t *testing.T) {
	payload := bytes.Repeat([]byte("repeat-capture\n"), 128)
	store := newTestStore(t, int64(len(payload)), int64(len(payload))+16)

	first, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	// Nothing else fits.
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

// TestPutDoesNotHoldTheStoreLockAcrossItsSource is what moving the copy out of
// the critical section buys. A capture source is a browser round trip, and an
// Info or Delete must not wait behind one.
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

// gatedReader reports that it has been read from, then blocks until released,
// standing in for a browser that has not finished answering.
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

// TestStagingSurvivesAConcurrentOrphanSweep guards the consequence of copying
// outside the lock: another capture's reconciliation pass must not reclaim a
// staging file that is still being written, however long the transfer takes.
func TestStagingSurvivesAConcurrentOrphanSweep(t *testing.T) {
	store := newTestStore(t, 1<<20, 8<<20)
	source := &gatedReader{started: make(chan struct{}), release: make(chan struct{}), body: []byte("long transfer body")}
	captured := make(chan error, 1)
	go func() {
		_, putErr := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, source)
		captured <- putErr
	}()
	<-source.started

	// Age the staging file well past orphanGrace, which is what a transfer slower
	// than five minutes looks like to the sweep.
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
