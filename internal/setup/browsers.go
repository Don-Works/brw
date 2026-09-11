package setup

import (
	"fmt"
	"sort"
	"strings"
)

// Browser is one Chromium-family browser a profile can bind to. Everything that
// varies between them is data: the daemon drives them all over the same CDP and
// the same extension, and a profile directory has the same layout inside every
// one of their user data directories.
//
// A browser absent from this table is still reachable — `brwctl setup --browser
// <name> --user-data-dir <path>` binds to any Chromium fork without a code
// change — but a listed one needs no paths from the operator.
type Browser struct {
	// Name is the --browser value and the stem of the generated profile and
	// workspace names.
	Name string
	// DisplayName is what the human sees in their Applications folder.
	DisplayName string
	// BundleID is the macOS preference domain whose App Nap is disabled. It is
	// a fallback: when the application is installed, setup reads the identifier
	// out of its Info.plist instead, so a wrong entry here cannot leave a stray
	// preference domain behind.
	BundleID string
	// AppPaths are macOS install locations, most likely first.
	AppPaths []string
	// Commands are the executable names to look for on PATH elsewhere.
	Commands []string
	// UserDataDirs maps GOOS to the unexpanded user data directory. A GOOS with
	// no entry means brw does not know where this browser keeps its profiles
	// there; the operator has to say. Paths stay unexpanded so a policy copied
	// to another machine or another user still resolves.
	UserDataDirs map[string]string
}

// browsers is ordered: detection walks it in this sequence and the first
// browser that has actually been run wins, so Chrome and Chromium keep the
// precedence they had before the others were listed.
var browsers = []Browser{
	{
		Name:        "chrome",
		DisplayName: "Google Chrome",
		BundleID:    "com.google.Chrome",
		AppPaths:    []string{"/Applications/Google Chrome.app"},
		Commands:    []string{"google-chrome", "google-chrome-stable"},
		UserDataDirs: map[string]string{
			"darwin":  "~/Library/Application Support/Google/Chrome",
			"linux":   "~/.config/google-chrome",
			"windows": "${LOCALAPPDATA}/Google/Chrome/User Data",
		},
	},
	{
		Name:        "chromium",
		DisplayName: "Chromium",
		BundleID:    "org.chromium.Chromium",
		AppPaths:    []string{"/Applications/Chromium.app"},
		Commands:    []string{"chromium", "chromium-browser"},
		UserDataDirs: map[string]string{
			"darwin":  "~/Library/Application Support/Chromium",
			"linux":   "~/.config/chromium",
			"windows": "${LOCALAPPDATA}/Chromium/User Data",
		},
	},
	{
		Name:        "edge",
		DisplayName: "Microsoft Edge",
		BundleID:    "com.microsoft.edgemac",
		AppPaths:    []string{"/Applications/Microsoft Edge.app"},
		Commands:    []string{"microsoft-edge", "microsoft-edge-stable"},
		UserDataDirs: map[string]string{
			"darwin":  "~/Library/Application Support/Microsoft Edge",
			"linux":   "~/.config/microsoft-edge",
			"windows": "${LOCALAPPDATA}/Microsoft/Edge/User Data",
		},
	},
	{
		Name:        "brave",
		DisplayName: "Brave Browser",
		BundleID:    "com.brave.Browser",
		AppPaths:    []string{"/Applications/Brave Browser.app"},
		Commands:    []string{"brave-browser", "brave"},
		UserDataDirs: map[string]string{
			"darwin":  "~/Library/Application Support/BraveSoftware/Brave-Browser",
			"linux":   "~/.config/BraveSoftware/Brave-Browser",
			"windows": "${LOCALAPPDATA}/BraveSoftware/Brave-Browser/User Data",
		},
	},
	{
		Name:        "vivaldi",
		DisplayName: "Vivaldi",
		BundleID:    "com.vivaldi.Vivaldi",
		AppPaths:    []string{"/Applications/Vivaldi.app"},
		Commands:    []string{"vivaldi", "vivaldi-stable"},
		UserDataDirs: map[string]string{
			"darwin":  "~/Library/Application Support/Vivaldi",
			"linux":   "~/.config/vivaldi",
			"windows": "${LOCALAPPDATA}/Vivaldi/User Data",
		},
	},
	{
		Name:        "opera",
		DisplayName: "Opera",
		BundleID:    "com.operasoftware.Opera",
		AppPaths:    []string{"/Applications/Opera.app"},
		Commands:    []string{"opera"},
		UserDataDirs: map[string]string{
			"darwin": "~/Library/Application Support/com.operasoftware.Opera",
			"linux":  "~/.config/opera",
			// Opera keeps its profile in the roaming directory, unlike the rest.
			"windows": "${APPDATA}/Opera Software/Opera Stable",
		},
	},
	{
		Name:        "arc",
		DisplayName: "Arc",
		BundleID:    "company.thebrowser.Browser",
		AppPaths:    []string{"/Applications/Arc.app"},
		UserDataDirs: map[string]string{
			"darwin": "~/Library/Application Support/Arc/User Data",
			// Arc on Windows ships as an MSIX package whose per-user data path
			// is not a stable literal, so it is left for --user-data-dir.
		},
	},
}

// Browsers returns the table in detection order.
func Browsers() []Browser {
	out := make([]Browser, len(browsers))
	copy(out, browsers)
	return out
}

// LookupBrowser finds a browser by its --browser name.
func LookupBrowser(name string) (Browser, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, b := range browsers {
		if b.Name == name {
			return b, true
		}
	}
	return Browser{}, false
}

// BrowserNames lists every --browser value, for an error message that tells the
// operator what to type instead.
func BrowserNames() []string {
	names := make([]string, 0, len(browsers))
	for _, b := range browsers {
		names = append(names, b.Name)
	}
	sort.Strings(names)
	return names
}

// BrowserUserDataDir is where the named browser keeps its user data directory
// on the named OS, unexpanded. An empty return means the table has no entry for
// that pair and the operator has to supply one.
func BrowserUserDataDir(goos, browser string) string {
	b, ok := LookupBrowser(browser)
	if !ok {
		return ""
	}
	return b.UserDataDirs[goos]
}

// BrowserBundleIDs are the macOS preference domains whose App Nap must be
// disabled so a backgrounded window keeps servicing the bridge. Only the chosen
// browser is listed: writing a default for a browser the user does not run
// leaves a stray preference domain behind.
func BrowserBundleIDs(browser string) []string {
	b, ok := LookupBrowser(browser)
	if !ok || b.BundleID == "" {
		return nil
	}
	return []string{b.BundleID}
}

// BrowserDisplayName is the name a human sees in their Applications folder.
// An unlisted browser is named as the operator typed it.
func BrowserDisplayName(browser string) string {
	if b, ok := LookupBrowser(browser); ok {
		return b.DisplayName
	}
	return browser
}

// UnknownBrowserError explains what the operator can type, including the escape
// hatch for a fork brw has never heard of.
func UnknownBrowserError(browser string) error {
	return fmt.Errorf("unknown browser %q: choose one of %s, or name any Chromium build with --browser %s --user-data-dir <path>",
		browser, strings.Join(BrowserNames(), ", "), browser)
}
