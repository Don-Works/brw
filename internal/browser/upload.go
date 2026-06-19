package browser

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Don-Works/brw/internal/snapshot"
)

func NormalizeUploadPaths(opts snapshot.UploadOptions) ([]string, error) {
	paths := make([]string, 0, len(opts.Paths)+1)
	if strings.TrimSpace(opts.Path) != "" {
		paths = append(paths, opts.Path)
	}
	paths = append(paths, opts.Paths...)
	if len(paths) == 0 {
		return nil, errors.New("path or paths is required")
	}

	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}
		if strings.HasPrefix(path, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("upload file %q: %w", abs, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("upload file %q is a directory", abs)
		}
		out = append(out, abs)
	}
	if len(out) == 0 {
		return nil, errors.New("path or paths is required")
	}
	return out, nil
}
