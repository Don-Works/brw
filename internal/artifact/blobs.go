package artifact

import (
	"errors"
	"io"
	"os"
)

func (s *Store) commitBlobLocked(tmpName, id, source string) error {
	target := s.blobPath(id)
	if source != "" {
		if err := os.Link(source, target); err == nil {
			return os.Remove(tmpName)
		}

	}
	return os.Rename(tmpName, target)
}

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
