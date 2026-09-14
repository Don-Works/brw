package setup

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// FindSkillSource locates the shipped skills/brw directory: the app directory a
// package installer or `task install-mac` wrote, the platform share directory a
// native installer wrote, the directory the running brwctl sits in, or the
// working directory of a source checkout. The first that holds a SKILL.md wins.
func FindSkillSource(appDir, executable, workingDir string) (string, error) {
	var candidates []string
	if appDir != "" {
		candidates = append(candidates, filepath.Join(appDir, "skills", "brw"))
	}
	if executable != "" {
		binDir := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(binDir, "..", "skills", "brw"),
			filepath.Join(binDir, "..", "share", "brw", "skills", "brw"),
		)
	}
	switch runtime.GOOS {
	case "darwin":
		candidates = append(candidates, filepath.Join("/usr", "local", "share", "brw", "skills", "brw"))
	case "windows":
	default:
		candidates = append(candidates, filepath.Join("/usr", "share", "brw", "skills", "brw"))
	}
	if workingDir != "" {
		candidates = append(candidates, filepath.Join(workingDir, "skills", "brw"))
	}
	for _, candidate := range dedupeStrings(candidates) {
		clean := filepath.Clean(candidate)
		if info, err := os.Stat(filepath.Join(clean, "SKILL.md")); err == nil && info.Mode().IsRegular() {
			return clean, nil
		}
	}
	return "", errors.New("no skills/brw directory found next to the brw install or in the working directory")
}

// CopyTree mirrors src into dst and reports whether anything on disk changed.
// Identical content is left alone so a re-run can honestly say it did nothing,
// and files the source no longer has are removed so a stale skill page cannot
// linger as instructions an agent still reads.
func CopyTree(src, dst string) (changed bool, err error) {
	wanted := map[string]bool{}
	err = filepath.Walk(src, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, relative)
		if relative != "." {
			wanted[filepath.Clean(target)] = true
		}
		if info.IsDir() {
			if _, statErr := os.Stat(target); os.IsNotExist(statErr) {
				changed = true
			}
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
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
