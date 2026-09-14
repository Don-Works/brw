package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureStoreKey is deliberately a flat repeated byte. It is key-shaped, has
// no entropy, and cannot be mistaken for a real key that escaped into the tree.
func fixtureStoreKey() []byte { return bytes.Repeat([]byte{0x2a}, MinEncryptionKeyBytes) }

func newEncryptedTestStore(t *testing.T, maxArtifact, maxTotal int64) *Store {
	t.Helper()
	store, err := NewStore(Config{
		Root: t.TempDir(), MaxArtifactBytes: maxArtifact, MaxTotalBytes: maxTotal,
		TTL: time.Hour, EncryptionKey: fixtureStoreKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestEncryptedArtifactRoundTripsWithNoPlaintextOnDisk drives the real write and
// read paths at every interesting size around the chunk boundary, and checks the
// property that justifies the feature: the payload never lands on disk in the
// clear, not even transiently while the capture is being written.
func TestEncryptedArtifactRoundTripsWithNoPlaintextOnDisk(t *testing.T) {
	const sentinel = "encrypted-capture-sentinel"
	tests := []struct {
		name string
		size int
	}{
		{name: "empty", size: 0},
		{name: "single partial chunk", size: 1000},
		{name: "exact chunk boundary", size: blobChunkSize},
		{name: "chunk boundary plus one", size: blobChunkSize + 1},
		{name: "several chunks", size: 3*blobChunkSize + 77},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newEncryptedTestStore(t, 8<<20, 16<<20)
			payload := make([]byte, test.size)
			for index := range payload {
				payload[index] = sentinel[index%len(sentinel)]
			}
			meta, err := store.PutContext(context.Background(),
				PutOptions{Kind: "text", MIMEType: "text/plain", Encrypt: true}, bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			if !meta.Encrypted || meta.SizeBytes != int64(len(payload)) {
				t.Fatalf("meta = %+v", meta)
			}
			wantStored := int64(blobHeaderSize) + int64(len(payload)) + blobChunkCount(int64(len(payload)))*blobTagSize
			if meta.storedSize() != wantStored {
				t.Fatalf("stored size = %d, want %d", meta.storedSize(), wantStored)
			}

			if len(payload) > 0 {
				assertNoPlaintextOnDisk(t, store.Root(), sentinel)
			}

			// Whole-payload read back through the public bounded-read path.
			var got []byte
			for offset := int64(0); ; {
				window, _, more, err := store.Read(meta.ID, offset, 4096)
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, window...)
				if !more {
					break
				}
				offset += int64(len(window))
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("round trip returned %d bytes, want %d", len(got), len(payload))
			}

			// An offset read must decrypt only the window it was asked for and
			// still land on the right bytes.
			if len(payload) > blobChunkSize+10 {
				offset := int64(blobChunkSize + 5)
				window, _, _, err := store.Read(meta.ID, offset, 32)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(window, payload[offset:offset+32]) {
					t.Fatalf("offset read = %q, want %q", window, payload[offset:offset+32])
				}
			}
		})
	}
}

// assertNoPlaintextOnDisk walks every file the store owns, temporary files
// included. It is called after the write as well as from inside it — see
// TestEncryptedCaptureIsNeverStagedInTheClear, which scans the root while the
// capture is mid-copy, because "encrypted at rest" is worth nothing if the
// plaintext is staged in the clear first and only encrypted on commit.
func assertNoPlaintextOnDisk(t *testing.T, root, sentinel string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(sentinel)) {
			t.Fatalf("plaintext sentinel found on disk in %s", entry.Name())
		}
	}
}

// TestEncryptedArtifactSearchAndTamperDetection covers the two things an
// authenticated format buys: search still works through the decrypting reader,
// and a modified or truncated blob is refused rather than partially returned.
func TestEncryptedArtifactSearchAndTamperDetection(t *testing.T) {
	store := newEncryptedTestStore(t, 1<<20, 4<<20)
	payload := "line one\nInvoice_2026_09 paid\nline three\n"
	meta, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain", Encrypt: true}, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.SearchText(meta.ID, "invoice_2026_09", 5)
	if err != nil || len(hits) != 1 || hits[0].Line != 2 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}

	blob := store.blobPath(meta.ID)
	raw, err := os.ReadFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		mutated []byte
	}{
		{name: "flipped ciphertext byte", mutated: flipByte(raw, blobHeaderSize+1)},
		{name: "flipped salt byte", mutated: flipByte(raw, blobMagicSize)},
		{name: "truncated tail", mutated: raw[:len(raw)-1]},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(blob, test.mutated, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.WriteFile(blob, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			})
			if _, _, _, err := store.Read(meta.ID, 0, 64); err == nil {
				t.Fatal("tampered encrypted artifact was read without error")
			}
		})
	}
}

func flipByte(data []byte, index int) []byte {
	out := append([]byte(nil), data...)
	out[index] ^= 0xff
	return out
}

// TestEncryptionRefusedWithoutAKey keeps the failure loud. Silently storing a
// capture in the clear because no key was configured is the one outcome an
// operator who asked for encryption must never get.
func TestEncryptionRefusedWithoutAKey(t *testing.T) {
	store := newTestStore(t, 1<<20, 2<<20)
	if store.EncryptionAvailable() {
		t.Fatal("plain store reported an encryption key")
	}
	if _, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain", Encrypt: true}, strings.NewReader("x")); err == nil {
		t.Fatal("encryption without a key was accepted")
	}
	if _, err := NewStore(Config{Root: t.TempDir(), TTL: time.Hour, EncryptionKey: []byte("too-short")}); err == nil {
		t.Fatal("short encryption key was accepted")
	}
}

func TestLoadEncryptionKeyRejectsUnsafeSources(t *testing.T) {
	artifactRoot := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	keyDir := t.TempDir()
	good := filepath.Join(keyDir, "artifact.key")
	if err := os.WriteFile(good, fixtureStoreKey(), 0o600); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(keyDir, "short.key")
	if err := os.WriteFile(short, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(keyDir, "loose.key")
	if err := os.WriteFile(loose, fixtureStoreKey(), 0o644); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(artifactRoot, "artifact.key")
	if err := os.WriteFile(inside, fixtureStoreKey(), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "owner-only key outside the root", path: good},
		{name: "relative path", path: "artifact.key", wantErr: true},
		{name: "too little key material", path: short, wantErr: true},
		{name: "readable by group and other", path: loose, wantErr: true},
		{name: "stored inside the artifact root", path: inside, wantErr: true},
		{name: "missing", path: filepath.Join(keyDir, "absent.key"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key, err := LoadEncryptionKey(test.path, artifactRoot)
			if test.wantErr {
				if err == nil {
					t.Fatalf("accepted %s", test.name)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(key, fixtureStoreKey()) {
				t.Fatalf("key = %q", key)
			}
		})
	}
}

// TestEncryptedCaptureIsNeverStagedInTheClear asserts the transient half of the
// at-rest claim. The scan runs from inside the source reader, so the store is
// caught mid-copy with its staging file open and unrenamed — the one moment a
// plaintext staging bug would be visible, and the moment every after-the-fact
// scan misses.
func TestEncryptedCaptureIsNeverStagedInTheClear(t *testing.T) {
	const sentinel = "mid-copy-plaintext-sentinel"
	store := newEncryptedTestStore(t, 8<<20, 16<<20)
	payload := bytes.Repeat([]byte(sentinel), 4096/len(sentinel)+1)

	scans := 0
	source := &probeReader{
		body: payload,
		// Small enough that the scan happens with most of the payload still to
		// come, so the staging file is genuinely partial and still open.
		chunk: 1024,
		probe: func() {
			scans++
			assertNoPlaintextOnDisk(t, store.Root(), sentinel)
			assertStagingFileExists(t, store.Root())
		},
	}
	meta, err := store.PutContext(context.Background(),
		PutOptions{Kind: "text", MIMEType: "text/plain", Encrypt: true}, source)
	if err != nil {
		t.Fatal(err)
	}
	if scans < 2 {
		t.Fatalf("the store was only caught mid-copy %d times; the probe is not exercising the staging path", scans)
	}
	if !meta.Encrypted || meta.SizeBytes != int64(len(payload)) {
		t.Fatalf("meta = %+v", meta)
	}
	assertNoPlaintextOnDisk(t, store.Root(), sentinel)
}

// probeReader hands the store one bounded chunk at a time and runs probe before
// each one, so an assertion can observe the store's directory part-way through a
// capture rather than only after it.
type probeReader struct {
	body   []byte
	chunk  int
	offset int
	probe  func()
}

func (r *probeReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.body) {
		return 0, io.EOF
	}
	r.probe()
	n := copy(p, r.body[r.offset:min(len(r.body), r.offset+r.chunk)])
	r.offset += n
	return n, nil
}

func assertStagingFileExists(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".artifact-") {
			return
		}
	}
	t.Fatal("no staging file mid-capture, so the scan above proves nothing")
}

// TestEncryptedArtifactRecordsNoPlaintextDigest closes the oracle the ciphertext
// exists to shut. A plaintext SHA-256 sitting beside the blob lets anyone who can
// read the artifact root confirm a payload they can guess, and lets them see that
// two encrypted artifacts hold the same bytes — the equivalence that keeps
// encrypted blobs out of dedup in the first place.
func TestEncryptedArtifactRecordsNoPlaintextDigest(t *testing.T) {
	store := newEncryptedTestStore(t, 1<<20, 8<<20)
	payload := []byte("guessable-encrypted-payload")
	digest := sha256.Sum256(payload)
	contentAddress := hex.EncodeToString(digest[:])

	first, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain", Encrypt: true}, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain", Encrypt: true}, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	for _, meta := range []Meta{first, second} {
		if meta.SHA256 != "" {
			t.Fatalf("encrypted artifact %s carries a plaintext digest %q", meta.ID, meta.SHA256)
		}
		stored, infoErr := store.Info(meta.ID)
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if stored.SHA256 != "" {
			t.Fatalf("artifact info returned a plaintext digest for encrypted artifact %s", meta.ID)
		}
	}
	// Nothing on disk answers "are these the bytes?" either.
	assertNoPlaintextOnDisk(t, store.Root(), contentAddress)

	// The two encrypted copies must not be linkable to each other, and must not
	// have been deduplicated into one blob.
	firstInfo, err := os.Stat(store.blobPath(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(store.blobPath(second.ID))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(firstInfo, secondInfo) {
		t.Fatal("two encrypted captures share a blob, which is the equality encryption is meant to hide")
	}

	// An unencrypted capture still records its digest: the assertions above are
	// about ciphertext, not about the digest disappearing everywhere.
	plain, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if plain.SHA256 != contentAddress {
		t.Fatalf("unencrypted digest = %q, want %q", plain.SHA256, contentAddress)
	}
}
