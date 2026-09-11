package cdp

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func FindChrome(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}

	candidates := chromeCandidates()
	for _, candidate := range candidates {
		if filepath.IsAbs(candidate) {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}

	return "", errors.New("Chrome/Chromium executable not found; pass --chrome-path")
}

// Candidates is the ordered list FindChrome walks when no path is given. Chrome
// and Chromium lead, so a machine that has them keeps the binary it always
// picked; the rest are there so a machine with only a Chromium fork on it still
// starts. Every browser in setup's table appears here, which a test in that
// package enforces — the two lists drift apart silently otherwise.
func Candidates(goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Vivaldi.app/Contents/MacOS/Vivaldi",
			"/Applications/Opera.app/Contents/MacOS/Opera",
			"/Applications/Arc.app/Contents/MacOS/Arc",
			"google-chrome",
			"chromium",
			"chromium-browser",
		}
	default:
		return []string{
			"google-chrome",
			"google-chrome-stable",
			"chromium",
			"chromium-browser",
			"chrome",
			"microsoft-edge",
			"microsoft-edge-stable",
			"brave-browser",
			"brave",
			"vivaldi",
			"vivaldi-stable",
			"opera",
		}
	}
}

func chromeCandidates() []string {
	return Candidates(runtime.GOOS)
}

func DefaultProfileDir(home string) string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return filepath.Join(".", ".brw", "chrome-profile")
	}
	return filepath.Join(home, ".brw", "chrome-profile")
}
