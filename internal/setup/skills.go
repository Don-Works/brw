package setup

import (
	"io/fs"
	"os"
	"path/filepath"
)

// SkillDestinations are the global skill directories the common agent harnesses
// discover. One copy per harness, all under the user's home.
func SkillDestinations(home string) []string {
	return []string{
		filepath.Join(home, ".claude", "skills", "brw"),
		filepath.Join(home, ".agents", "skills", "brw"),
		filepath.Join(home, ".codex", "skills", "brw"),
	}
}

// CopyTree mirrors an fs.FS into dst and reports whether anything on disk
// changed. Identical content is left alone so a re-run can honestly say it did
// nothing, and files the source no longer has are removed so a stale skill page
// cannot linger as instructions an agent still reads.
//
// The source is an fs.FS rather than a directory path because the skill brwctl
// installs comes out of the binary (see internal/agentskill). Hunting for a
// directory next to the executable is what let the copy on disk and the daemon
// serving it drift apart in the first place: whichever brw ran setup decided
// what the page said, for every brw afterwards.
func CopyTree(src fs.FS, dst string) (changed bool, err error) {
	wanted := map[string]bool{}
	err = fs.WalkDir(src, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := filepath.Join(dst, filepath.FromSlash(path))
		if path != "." {
			wanted[filepath.Clean(target)] = true
		}
		if entry.IsDir() {
			if _, statErr := os.Stat(target); os.IsNotExist(statErr) {
				changed = true
			}
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		if existing, readErr := os.ReadFile(target); readErr == nil && string(existing) == string(data) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return changed, err
	}

	var stale []string
	err = filepath.Walk(dst, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filepath.Clean(path) == filepath.Clean(dst) {
			return nil
		}
		if !wanted[filepath.Clean(path)] {
			stale = append(stale, path)
			if info.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		return changed, err
	}
	for _, path := range stale {
		if err := os.RemoveAll(path); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}
