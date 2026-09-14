package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
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
	if used := bytesUsed(live); used != int64(len(payload)) {
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

// TestArtifactIDsRemainNonEnumerableAfterDedup proves dedup did not turn the
// store into an oracle. A handle id must stay unguessable and unrelated to the
// content it points at, or anyone who can guess a payload could ask the store
// whether it has been captured.
func TestArtifactIDsRemainNonEnumerableAfterDedup(t *testing.T) {
	store := newTestStore(t, 1<<20, 8<<20)
	payload := []byte("shared-capture-body")
	digest := sha256.Sum256(payload)
	contentAddress := hex.EncodeToString(digest[:])

	const captures = 32
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
		body := strings.TrimPrefix(meta.ID, "art_")
		if strings.Contains(contentAddress, body) || strings.HasPrefix(contentAddress, body[:8]) {
			t.Fatalf("handle id %q is derived from the content address %s", meta.ID, contentAddress)
		}
	}
	if len(seen) != captures {
		t.Fatalf("distinct ids = %d, want %d", len(seen), captures)
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
