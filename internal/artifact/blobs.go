package artifact

import (
	"errors"
	"io"
	"os"
)

// commitBlobLocked installs a finished temporary file as the handle's blob.
//
// Identical payloads are stored once. The second capture of the same bytes gets
// a hard link to the copy that is already there, so N captures of one 50 MiB
// download occupy 50 MiB rather than N*50 MiB. Every other property of a handle
// is unaffected: the id stays a fresh 128-bit random value (dedup must not turn
// the store into an oracle that answers "does this content exist?" from a
// guessable id), each handle keeps its own metadata and its own expiry, and
// unlinking one name leaves the other readable because the kernel, not brw,
// owns the reference count.
//
// Encrypted blobs are never deduplicated. Recognising a duplicate requires
// producing identical ciphertext for identical plaintext, and that equivalence
// is exactly what encrypting a sensitive capture is supposed to hide — which is
// also why an encrypted handle records no plaintext digest, so dedupSource can
// never find one.
func (s *Store) commitBlobLocked(tmpName, id, source string) error {
	target := s.blobPath(id)
	if source != "" {
		if err := os.Link(source, target); err == nil {
			return os.Remove(tmpName)
		}
		// A filesystem without hard links, or a source removed by a concurrent
		// daemon between the scan and here, simply costs a second copy — which
		// bytesUsedLocked charges, because it counts inodes rather than digests.
	}
	return os.Rename(tmpName, target)
}

// dedupSource names a live blob holding exactly these bytes, or reports that
// there is none. An empty digest (every encrypted blob) never matches.
func dedupSource(s *Store, digest string, live map[string]Meta) (string, bool) {
	if digest == "" {
		return "", false
	}
	for id, meta := range live {
		if meta.Encrypted || meta.SHA256 != digest {
			continue
		}
		path := s.blobPath(id)
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			return path, true
		}
	}
	return "", false
}

// openPayload returns a reader over an artifact's plaintext, decrypting on the
// way out when the blob is encrypted at rest. Callers get identical semantics
// either way, which is why encryption can be a per-artifact decision.
func (s *Store) openPayload(id string, meta Meta) (io.ReadSeekCloser, error) {
	file, err := s.openBlob(id)
	if err != nil {
		return nil, err
	}
	if !meta.Encrypted {
		return file, nil
	}
	if len(s.key) == 0 {
		_ = file.Close()
		return nil, errors.New("artifact is encrypted but this store has no encryption key")
	}
	reader, err := newBlobDecrypter(file, s.key, meta.SizeBytes)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return reader, nil
}
