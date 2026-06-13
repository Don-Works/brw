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

func chromeCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
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
		}
	}
}

func DefaultProfileDir(home string) string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return filepath.Join(".", ".agent-browser", "chrome-profile")
	}
	return filepath.Join(home, ".agent-browser", "chrome-profile")
}
