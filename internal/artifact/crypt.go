package artifact

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// At-rest artifact encryption.
//
// The blob format is chunked AEAD rather than one sealed buffer, because a
// capture is written and read as a stream: a 50 MiB PDF must never be held whole
// in the daemon heap to be encrypted, and a bounded artifact read must decrypt
// only the window it was asked for instead of the whole file.
//
//	header: magic(8) || salt(16)
//	body:   repeat( AES-256-GCM(plaintext chunk) || tag(16) )
//
// The file key is derived per blob from the store key and the random salt, so
// one key can protect any number of artifacts without a nonce-reuse budget. The
// nonce is the chunk counter, and the FINAL chunk sets the high bit: truncating
// a blob therefore fails authentication instead of silently returning a prefix.
// A zero-length payload still writes one final (empty) chunk, so every blob has
// an authenticated end.
const (
	blobChunkSize       = 64 << 10
	blobTagSize         = 16
	blobSaltSize        = 16
	blobMagicSize       = 8
	blobHeaderSize      = blobMagicSize + blobSaltSize
	blobCipherChunkSize = blobChunkSize + blobTagSize
	blobFinalChunkFlag  = uint64(1) << 63
	blobKeyInfo         = "brw artifact blob v1"

	// MinEncryptionKeyBytes is the shortest operator-supplied key the store
	// accepts. The key is stretched by HKDF, so this is about the entropy an
	// operator must actually provide, not about the AES key length.
	MinEncryptionKeyBytes = 32
)

var blobMagic = [blobMagicSize]byte{'b', 'r', 'w', 'e', 'n', 'c', '0', '1'}

func normalizeEncryptionKey(key []byte) ([]byte, error) {
	if len(key) == 0 {
		return nil, nil
	}
	if len(key) < MinEncryptionKeyBytes {
		return nil, fmt.Errorf("artifact encryption key must be at least %d bytes", MinEncryptionKeyBytes)
	}
	return append([]byte(nil), key...), nil
}

// EncryptionAvailable reports whether this store can encrypt at rest, so a
// daemon can refuse an encryption policy at startup rather than at the first
// failed capture.
func (s *Store) EncryptionAvailable() bool { return len(s.key) > 0 }

func blobAEAD(storeKey, salt []byte) (cipher.AEAD, error) {
	fileKey, err := hkdf.Key(sha256.New, storeKey, salt, blobKeyInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(fileKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func blobNonce(index uint64, final bool) []byte {
	nonce := make([]byte, 12)
	counter := index
	if final {
		counter |= blobFinalChunkFlag
	}
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce
}

// blobChunkCount is the number of sealed chunks a payload of size bytes
// occupies, including the always-present final chunk.
func blobChunkCount(size int64) int64 { return size/blobChunkSize + 1 }

type blobEncrypter struct {
	dst    io.Writer
	aead   cipher.AEAD
	buf    []byte
	sealed []byte
	index  uint64
	closed bool
}

func newBlobEncrypter(dst io.Writer, storeKey []byte) (*blobEncrypter, error) {
	if dst == nil {
		return nil, errors.New("artifact encryption destination is nil")
	}
	salt := make([]byte, blobSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, err := blobAEAD(storeKey, salt)
	if err != nil {
		return nil, err
	}
	if _, err := dst.Write(blobMagic[:]); err != nil {
		return nil, err
	}
	if _, err := dst.Write(salt); err != nil {
		return nil, err
	}
	return &blobEncrypter{
		dst:    dst,
		aead:   aead,
		buf:    make([]byte, 0, blobChunkSize),
		sealed: make([]byte, 0, blobCipherChunkSize),
	}, nil
}

func (e *blobEncrypter) Write(p []byte) (int, error) {
	if e.closed {
		return 0, errors.New("artifact encryption writer is closed")
	}
	total := len(p)
	for len(p) > 0 {
		room := blobChunkSize - len(e.buf)
		take := min(room, len(p))
		e.buf = append(e.buf, p[:take]...)
		p = p[take:]
		if len(e.buf) == blobChunkSize {
			if err := e.flush(false); err != nil {
				return total - len(p), err
			}
		}
	}
	return total, nil
}

func (e *blobEncrypter) flush(final bool) error {
	e.sealed = e.aead.Seal(e.sealed[:0], blobNonce(e.index, final), e.buf, nil)
	if _, err := e.dst.Write(e.sealed); err != nil {
		return err
	}
	e.buf = e.buf[:0]
	e.index++
	return nil
}

// Close seals the trailing partial chunk. It is part of the format, not a
// courtesy: without the final-flagged chunk a decrypter cannot tell a complete
// blob from a truncated one.
func (e *blobEncrypter) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	return e.flush(true)
}

type blobDecrypter struct {
	src    *os.File
	aead   cipher.AEAD
	size   int64
	chunks int64
	pos    int64

	chunk      []byte
	chunkIndex int64
	chunkValid bool
	cipherBuf  []byte
}

func newBlobDecrypter(src *os.File, storeKey []byte, size int64) (*blobDecrypter, error) {
	if src == nil {
		return nil, errors.New("artifact decryption source is nil")
	}
	if size < 0 {
		return nil, errors.New("artifact plaintext size is negative")
	}
	header := make([]byte, blobHeaderSize)
	if _, err := io.ReadFull(io.NewSectionReader(src, 0, blobHeaderSize), header); err != nil {
		return nil, errors.New("encrypted artifact header is unreadable")
	}
	if string(header[:blobMagicSize]) != string(blobMagic[:]) {
		return nil, errors.New("encrypted artifact has an unrecognised format")
	}
	aead, err := blobAEAD(storeKey, header[blobMagicSize:])
	if err != nil {
		return nil, err
	}
	return &blobDecrypter{
		src: src, aead: aead, size: size, chunks: blobChunkCount(size),
		chunkIndex: -1,
	}, nil
}

func (d *blobDecrypter) load(index int64) error {
	if d.chunkValid && d.chunkIndex == index {
		return nil
	}
	if index < 0 || index >= d.chunks {
		return io.EOF
	}
	plainLen := int64(blobChunkSize)
	if index == d.chunks-1 {
		plainLen = d.size % blobChunkSize
	}
	cipherLen := plainLen + blobTagSize
	if int64(cap(d.cipherBuf)) < cipherLen {
		d.cipherBuf = make([]byte, cipherLen)
	}
	buf := d.cipherBuf[:cipherLen]
	offset := int64(blobHeaderSize) + index*blobCipherChunkSize
	if _, err := io.ReadFull(io.NewSectionReader(d.src, offset, cipherLen), buf); err != nil {
		return errors.New("encrypted artifact chunk is unreadable")
	}
	plain, err := d.aead.Open(d.chunk[:0], blobNonce(uint64(index), index == d.chunks-1), buf, nil)
	if err != nil {
		return errors.New("encrypted artifact failed authentication")
	}
	d.chunk = plain
	d.chunkIndex = index
	d.chunkValid = true
	return nil
}

func (d *blobDecrypter) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if d.pos >= d.size {
		return 0, io.EOF
	}
	if err := d.load(d.pos / blobChunkSize); err != nil {
		return 0, err
	}
	start := int(d.pos % blobChunkSize)
	n := copy(p, d.chunk[start:])
	d.pos += int64(n)
	return n, nil
}

func (d *blobDecrypter) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = d.pos + offset
	case io.SeekEnd:
		target = d.size + offset
	default:
		return 0, errors.New("invalid seek mode for an encrypted artifact")
	}
	if target < 0 {
		return 0, errors.New("negative seek in an encrypted artifact")
	}
	d.pos = target
	return target, nil
}

func (d *blobDecrypter) Close() error { return d.src.Close() }

// LoadEncryptionKey reads the operator's artifact key.
//
// The key is never generated by brw and never stored beside the ciphertext: a
// key file inside the artifact root would encrypt the artifacts against nobody,
// and is refused. The file must also be unreadable by group and other, because
// a key any local account can read protects against nothing a local account
// could not already do.
func LoadEncryptionKey(path, artifactRoot string) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("artifact encryption key path is empty")
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("artifact encryption key path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("artifact encryption key must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("artifact encryption key %s is readable beyond its owner", filepath.Base(path))
	}
	if err := rejectKeyInsideRoot(path, artifactRoot); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key := bytes.TrimRight(raw, " \t\r\n")
	if len(key) < MinEncryptionKeyBytes {
		return nil, fmt.Errorf("artifact encryption key must carry at least %d bytes of key material", MinEncryptionKeyBytes)
	}
	return key, nil
}

func rejectKeyInsideRoot(path, artifactRoot string) error {
	artifactRoot = strings.TrimSpace(artifactRoot)
	if artifactRoot == "" {
		return nil
	}
	resolvedKey := resolvePathPrefix(filepath.Dir(path))
	resolvedRoot := resolvePathPrefix(artifactRoot)
	relative, err := filepath.Rel(resolvedRoot, resolvedKey)
	if err != nil {
		return nil
	}
	if relative == "." || !strings.HasPrefix(relative, "..") {
		return errors.New("artifact encryption key must not live inside the artifact root")
	}
	return nil
}

func resolvePathPrefix(path string) string {
	if resolved, err := resolveArtifactRoot(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}
