package setup

import (
	"path"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/cdp"
)

func TestBrowserTableIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, b := range Browsers() {
		t.Run(b.Name, func(t *testing.T) {
			if b.Name == "" || b.DisplayName == "" {
				t.Fatalf("browser has an empty name or display name: %+v", b)
			}
			if b.Name != strings.ToLower(b.Name) {
				t.Fatalf("name %q must be lower case: it is what the operator types", b.Name)
			}
			if seen[b.Name] {
				t.Fatalf("duplicate browser name %q", b.Name)
			}
			seen[b.Name] = true
			if len(b.UserDataDirs) == 0 {
				t.Fatal("a browser with no user data directory anywhere cannot be bound to")
			}
			for goos, path := range b.UserDataDirs {

				if !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, "${") {
					t.Fatalf("%s path %q must stay unexpanded", goos, path)
				}
			}
			if b.UserDataDirs["darwin"] != "" && len(b.AppPaths) == 0 {
				t.Fatal("a browser with a macOS profile path needs an app path, or detection cannot see it")
			}
			if b.UserDataDirs["linux"] != "" && len(b.Commands) == 0 {
				t.Fatal("a browser with a Linux profile path needs a command, or detection cannot see it")
			}
		})
	}
}

func TestBrowserTableOrder(t *testing.T) {
	all := Browsers()
	if len(all) < 2 {
		t.Fatal("the table lost its entries")
	}
	if all[0].Name != BrowserChrome || all[1].Name != BrowserChromium {
		t.Fatalf("table starts %q, %q; want %q, %q", all[0].Name, all[1].Name, BrowserChrome, BrowserChromium)
	}
}

func TestLookupBrowser(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		found bool
	}{
		{name: "exact", input: "brave", want: "brave", found: true},
		{name: "upper case", input: "Brave", want: "brave", found: true},
		{name: "padded", input: "  edge  ", want: "edge", found: true},
		{name: "unknown fork", input: "comet", found: false},
		{name: "empty", input: "", found: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, found := LookupBrowser(tc.input)
			if found != tc.found {
				t.Fatalf("LookupBrowser(%q) found = %v, want %v", tc.input, found, tc.found)
			}
			if found && got.Name != tc.want {
				t.Fatalf("LookupBrowser(%q) = %q, want %q", tc.input, got.Name, tc.want)
			}
		})
	}
}

func TestBrowserUserDataDir(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		browser string
		want    string
	}{
		{name: "chrome darwin", goos: "darwin", browser: "chrome", want: "~/Library/Application Support/Google/Chrome"},
		{name: "chrome linux", goos: "linux", browser: "chrome", want: "~/.config/google-chrome"},
		{name: "chrome windows", goos: "windows", browser: "chrome", want: "${LOCALAPPDATA}/Google/Chrome/User Data"},
		{name: "chromium darwin", goos: "darwin", browser: "chromium", want: "~/Library/Application Support/Chromium"},
		{name: "chromium linux", goos: "linux", browser: "chromium", want: "~/.config/chromium"},
		{name: "chromium windows", goos: "windows", browser: "chromium", want: "${LOCALAPPDATA}/Chromium/User Data"},
		{name: "edge windows", goos: "windows", browser: "edge", want: "${LOCALAPPDATA}/Microsoft/Edge/User Data"},
		{name: "brave darwin", goos: "darwin", browser: "brave", want: "~/Library/Application Support/BraveSoftware/Brave-Browser"},

		{name: "opera windows", goos: "windows", browser: "opera", want: "${APPDATA}/Opera Software/Opera Stable"},

		{name: "arc windows is unknown", goos: "windows", browser: "arc", want: ""},
		{name: "unlisted browser", goos: "darwin", browser: "comet", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := BrowserUserDataDir(tc.goos, tc.browser); got != tc.want {
				t.Fatalf("BrowserUserDataDir(%q, %q) = %q, want %q", tc.goos, tc.browser, got, tc.want)
			}
		})
	}
}

func TestBrowserBundleIDsAndNames(t *testing.T) {
	if got := BrowserBundleIDs("vivaldi"); len(got) != 1 || got[0] != "com.vivaldi.Vivaldi" {
		t.Fatalf("BrowserBundleIDs(vivaldi) = %v", got)
	}
	if got := BrowserBundleIDs("comet"); got != nil {
		t.Fatalf("BrowserBundleIDs(comet) = %v, want nil", got)
	}

	if got := BrowserDisplayName("comet"); got != "comet" {
		t.Fatalf("BrowserDisplayName(comet) = %q", got)
	}
	err := UnknownBrowserError("comet")
	for _, want := range []string{"comet", "chrome", "--user-data-dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("UnknownBrowserError does not mention %q: %v", want, err)
		}
	}
}

func TestPolicyRequestPrefersTheOperatorsUserDataDir(t *testing.T) {
	req := PolicyRequest{Browser: "chrome", GOOS: "darwin"}
	if got := req.userDataDir(); got != "~/Library/Application Support/Google/Chrome" {
		t.Fatalf("table lookup = %q", got)
	}
	req.UserDataDir = "~/somewhere/else"
	if got := req.userDataDir(); got != "~/somewhere/else" {
		t.Fatalf("override = %q, want the operator's path", got)
	}
}

func TestEveryTableBrowserIsDiscoverable(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		candidates := strings.Join(cdp.Candidates(goos), "\n")
		for _, b := range Browsers() {
			if b.UserDataDirs[goos] == "" {
				continue
			}
			t.Run(goos+"/"+b.Name, func(t *testing.T) {
				if goos == "darwin" {

					for _, app := range b.AppPaths {
						want := strings.TrimSuffix(app, ".app")
						want = app + "/Contents/MacOS/" + path.Base(want)
						if strings.Contains(candidates, want) {
							return
						}
					}
					t.Fatalf("no macOS candidate for %s; cdp.Candidates needs its bundle executable", b.Name)
				}
				for _, cmd := range b.Commands {
					if strings.Contains(candidates, cmd) {
						return
					}
				}
				t.Fatalf("no %s candidate for %s; cdp.Candidates needs one of %v", goos, b.Name, b.Commands)
			})
		}
	}
}
