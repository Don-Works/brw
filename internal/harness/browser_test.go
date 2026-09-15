package harness

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// TestHarnessBrowserReachesNothingButTheLoopbackLiteral drives the real harness
// browser at the same bytes under three host names and requires two of them to
// fail.
//
// The claim "no network beyond the fixture origin" was previously carried by
// Chrome's own --disable-* networking flags, which Chrome does not honour for
// every subsystem. The loopback-NAME row is what discriminates: localhost
// resolves without a network, so a run that loads it is a run whose resolver
// rule is absent or misspelled, while the fixture origin's literal address
// keeps working. The off-box row says the same thing about a public name.
func TestHarnessBrowserReachesNothingButTheLoopbackLiteral(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real browser")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	fixtures, err := ServeFixtures(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("serve fixtures: %v", err)
	}
	defer fixtures.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(fixtures.BaseURL(), "http://"))
	if err != nil {
		t.Fatalf("fixture base url %q: %v", fixtures.BaseURL(), err)
	}

	rig, err := LaunchBrowser(ctx, BrowserOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("launch browser: %v", err)
	}
	defer rig.Close()

	const fixtureTitle = "Readable Content Fixture"
	cases := []struct {
		name      string
		host      string
		wantLoad  bool
		wantWhyOK string
	}{
		{name: "loopback literal", host: "127.0.0.1", wantLoad: true,
			wantWhyOK: "the fixture origin is the one address the harness is allowed to reach"},
		{name: "loopback name", host: "localhost",
			wantWhyOK: "localhost resolves with no network, so loading it means no resolver rule is in force"},
		{name: "off-box name", host: "cdn.example.test",
			wantWhyOK: "a public name must not resolve at all"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			url := fmt.Sprintf("http://%s/content.html", net.JoinHostPort(testCase.host, port))
			opened, err := rig.Manager.Open(ctx, url)
			if err != nil {
				if testCase.wantLoad {
					t.Fatalf("open %s: %v", url, err)
				}
				return
			}
			tabID := opened.Tab.ID
			defer func() { _ = rig.Manager.CloseTab(ctx, tabID) }()

			// An evaluate that fails is also "the fixture did not load": either
			// way its title was never read out of the page.
			title, err := rig.Manager.Evaluate(browser.WithTabID(ctx, tabID), "String(document.title)")
			if err != nil {
				if testCase.wantLoad {
					t.Fatalf("read the title of %s: %v", url, err)
				}
				return
			}
			loaded := fmt.Sprint(title) == fixtureTitle
			if loaded != testCase.wantLoad {
				t.Fatalf("%s reported title %q (loaded=%v), want loaded=%v: %s",
					url, title, loaded, testCase.wantLoad, testCase.wantWhyOK)
			}
		})
	}
}

// TestChromeArgsCarryTheResolverRule guards the spelling. The rule above is a
// single string Chrome parses itself and silently ignores when malformed, and
// the end-to-end test is skipped in short mode, so the flag's presence and its
// exclusion are also asserted where no browser is needed.
func TestChromeArgsCarryTheResolverRule(t *testing.T) {
	var rule string
	for _, arg := range chromeArgs() {
		if strings.HasPrefix(arg, "--host-resolver-rules=") {
			rule = strings.TrimPrefix(arg, "--host-resolver-rules=")
		}
	}
	if rule == "" {
		t.Fatal("no --host-resolver-rules flag; nothing stops the browser resolving a public name")
	}
	if !strings.Contains(rule, "MAP * ~NOTFOUND") {
		t.Errorf("rule %q does not fail every lookup by default", rule)
	}
	if !strings.Contains(rule, "EXCLUDE 127.0.0.1") {
		t.Errorf("rule %q does not exempt the fixture origin's address", rule)
	}
	if strings.Contains(rule, "EXCLUDE localhost") {
		t.Errorf("rule %q exempts localhost, which is the name the end-to-end test uses to prove the rule is enforced", rule)
	}
}
