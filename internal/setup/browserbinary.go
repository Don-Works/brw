package setup

import (
	"os"
	"path/filepath"
	"strings"
)

// BrowserExecutable is the launchable binary for a browser on this machine, or
// "" when none of its install locations hold one. macOS bundles are searched
// before PATH because a Chromium fork installed as an .app puts nothing on PATH
// at all, so a PATH-first probe reports an installed browser as missing.
func BrowserExecutable(goos string, browser Browser, lookPath func(string) (string, bool)) string {
	if goos == "darwin" {
		for _, app := range browser.AppPaths {
			if exe := appBundleExecutable(app); exe != "" {
				return exe
			}
		}
	}
	if lookPath == nil {
		return ""
	}
	for _, command := range browser.Commands {
		if path, ok := lookPath(command); ok {
			return path
		}
	}
	return ""
}

// appBundleExecutable reads Contents/MacOS rather than deriving the binary name
// from the bundle name. The two differ per fork — Brave's bundle holds "Brave
// Browser", Edge's holds "Microsoft Edge" — and a name that has to be guessed
// is a name that goes stale when a vendor renames its binary.
func appBundleExecutable(app string) string {
	dir := filepath.Join(app, "Contents", "MacOS")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return path
	}
	return ""
}

// ParseBrowserVersion pulls the version out of `<browser> --version`, which
// prints the product name first ("Google Chrome 141.0.7390.55"). An empty
// return means the output had no version-shaped field, which is what a wrapper
// script or a localised build can produce.
func ParseBrowserVersion(out string) string {
	for _, field := range strings.Fields(strings.TrimSpace(out)) {
		if field[0] < '0' || field[0] > '9' {
			continue
		}
		if !strings.Contains(field, ".") {
			continue
		}
		return field
	}
	return ""
}
