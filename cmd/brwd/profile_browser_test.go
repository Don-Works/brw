package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProfileBrowserExecutableLeavesAnUnknownKindToTheLauncher(t *testing.T) {
	for _, kind := range []string{"", "netscape"} {
		if got := profileBrowserExecutable(kind); got != "" {
			t.Fatalf("kind %q resolved to %q; an unknown kind must fall through to discovery", kind, got)
		}
	}
}

func TestAHeadlessProfileStartsAsAProxy(t *testing.T) {
	home := t.TempDir()
	health := fmt.Sprintf(`{"ok":true,"version":"test","identity":{"workspace":"brw-chromium-headless","profile":"chromium-headless","user_data_dir":%q,"mode":"direct","transport":"direct-cdp","headless":true}}`,
		filepath.Join(home, ".brw", "chromium-headless"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(health))
	}))
	defer upstream.Close()

	policy := filepath.Join(home, "browser-profiles.json")
	body := `{"workspace_bindings":[{"workspace":"brw-chromium-headless","default_profile":"chromium-headless","allowed_profiles":["chromium-headless"]}],
"profiles":[{"name":"chromium-headless","kind":"chromium","user_data_dir":"~/.brw/chromium-headless","direct_cdp_allowed":true,"headless":true}]}`
	if err := os.WriteFile(policy, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := runBrwdUntilItStops(t,
		[]string{"--mcp", "--http", "off", "--upstream-http", upstream.URL, "--workspace", "brw-chromium-headless", "--profile-policy", policy},
		startupEnvironment(home), 20*time.Second)
	if strings.Contains(out, "cannot be combined") || code != 0 {
		t.Fatalf("a headless profile's proxy refused to start (exit %d):\n%s", code, out)
	}
}
