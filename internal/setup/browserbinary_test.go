package setup

import "testing"

func TestParseBrowserVersion(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{name: "chrome", out: "Google Chrome 141.0.7390.55 ", want: "141.0.7390.55"},
		{name: "chromium", out: "Chromium 140.0.7339.207", want: "140.0.7339.207"},
		{name: "brave prints two numbers", out: "Brave Browser 1.85.128", want: "1.85.128"},
		{name: "edge dev channel", out: "Microsoft Edge 142.0.3595.6 dev", want: "142.0.3595.6"},
		{name: "no version at all", out: "not a browser", want: ""},
		{name: "empty", out: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseBrowserVersion(tc.out); got != tc.want {
				t.Fatalf("ParseBrowserVersion(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

// TestBrowserExecutableFallsBackToPath covers the non-macOS lane, where a
// browser is only ever found through PATH.
func TestBrowserExecutableFallsBackToPath(t *testing.T) {
	chrome, ok := LookupBrowser(BrowserChrome)
	if !ok {
		t.Fatal("chrome is missing from the browser table")
	}
	installed := map[string]string{"google-chrome-stable": "/usr/bin/google-chrome-stable"}
	lookPath := func(name string) (string, bool) {
		path, ok := installed[name]
		return path, ok
	}
	if got := BrowserExecutable("linux", chrome, lookPath); got != "/usr/bin/google-chrome-stable" {
		t.Fatalf("BrowserExecutable = %q", got)
	}
	if got := BrowserExecutable("linux", chrome, func(string) (string, bool) { return "", false }); got != "" {
		t.Fatalf("an uninstalled browser resolved to %q", got)
	}
	if got := BrowserExecutable("linux", chrome, nil); got != "" {
		t.Fatalf("a nil lookup resolved to %q", got)
	}
}
